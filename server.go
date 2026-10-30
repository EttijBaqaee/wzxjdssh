package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

type counters struct {
	accepted     atomic.Uint64
	rejected     atomic.Uint64
	acceptErrors atomic.Uint64
	closed       atomic.Uint64
	evicted      atomic.Uint64
	bytesSent    atomic.Uint64
	authAttempts atomic.Uint64
	active       atomic.Int64
	pending      atomic.Int64
	closedNanos  atomic.Int64
}

type rateBucket struct {
	tokens  float64
	updated time.Time
}

type Server struct {
	mu            sync.Mutex
	reloadMu      sync.Mutex
	policyMu      sync.Mutex // Serializes persisted parallel-policy and cooldown decisions.
	logMu         sync.Mutex
	cfg           atomic.Pointer[Config]
	listener      net.Listener
	clients       map[*clientConn]struct{}
	ipCount       map[string]int
	buckets       map[string]rateBucket
	keys          []ssh.Signer
	changed       chan struct{}
	stopping      bool
	wg            sync.WaitGroup
	stats         counters
	logWriter     io.Writer
	lastPrune     time.Time
	store         *Store
	storageErrors atomic.Uint64
	authDelays    authDelayLearning
	singleAuth    singleAuthRetries
}

func NewServer(c Config, logWriter io.Writer) (*Server, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	keys, err := loadHostKeys(c.HostKeyDirectory)
	if err != nil {
		return nil, err
	}
	if logWriter == nil {
		logWriter = io.Discard
	}
	s := &Server{clients: map[*clientConn]struct{}{}, ipCount: map[string]int{}, buckets: map[string]rateBucket{}, keys: keys, changed: make(chan struct{}), logWriter: logWriter, lastPrune: time.Now()}
	s.cfg.Store(&c)
	return s, nil
}

func (s *Server) log(level int, event string, fields map[string]any) {
	if s.cfg.Load().LogLevel < level {
		return
	}
	if fields == nil {
		fields = map[string]any{}
	}
	fields["time"] = time.Now().UTC().Format(time.RFC3339Nano)
	fields["event"] = event
	s.logMu.Lock()
	defer s.logMu.Unlock()
	_ = json.NewEncoder(s.logWriter).Encode(fields)
}

func listen(c Config) (net.Listener, error) {
	network := "tcp"
	if c.BindFamily == 4 {
		network = "tcp4"
	} else if c.BindFamily == 6 {
		network = "tcp6"
	}
	return net.Listen(network, net.JoinHostPort(c.BindAddress, strconv.Itoa(c.Port)))
}

func (s *Server) Start() error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return errors.New("server stopped")
	}
	if s.listener != nil {
		return errors.New("already started")
	}
	l, err := listen(*s.cfg.Load())
	if err != nil {
		return err
	}
	s.listener = l
	s.wg.Add(1)
	go s.acceptLoop(l)
	s.log(1, "listening", map[string]any{"address": l.Addr().String(), "mode": s.cfg.Load().Mode, "version": version})
	return nil
}

func bindingChanged(a, b Config) bool {
	return a.Port != b.Port || a.BindFamily != b.BindFamily || a.BindAddress != b.BindAddress
}

func (s *Server) Reload(next Config) error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	if err := next.Validate(); err != nil {
		return err
	}
	old := s.cfg.Load()
	if old.HostKeyDirectory != next.HostKeyDirectory {
		return errors.New("HostKeyDirectory is startup-only; restart required")
	}
	var replacement net.Listener
	var err error
	if bindingChanged(*old, next) {
		replacement, err = listen(next)
		if err != nil {
			return fmt.Errorf("new listener failed; old config retained: %w", err)
		}
	}
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		if replacement != nil {
			replacement.Close()
		}
		return errors.New("server stopped")
	}
	previous := s.listener
	s.cfg.Store(&next)
	close(s.changed)
	s.changed = make(chan struct{})
	if replacement != nil {
		s.listener = replacement
		s.wg.Add(1)
		go s.acceptLoop(replacement)
	}
	s.mu.Unlock()
	if replacement != nil && previous != nil {
		previous.Close()
	}
	s.log(1, "reloaded", map[string]any{"port": next.Port, "mode": next.Mode, "delay_ms": next.Delay.Milliseconds()})
	return nil
}

