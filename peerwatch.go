package main

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

type peerWatch struct {
	wake       chan struct{}
	mu         sync.Mutex
	held       context.Context
	pending    atomic.Bool
	verifiedAt atomic.Int64
	missed     atomic.Int64
}

type PeerLiveness struct {
	LastReceivedAt int64 `json:"last_received_at"`
	LastVerifiedAt int64 `json:"last_verified_at"`
	Pending        bool  `json:"pending"`
	Missed         int64 `json:"missed"`
	CountMax       int   `json:"count_max"`
}

func (c *clientConn) measuredDelay() time.Duration {
	c.server.mu.Lock()
	defer c.server.mu.Unlock()
	delay := c.delayed
	if !c.delayStarted.IsZero() {
		delay += time.Since(c.delayStarted)
	}
	return delay
}

// A session reaching the timer threshold only requests verification. Learning
// requires a valid SSH response while that same session is still open.
func (c *clientConn) verifyHeldSession(ctx context.Context) {
	c.peer.mu.Lock()
	c.peer.held = ctx
	c.peer.mu.Unlock()
	select {
	case c.peer.wake <- struct{}{}:
	default:
	}
}

func (c *clientConn) learnVerifiedSession() {
	c.peer.mu.Lock()
	ctx := c.peer.held
	c.peer.held = nil
	c.peer.mu.Unlock()
	if ctx != nil && ctx.Err() == nil && c.ctx.Err() == nil && !c.authLearned.Load() {
		c.sessionEvent("session_held", "")
		c.learnAuthDelay(true)
	}
}

// Send only one outstanding want-reply request. Negative SSH request replies
// also prove that the peer's SSH implementation is responding. TCP ACKs do not.
// A silent peer never causes one new goroutine per retry interval.
func (c *clientConn) checkPeer(conn *ssh.ServerConn) bool {
	cfg := c.cfg
	started, baseline := time.Now(), c.measuredDelay()
	c.peer.pending.Store(true)
	defer c.peer.pending.Store(false)
	done := make(chan error, 1)
	go func() { _, _, err := conn.SendRequest("keepalive@openssh.com", true, nil); done <- err }()
	timer := time.NewTimer(cfg.SSHKeepaliveTimeout)
	defer timer.Stop()
	stop := func() bool {
		c.cancel()
		c.Conn.Close()
		<-done // Closing the transport also releases the single request waiter.
		return false
	}
	for {
		select {
		case err := <-done:
			if err != nil {
				c.cancel()
				c.Conn.Close()
				return false
			}
			c.peer.verifiedAt.Store(time.Now().UnixMilli())
			c.peer.missed.Store(0)
			c.learnVerifiedSession()
			return true
		case <-c.ctx.Done():
			return stop()
		case <-timer.C:
			// Include queued writes, but exclude our deliberately imposed waits.
			elapsed := time.Since(started) - (c.measuredDelay() - baseline)
			if elapsed < 0 {
				elapsed = 0
			}
			missed := int64(elapsed / cfg.SSHKeepaliveTimeout)
			c.peer.missed.Store(missed)
			if missed >= int64(cfg.SSHKeepaliveCountMax) {
				c.peerUnresponsive.Store(true)
				return stop()
			}
			next := time.Duration(missed+1)*cfg.SSHKeepaliveTimeout - elapsed
			if next < time.Millisecond {
				next = time.Millisecond
			}
			timer.Reset(next)
		}
	}
}

func (c *clientConn) watchPeer(conn *ssh.ServerConn) {
	timer := time.NewTimer(c.cfg.SSHKeepaliveInterval)
	defer timer.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-timer.C:
		case <-c.peer.wake:
		}
		if !c.checkPeer(conn) {
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(c.cfg.SSHKeepaliveInterval)
	}
}

type LiveRecordSnapshot struct {
	DurationMS    int64
	DelayedMS     int64
	BytesSent     int64
	BytesReceived int64
	Session       *SessionTrace
	Liveness      *PeerLiveness
}

// Obtain the pointer under server.mu, then copy its separately guarded trace.
// No database operation or trace lock runs while server.mu is held.
func (s *Server) ConnectionDetail(id int64) (LiveRecordSnapshot, bool) {
	s.mu.Lock()
	var found *clientConn
	for c := range s.clients {
		if c.recordID == id {
			found = c
			break
		}
	}
	s.mu.Unlock()
	if found == nil {
		return LiveRecordSnapshot{}, false
	}
	c := found
	out := LiveRecordSnapshot{DurationMS: time.Since(c.started).Milliseconds(), DelayedMS: c.measuredDelay().Milliseconds(), BytesSent: int64(c.bytesSent.Load()), BytesReceived: int64(c.bytesReceived.Load())}
	if c.simulated.Load() {
		out.Liveness = &PeerLiveness{LastReceivedAt: c.lastReceivedAt.Load(), LastVerifiedAt: c.peer.verifiedAt.Load(), Pending: c.peer.pending.Load(), Missed: c.peer.missed.Load(), CountMax: c.cfg.SSHKeepaliveCountMax}
		state := &c.sessionTrace
		state.mu.Lock()
		if !state.started.IsZero() {
			trace := state.trace
			trace.Events = append([]SessionEvent(nil), trace.Events...)
			if trace.EndedAt == 0 {
				trace.PostAuthMS = time.Since(state.started).Milliseconds()
			}
			trace.LastReceivedAt, trace.LastVerifiedAt = out.Liveness.LastReceivedAt, out.Liveness.LastVerifiedAt
			out.Session = &trace
		}
		state.mu.Unlock()
	}
	return out, true
}
