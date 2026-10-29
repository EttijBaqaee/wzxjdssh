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
	for {
		s.mu.Lock()
		changed := s.changed
		delay := s.cfg.Load().Delay
		stopped := s.stopping
		s.mu.Unlock()
		if stopped {
			return context.Canceled
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
	if len(s.clients) >= c.MaxClients {
		return "max_clients"
	}
	if c.MaxClientsPerIP > 0 && s.ipCount[ip] >= c.MaxClientsPerIP {
		return "per_ip_limit"
	}
	if c.Mode == "ssh" && s.stats.pending.Load() >= int64(c.MaxPendingHandshakes) {
		return "handshake_limit"
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
		s.mu.Lock()
		cfg := *s.cfg.Load()
		reason := s.admit(ip, cfg)
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
		ctx, cancel := context.WithTimeout(context.Background(), cfg.MaxConnectionTime)
		c := &clientConn{Conn: raw, server: s, cfg: cfg, ip: ip, started: time.Now(), ctx: ctx, cancel: cancel}
		if cfg.Mode == "ssh" {
			c.pending.Store(true)
			s.stats.pending.Add(1)
		}
		s.clients[c] = struct{}{}
		s.ipCount[ip]++
		s.stats.active.Add(1)
		s.stats.accepted.Add(1)
		s.wg.Add(1)
		s.mu.Unlock()
		s.log(1, "accepted", map[string]any{"peer": raw.RemoteAddr().String(), "mode": cfg.Mode, "active": s.stats.active.Load()})
		go s.handle(c)
	}
}

type clientConn struct {
	net.Conn
	server         *Server
	cfg            Config
	ip             string
	started        time.Time
	ctx            context.Context
	cancel         context.CancelFunc
	pending        atomic.Bool
	authStage      atomic.Bool
	bytesSent      atomic.Uint64
	readDeadlineMu sync.Mutex
	delaying       bool
	recordID       int64
	bytesReceived  atomic.Uint64
	attemptCount   int
	readTimedOut   atomic.Bool
	peerClosed     atomic.Bool
	delayStarted   time.Time     // Protected by server.mu.
	delayed        time.Duration // Completed delay waits; protected by server.mu.
}

func (c *clientConn) releasePending() {
	if c.pending.Swap(false) {
		c.server.stats.pending.Add(-1)
	}
}

func (c *clientConn) Read(p []byte) (int, error) {
	c.readDeadlineMu.Lock()
	if c.authStage.Load() && !c.delaying {
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
	if err := c.Conn.SetWriteDeadline(time.Now().Add(c.cfg.WriteTimeout)); err != nil {
		return 0, err
	}
	n, err := writeAll(c.Conn, p)
	c.bytesSent.Add(uint64(n))
	c.server.stats.bytesSent.Add(uint64(n))
	return n, err
}

func (s *Server) handle(c *clientConn) {
	defer s.wg.Done()
	defer func() {
		status, reason := "closed", "peer_closed"
		if c.readTimedOut.Load() {
			status = "timeout"
			reason = "handshake_timeout"
			if c.authStage.Load() {
				reason = "idle_timeout"
			}
		}
		if errors.Is(c.ctx.Err(), context.DeadlineExceeded) {
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
		} else if c.cfg.Mode == "ssh" && c.attemptCount > 0 {
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
		c.cancel()
		c.Conn.Close()
		c.releasePending()
		s.mu.Lock()
		delete(s.clients, c)
		s.ipCount[c.ip]--
		if s.ipCount[c.ip] == 0 {
			delete(s.ipCount, c.ip)
		}
		s.stats.active.Add(-1)
		s.mu.Unlock()
		duration := time.Since(c.started)
		s.stats.closedNanos.Add(int64(duration))
		s.stats.closed.Add(1)
		if s.store != nil && c.recordID != 0 {
			s.storageError(s.store.Finish(c.recordID, duration, status, reason, c.bytesSent.Load(), c.bytesReceived.Load()))
		}
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
	}
	if c.cfg.Mode == "banner" {
		s.serveBanner(c)
		return
	}
	_ = c.Conn.SetReadDeadline(time.Now().Add(c.cfg.HandshakeTimeout))
	cfg := &ssh.ServerConfig{MaxAuthTries: c.cfg.MaxAuthTries, ServerVersion: c.cfg.ServerVersion}
	for _, key := range s.keys {
		cfg.AddHostKey(key)
	}
	// Callback runs only after SSH transport and key exchange are ready.
	cfg.PreAuthConnCallback = func(meta ssh.ServerPreAuthConn) {
		c.authStage.Store(true)
		c.releasePending()
		_ = c.Conn.SetReadDeadline(time.Now().Add(c.cfg.IdleTimeout))
		if s.store != nil {
			if err := s.store.Transport(c.recordID, string(meta.ClientVersion())); err != nil {
				s.storageError(err)
				c.cancel()
			}
		}
		s.log(2, "ssh_transport_ready", map[string]any{"peer": c.RemoteAddr().String(), "client_version": string(meta.ClientVersion())})
	}
	denied := errors.New("permission denied")
	delay := func() error {
		c.readDeadlineMu.Lock()
		c.delaying = true
		_ = c.Conn.SetReadDeadline(time.Time{})
		c.readDeadlineMu.Unlock()
		err := c.pause()
		c.readDeadlineMu.Lock()
		c.delaying = false
		_ = c.Conn.SetReadDeadline(time.Now().Add(c.cfg.IdleTimeout))
		c.readDeadlineMu.Unlock()
		if err != nil {
			return err
		}
		return denied
	}
	record := func(meta ssh.ConnMetadata, method string, credential []byte, key ssh.PublicKey) (int64, error) {
		c.attemptCount++
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
		if s.store != nil && id != 0 {
			result := "rejected"
			if !errors.Is(err, denied) {
				result = "interrupted"
			}
			s.storageError(s.store.AttemptResult(id, result))
		}
		return err
	}
	cfg.PasswordCallback = func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		id, err := record(meta, "password", password, nil)
		if err != nil {
			return nil, err
		}
		return nil, finishAttempt(id, delay())
	}
	cfg.PublicKeyCallback = func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		id, err := record(meta, "publickey_probe", nil, key)
		if err != nil {
			return nil, err
		}
		return nil, finishAttempt(id, delay())
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
		return nil, finishAttempt(id, delay())
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
		s.log(1, "authentication", map[string]any{"peer": c.RemoteAddr().String(), "method": method, "accepted": false})
	}
	conn, chans, requests, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		s.log(2, "ssh_closed", map[string]any{"peer": c.RemoteAddr().String(), "reason": err.Error()})
		return
	}
	// Defensive guard: no callback is allowed to authenticate. Never open a shell.
	defer conn.Close()
	go ssh.DiscardRequests(requests)
	for channel := range chans {
		_ = channel.Reject(ssh.Prohibited, "administratively prohibited")
	}
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
	strategy := map[string]any{"mode": cfg.Mode, "delay_ms": cfg.Delay.Milliseconds(), "max_line_length": cfg.MaxLineLength, "max_auth_tries": cfg.MaxAuthTries, "handshake_timeout_ms": cfg.HandshakeTimeout.Milliseconds(), "idle_timeout_ms": cfg.IdleTimeout.Milliseconds(), "max_connection_time_ms": cfg.MaxConnectionTime.Milliseconds(), "max_clients": cfg.MaxClients, "max_clients_per_ip": cfg.MaxClientsPerIP, "connections_per_minute": cfg.ConnectionsPerMinute, "max_pending_handshakes": cfg.MaxPendingHandshakes}
	return map[string]any{"accepted": s.stats.accepted.Load(), "rejected": s.stats.rejected.Load(), "accept_errors": s.stats.acceptErrors.Load(), "closed": s.stats.closed.Load(), "active": s.stats.active.Load(), "pending_handshakes": s.stats.pending.Load(), "bytes_sent": s.stats.bytesSent.Load(), "auth_attempts": s.stats.authAttempts.Load(), "connection_seconds": float64(nanos) / float64(time.Second), "storage_errors": s.storageErrors.Load(), "strategy": strategy, "live": live}
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