// Each waiter resets its own timer after reload. No shared FIFO can block it.
func (s *Server) pause(ctx context.Context) error {
	started := time.Now()
	var sampledConfig *Config
	var delay time.Duration
	for {
		s.mu.Lock()
		changed := s.changed
		cfg := s.cfg.Load()
		stopped := s.stopping
		s.mu.Unlock()
		if stopped {
			return context.Canceled
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if cfg != sampledConfig {
			var err error
			delay, err = jitterDelay(cfg.Delay, cfg.DelayJitter)
			if err != nil {
				return err
			}
			sampledConfig = cfg
		}
		remaining := delay - time.Since(started)
		if remaining <= 0 {
			return nil
		}
		timer := time.NewTimer(remaining)
		select {
		case <-timer.C:
			return nil
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-changed:
			timer.Stop()
		}
	}
}

func (s *Server) admit(ip string, c Config) string {
	// Caller holds s.mu. Bound memory independently of source-address cardinality.
	if s.stopping {
		return "shutdown"
	}
	if c.Mode == "ssh" && s.stats.pending.Load() >= int64(c.MaxPendingHandshakes) {
		return "handshake_limit"
	}
	if _, reason := s.capacityVictimsLocked(ip, c); reason != "" {
		return reason
	}
	if c.ConnectionsPerMinute > 0 {
		now := time.Now()
		if now.Sub(s.lastPrune) > time.Minute || len(s.buckets) >= 65536 {
			for key, b := range s.buckets {
				if now.Sub(b.updated) > 2*time.Minute {
					delete(s.buckets, key)
				}
			}
			s.lastPrune = now
		}
		b, ok := s.buckets[ip]
		if !ok {
			if len(s.buckets) >= 65536 {
				return "rate_table_full"
			}
			b = rateBucket{float64(c.ConnectionsPerMinute), now}
		}
		b.tokens += now.Sub(b.updated).Minutes() * float64(c.ConnectionsPerMinute)
		if b.tokens > float64(c.ConnectionsPerMinute) {
			b.tokens = float64(c.ConnectionsPerMinute)
		}
		b.updated = now
		if b.tokens < 1 {
			s.buckets[ip] = b
			return "rate_limit"
		}
		b.tokens--
		s.buckets[ip] = b
	}
	return ""
}

func (s *Server) acceptLoop(l net.Listener) {
	defer s.wg.Done()
	backoff := time.Duration(0)
	for {
		raw, err := l.Accept()
		if err != nil {
			s.mu.Lock()
			finished := s.stopping || s.listener != l
			s.mu.Unlock()
			if finished {
				return
			}
			s.stats.acceptErrors.Add(1)
			s.log(1, "accept_error", map[string]any{"error": err.Error()})
			if backoff == 0 {
				backoff = 10 * time.Millisecond
			} else {
				backoff *= 2
			}
			if backoff > time.Second {
				backoff = time.Second
			}
			time.Sleep(backoff)
			continue
		}
		backoff = 0
		ip, _, _ := net.SplitHostPort(raw.RemoteAddr().String())
		if parsed := net.ParseIP(ip); parsed != nil {
			ip = parsed.String()
		}
		var probeUntil int64
		if s.store != nil && s.cfg.Load().Mode == "ssh" {
			probeUntil, err = s.store.ProbeCooldown(ip, time.Now())
			if err != nil {
				s.storageError(err)
				raw.Close()
				continue
			}
		}
		s.mu.Lock()
		cfg := *s.cfg.Load()
		reason := s.admit(ip, cfg)
		var victims []*clientConn
		if reason == "" && probeUntil > 0 {
			if len(s.clients) >= cfg.MaxClients {
				reason = "max_clients"
			} else if cfg.MaxClientsPerIP > 0 && s.ipCount[ip] >= cfg.MaxClientsPerIP {
				reason = "per_ip_limit"
			}
		}
		if reason == "" && probeUntil == 0 {
			victims, reason = s.capacityVictimsLocked(ip, cfg)
		}
		if reason != "" {
			s.stats.rejected.Add(1)
			s.mu.Unlock()
			raw.Close()
			s.log(2, "rejected", map[string]any{"peer": raw.RemoteAddr().String(), "reason": reason})
			if s.store != nil {
				s.storageError(s.store.Reject(raw.RemoteAddr().String(), ip, cfg.Mode, reason))
			}
			continue
		}
		for _, victim := range victims {
			victim.capacityEvicted.Store(true)
			s.stats.evicted.Add(1)
			victim.cancel()
			victim.Conn.Close()
		}
		if len(victims) > 0 {
			// Wait for actual resource release without holding the server lock.
			// This accept loop is the sole admission writer, so no slot is stolen.
			s.mu.Unlock()
			for _, victim := range victims {
				<-victim.done
			}
			s.mu.Lock()
			if s.stopping {
				s.mu.Unlock()
				raw.Close()
				return
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		c := &clientConn{Conn: raw, server: s, cfg: cfg, ip: ip, started: time.Now(), ctx: ctx, cancel: cancel, done: make(chan struct{}), probeUntil: probeUntil}
		if probeUntil == 0 {
			c.startLifetime()
		}
		if cfg.Mode == "ssh" && probeUntil == 0 {
			c.pending.Store(true)
			s.stats.pending.Add(1)
		}
		s.clients[c] = struct{}{}
		if probeUntil == 0 {
			s.detectParallelLocked(c)
		}
		s.ipCount[ip]++
		s.stats.active.Add(1)
		s.stats.accepted.Add(1)
		s.wg.Add(1)
		s.mu.Unlock()
		if probeUntil == 0 {
			if err := s.applyParallelPolicy(c); err != nil {
				s.storageError(err)
				c.cancel()
			}
		}
		s.log(1, "accepted", map[string]any{"peer": raw.RemoteAddr().String(), "mode": cfg.Mode, "active": s.stats.active.Load()})
		go s.handle(c)
	}
}

type clientConn struct {
	net.Conn
	server              *Server
	cfg                 Config
	ip                  string
	started             time.Time
	ctx                 context.Context
	cancel              context.CancelFunc
	pending             atomic.Bool
	authStage           atomic.Bool
	bytesSent           atomic.Uint64
	readDeadlineMu      sync.Mutex
	writeMu             sync.Mutex
	delaying            bool
	recordID            int64
	bytesReceived       atomic.Uint64
	attemptCount        int
	firstAuthMethod     string
	rejectedAuth        bool
	peerAuthEnded       bool
	sessionTrace        sessionTraceState
	readTimedOut        atomic.Bool
	peerClosed          atomic.Bool
	delayStarted        time.Time     // Protected by server.mu.
	delayed             time.Duration // Completed delay waits; protected by server.mu.
	peerDuringDelay     atomic.Bool
	interruptedAuthWait atomic.Int64
	simulated           atomic.Bool
	clientVersion       string
	markedUntil         int64
	authDelayTried      atomic.Bool
	authLearned         atomic.Bool
	probeUntil          int64
	forceSlow           atomic.Bool
	longLived           atomic.Bool
	capacityEvicted     atomic.Bool
	lifetimeExpired     atomic.Bool
	lifetimeMu          sync.Mutex
	lifetimeTimer       *time.Timer
	done                chan struct{}
}

func (c *clientConn) releasePending() {
	if c.pending.Swap(false) {
		c.server.stats.pending.Add(-1)
	}
}

func (c *clientConn) Read(p []byte) (int, error) {
	c.readDeadlineMu.Lock()
	if c.authStage.Load() && !c.delaying && !c.simulated.Load() {
		_ = c.Conn.SetReadDeadline(time.Now().Add(c.cfg.IdleTimeout))
	}
	c.readDeadlineMu.Unlock()
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.bytesReceived.Add(uint64(n))
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		c.readTimedOut.Store(true)
	} else if err != nil && !errors.Is(err, net.ErrClosed) && c.ctx != nil && c.ctx.Err() == nil && c.authStage.Load() {
		c.readDeadlineMu.Lock()
		if c.delaying {
			c.peerDuringDelay.Store(true)
		}
		c.readDeadlineMu.Unlock()
		c.peerClosed.Store(true)
		c.cancel()
	}
	return n, err
}

// net.Conn may return a short write. Keep the remaining bytes, including CRLF.
func writeAll(w io.Writer, p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n, err := w.Write(p)
		if n < 0 || n > len(p) {
			return total, errors.New("invalid write count")
		}
		total += n
		p = p[n:]
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

func (c *clientConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.simulated.Load() && c.forceSlow.Load() {
		if err := c.pause(); err != nil {
			return 0, err
		}
	} else if c.simulated.Load() {
		if err := c.networkPause(); err != nil {
			return 0, err
		}
	}
	if err := c.Conn.SetWriteDeadline(time.Now().Add(c.cfg.WriteTimeout)); err != nil {
		return 0, err
	}
	n, err := writeAll(c.Conn, p)
	c.bytesSent.Add(uint64(n))
	c.server.stats.bytesSent.Add(uint64(n))
	return n, err
}

func (s *Server) handle(c *clientConn) {
	defer close(c.done)
	defer s.wg.Done()
	defer func() {
		c.stopLifetime()
		status, reason := "closed", "peer_closed"
		if c.readTimedOut.Load() {
			status = "timeout"
			reason = "handshake_timeout"
			if c.authStage.Load() {
				reason = "idle_timeout"
			}
		}
		if c.lifetimeExpired.Load() {
			status = "timeout"
			reason = "max_connection_time"
		}
		if c.peerClosed.Load() {
			status = "closed"
			reason = "peer_closed"
		}
		s.mu.Lock()
		stopping := s.stopping
		s.mu.Unlock()
		if stopping {
			status = "interrupted"
			reason = "shutdown"
		} else if c.cfg.Mode == "ssh" && c.attemptCount > 0 && !c.simulated.Load() {
			if status == "closed" {
				status = "auth_rejected"
				reason = "authentication_failed"
			}
		} else if c.cfg.Mode == "ssh" && !c.authStage.Load() {
			if status == "closed" {
				status = "handshake_failed"
				reason = "handshake_incomplete"
			}
		}
		if !stopping && !c.readTimedOut.Load() && !c.lifetimeExpired.Load() && (c.peerClosed.Load() || c.peerAuthEnded) && (c.peerDuringDelay.Load() || c.simulated.Load()) {
			c.learnAuthDelay(false)
		}
		if !stopping && c.simulated.Load() {
			if status == "closed" {
				status = "simulated_closed"
			}
		} else if !stopping && !c.capacityEvicted.Load() && c.peerDuringDelay.Load() && !c.forceSlow.Load() {
			s.policyMu.Lock()
			if !c.forceSlow.Load() {
				if c.markedUntil != 0 {
					status, reason = "cooldown_auth_interrupted", "peer_closed_during_cooldown_auth_delay"
				} else {
					status, reason = "auth_interrupted", "peer_closed_during_auth_delay"
				}
				if c.markedUntil == 0 && s.store != nil && c.recordID != 0 && c.clientVersion != "" {
					now := time.Now()
					until, err := s.store.MarkInterruptedClient(c.recordID, c.ip, c.clientVersion, now)
					s.storageError(err)
					if err == nil {
						s.authDelays.seed(authDelayKey{c.ip, c.clientVersion}, until, now, time.Duration(c.interruptedAuthWait.Load()), *s.cfg.Load())
					}
				}
			}
			s.policyMu.Unlock()
		}
		if !stopping && c.capacityEvicted.Load() {
			status, reason = "capacity_evicted", "connection_capacity"
		}
		if c.probeUntil > 0 {
			status, reason = "probe_blocked", "probe_cooldown"
		}
		c.cancel()
		c.Conn.Close()
		c.releasePending()
		s.mu.Lock()
		delayed := c.delayed
		if !c.delayStarted.IsZero() {
			delayed += time.Since(c.delayStarted)
		}
		s.mu.Unlock()
		duration := time.Since(c.started)
		if s.store != nil && c.recordID != 0 {
			s.policyMu.Lock()
			if c.forceSlow.Load() {
				s.storageError(s.store.ForceParallelIP(c.ip, time.Now().Add(c.cfg.ParallelCooldown).UnixMilli()))
			}
			if c.clientVersion != "" {
				if s.singleAuth.observe(authDelayKey{c.ip, c.clientVersion}, c.singleAuthRetryEligible(status, reason, stopping), time.Now(), c.cfg) {
					_, err := s.store.markClientCooldown(c.recordID, c.ip, c.clientVersion, "single_auth_retry", time.Now(), c.cfg.SingleAuthRetryCooldown)
					s.storageError(err)
				}
			}
			s.storageError(s.store.finish(c.recordID, duration, status, reason, c.bytesSent.Load(), c.bytesReceived.Load(), delayed))
			if !stopping && c.probeUntil == 0 && c.cfg.Mode == "ssh" {
				_, err := s.store.ProbeResult(c.recordID, c.ip, reason == "handshake_incomplete", time.Now(), c.cfg)
				s.storageError(err)
			}
			s.policyMu.Unlock()
		}
		// Keep the live sample available until its final delay is persisted.
		s.mu.Lock()
		delete(s.clients, c)
		s.ipCount[c.ip]--
		if s.ipCount[c.ip] == 0 {
			delete(s.ipCount, c.ip)
		}
		s.stats.active.Add(-1)
		s.mu.Unlock()
		s.stats.closedNanos.Add(int64(duration))
		s.stats.closed.Add(1)
		s.log(1, "closed", map[string]any{"peer": c.RemoteAddr().String(), "duration_ms": duration.Milliseconds(), "bytes_sent": c.bytesSent.Load()})
	}()
	go func() { <-c.ctx.Done(); c.Conn.Close() }()
	if s.store != nil {
		id, err := s.store.Start(c.RemoteAddr().String(), c.ip, c.cfg.Mode, c.started)
		if err != nil {
			s.storageError(err)
			return
		}
		s.mu.Lock()
		c.recordID = id
		s.mu.Unlock()
		if c.forceSlow.Load() {
			if err := s.store.MarkParallelConnection(id); err != nil {
				s.storageError(err)
				return
			}
		}
		if c.probeUntil > 0 {
			s.storageError(s.store.MarkProbeConnection(id, c.probeUntil))
		}
	}
	if c.probeUntil > 0 {
		s.discardProbe(c)
		return
	}
	if c.cfg.Mode == "banner" {
		s.serveBanner(c)
		return
	}
	_ = c.Conn.SetReadDeadline(time.Now().Add(c.cfg.HandshakeTimeout))
	maxAuthTries := c.cfg.MaxAuthTries
	if maxAuthTries == 0 {
		maxAuthTries = -1
	}
	cfg := &ssh.ServerConfig{MaxAuthTries: maxAuthTries, ServerVersion: c.cfg.ServerVersion}
	for _, key := range s.keys {
		cfg.AddHostKey(key)
	}
	// Callback runs only after SSH transport and key exchange are ready.
	cfg.PreAuthConnCallback = func(meta ssh.ServerPreAuthConn) {
		c.retainSlowConnection()
		c.clientVersion = string(meta.ClientVersion())
		c.authStage.Store(true)
		c.releasePending()
		_ = c.Conn.SetReadDeadline(time.Now().Add(c.cfg.IdleTimeout))
		if s.store != nil {
			s.policyMu.Lock()
			defer s.policyMu.Unlock()
			_, err := s.store.ProbeResult(0, c.ip, false, time.Now(), c.cfg)
			s.storageError(err)
			if c.forceSlow.Load() {
				s.storageError(s.store.MarkParallelConnection(c.recordID))
			} else {
				until, err := s.store.ClientCooldown(c.ip, c.clientVersion, time.Now())
				if err != nil {
					s.storageError(err)
					c.cancel()
					return
				}
				c.markedUntil = until
				if until > 0 {
					if err = s.store.MarkReturningClient(c.recordID, until); err != nil {
						s.storageError(err)
						c.cancel()
						return
					}
				}
			}
			if err := s.store.Transport(c.recordID, string(meta.ClientVersion())); err != nil {
				s.storageError(err)
				c.cancel()
			}
		}
		s.log(2, "ssh_transport_ready", map[string]any{"peer": c.RemoteAddr().String(), "client_version": string(meta.ClientVersion())})
	}
	denied := errors.New("permission denied")
	delay := func(adaptive bool) error {
		started := time.Now()
		c.readDeadlineMu.Lock()
		c.delaying = true
		_ = c.Conn.SetReadDeadline(time.Time{})
		c.readDeadlineMu.Unlock()
		var err error
		if adaptive {
			err = c.cooldownAuthPause()
		} else {
			err = c.pause()
		}
		c.readDeadlineMu.Lock()
		c.delaying = false
		_ = c.Conn.SetReadDeadline(time.Now().Add(c.cfg.IdleTimeout))
		c.readDeadlineMu.Unlock()
		if err != nil {
			if c.peerDuringDelay.Load() {
				c.interruptedAuthWait.Store(int64(time.Since(started)))
			}
			return err
		}
		return denied
	}
	record := func(meta ssh.ConnMetadata, method string, credential []byte, key ssh.PublicKey) (int64, error) {
		c.attemptCount++
		if c.attemptCount == 1 {
			c.firstAuthMethod = method
		}
		if s.store == nil {
			return 0, nil
		}
		value, encoding, truncated := credentialBytes(credential)
		a := AuthRecord{ConnectionID: c.recordID, At: time.Now().UnixMilli(), Username: meta.User(), Method: method, Credential: value, Encoding: encoding, Truncated: truncated}
		if key != nil {
			a.PublicKey = string(ssh.MarshalAuthorizedKey(key))
			a.Fingerprint = ssh.FingerprintSHA256(key)
		}
		id, err := s.store.Attempt(a)
		if err != nil {
			s.storageError(err)
			c.cancel()
			return 0, errors.New("recording unavailable")
		}
		return id, nil
	}
	finishAttempt := func(id int64, err error) error {
		c.rejectedAuth = errors.Is(err, denied)
		if s.store != nil && id != 0 {
			result := "rejected"
			if !errors.Is(err, denied) {
				result = "interrupted"
			}
			s.storageError(s.store.AttemptResult(id, result))
		}
		return err
	}
	respond := func(id int64) error {
		if !c.forceSlow.Load() && c.markedUntil > time.Now().UnixMilli() {
			// A marked client still pays an adaptive, randomly perturbed delay.
			// Recheck policy after waiting: overlap or expiry may now require denial.
			if err := delay(true); !errors.Is(err, denied) {
				return finishAttempt(id, err)
			}
			if !c.forceSlow.Load() && c.markedUntil > time.Now().UnixMilli() {
				return nil
			}
			return finishAttempt(id, denied)
		}
		return finishAttempt(id, delay(false))
	}
	cfg.PasswordCallback = func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		id, err := record(meta, "password", password, nil)
		if err != nil {
			return nil, err
		}
		return nil, respond(id)
	}
	cfg.PublicKeyCallback = func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		id, err := record(meta, "publickey_probe", nil, key)
		if err != nil {
			return nil, err
		}
		return nil, respond(id)
	}
	cfg.KeyboardInteractiveCallback = func(meta ssh.ConnMetadata, challenge ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
		answers, err := challenge("", "", []string{"Password: "}, []bool{false})
		if err != nil {
			return nil, err
		}
		value, _ := json.Marshal(answers)
		id, err := record(meta, "keyboard-interactive", value, nil)
		if err != nil {
			return nil, err
		}
		return nil, respond(id)
	}
	cfg.AuthLogCallback = func(meta ssh.ConnMetadata, method string, err error) {
		if s.store != nil {
			if err := s.store.Username(c.recordID, meta.User()); err != nil {
				s.storageError(err)
				c.cancel()
			}
		}
		if method == "none" {
			return
		}
		s.stats.authAttempts.Add(1)
		s.log(1, "authentication", map[string]any{"peer": c.RemoteAddr().String(), "method": method, "accepted": err == nil})
	}
	conn, chans, requests, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		c.peerAuthEnded = isPeerAuthEnd(err)
		s.log(2, "ssh_closed", map[string]any{"peer": c.RemoteAddr().String(), "reason": err.Error()})
		return
	}
	// USERAUTH_SUCCESS is already on the wire. Subsequent packets simulate a
	// variable slow link; the virtual channel never starts a system process.
	c.readDeadlineMu.Lock()
	c.simulated.Store(true)
	_ = c.Conn.SetReadDeadline(time.Time{})
	c.readDeadlineMu.Unlock()
	c.sessionEvent("authenticated", "")
	if s.store != nil {
		if err := s.store.RecordSimulatedSession(c.recordID, c.markedUntil); err != nil {
			s.storageError(err)
			c.cancel()
		}
	}
	s.serveSimulatedNetwork(c, conn, chans, requests)
}

func randomLine(max int) ([]byte, error) {
	var seed [2]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return nil, err
	}
	n := 3 + int(binary.BigEndian.Uint16(seed[:]))%(max-2)
	line := make([]byte, n)
	if _, err := rand.Read(line[:n-2]); err != nil {
		return nil, err
	}
	for i := 0; i < n-2; i++ {
		line[i] = 32 + line[i]%95
	}
	line[n-2] = '\r'
	line[n-1] = '\n'
	if n >= 6 && string(line[:4]) == "SSH-" {
		line[0] = 'X'
	}
	return line, nil
}

func (s *Server) serveBanner(c *clientConn) {
	// Drain input so FIN/RST is observed promptly, even with a very long Delay.
	go func() {
		_, _ = io.Copy(io.Discard, c)
		if c.ctx.Err() == nil {
			c.peerClosed.Store(true)
		}
		c.cancel()
	}()
	for {
		if err := c.pause(); err != nil {
			return
		}
		line, err := randomLine(s.cfg.Load().MaxLineLength)
		if err != nil {
			return
		}
		if _, err = c.Write(line); err != nil {
			return
		}
	}
}

func (s *Server) Snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	nanos := s.stats.closedNanos.Load()
	for c := range s.clients {
		nanos += int64(time.Since(c.started))
	}
	cfg := s.cfg.Load()
	live := s.liveSnapshotLocked(time.Now())
	strategy := map[string]any{"mode": cfg.Mode, "delay_ms": cfg.Delay.Milliseconds(), "client_cooldown_ms": clientCooldown.Milliseconds(), "max_line_length": cfg.MaxLineLength, "max_auth_tries": cfg.MaxAuthTries, "handshake_timeout_ms": cfg.HandshakeTimeout.Milliseconds(), "idle_timeout_ms": cfg.IdleTimeout.Milliseconds(), "max_connection_time_ms": cfg.MaxConnectionTime.Milliseconds(), "max_clients": cfg.MaxClients, "max_clients_per_ip": cfg.MaxClientsPerIP, "connections_per_minute": cfg.ConnectionsPerMinute, "max_pending_handshakes": cfg.MaxPendingHandshakes}
	strategy["cooldown_return_mode"] = "network_jitter"
	strategy["slow_connection_lifetime"] = "unlimited"
	strategy["max_connection_time_ms"] = int64(0)
	strategy["pre_transport_max_time_ms"] = cfg.MaxConnectionTime.Milliseconds()
	strategy["capacity_policy"] = "oldest_slow_on_demand"
	strategy["parallel_window_ms"] = cfg.ParallelWindow.Milliseconds()
	strategy["parallel_cooldown_ms"] = cfg.ParallelCooldown.Milliseconds()
	low, high := jitterBounds(cfg.Delay, cfg.DelayJitter)
	strategy["delay_jitter_percent"] = cfg.DelayJitter
	strategy["delay_min_ms"] = low.Milliseconds()
	strategy["delay_max_ms"] = high.Milliseconds()
	strategy["cooldown_auth_initial_sample"] = "interrupted_wait_minus_step"
	strategy["probe_failure_threshold"] = cfg.ProbeFailureThreshold
	strategy["probe_cooldown_ms"] = cfg.ProbeCooldown.Milliseconds()
	strategy["single_auth_retry_threshold"] = cfg.SingleAuthRetryThreshold
	strategy["single_auth_retry_window_ms"] = cfg.SingleAuthRetryWindow.Milliseconds()
	strategy["single_auth_retry_cooldown_ms"] = cfg.SingleAuthRetryCooldown.Milliseconds()
	strategy["cooldown_auth_delay_ms"] = cfg.CooldownAuthDelay.Milliseconds()
	strategy["cooldown_auth_delay_min_ms"] = cfg.CooldownAuthMin.Milliseconds()
	strategy["cooldown_auth_delay_max_ms"] = cfg.CooldownAuthMax.Milliseconds()
	strategy["cooldown_auth_delay_step_ms"] = cfg.CooldownAuthStep.Milliseconds()
	strategy["cooldown_auth_jitter_percent"] = cfg.CooldownAuthJitter
	strategy["cooldown_auth_learning_persistent"] = false
	strategy["cooldown_auth_success"] = "shell_or_exec_held"
	strategy["cooldown_auth_success_hold_ms"] = cfg.Delay.Milliseconds()
	strategy["parallel_connections"] = cfg.ParallelConnections
	strategy["parallel_mode"] = "delayed_rejection"
	strategy["network_jitter"] = networkDelayPolicy(cfg.Delay)
	return map[string]any{"accepted": s.stats.accepted.Load(), "rejected": s.stats.rejected.Load(), "capacity_evicted": s.stats.evicted.Load(), "accept_errors": s.stats.acceptErrors.Load(), "closed": s.stats.closed.Load(), "active": s.stats.active.Load(), "pending_handshakes": s.stats.pending.Load(), "bytes_sent": s.stats.bytesSent.Load(), "auth_attempts": s.stats.authAttempts.Load(), "connection_seconds": float64(nanos) / float64(time.Second), "storage_errors": s.storageErrors.Load(), "strategy": strategy, "live": live}
}

func (s *Server) storageError(err error) {
	if err != nil {
		s.storageErrors.Add(1)
		s.log(0, "storage_error", map[string]any{"error": "database operation failed"})
	}
}

func (s *Server) Stop() {
	s.reloadMu.Lock()
	s.mu.Lock()
	if !s.stopping {
		s.stopping = true
		close(s.changed)
		if s.listener != nil {
			s.listener.Close()
		}
		for c := range s.clients {
			c.cancel()
			c.Conn.Close()
		}
	}
	s.mu.Unlock()
	s.reloadMu.Unlock()
	s.wg.Wait()
	s.log(1, "totals", s.Snapshot())
}
