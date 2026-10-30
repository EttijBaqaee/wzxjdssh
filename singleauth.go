package main

import (
	"errors"
	"io"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// A client may send SSH_MSG_DISCONNECT before the TCP reader observes EOF.
func isPeerAuthEnd(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || strings.HasPrefix(err.Error(), "ssh: disconnect, reason ") {
		return true
	}
	var authErr *ssh.ServerAuthError
	if errors.As(err, &authErr) {
		for _, cause := range authErr.Errors {
			if isPeerAuthEnd(cause) {
				return true
			}
		}
	}
	return false
}

type singleAuthEntry struct {
	first time.Time
	count int
}

// Guarded by policyMu. Counts are bounded, process-local, and expire as a window.
type singleAuthRetries struct {
	entries   map[authDelayKey]singleAuthEntry
	lastPrune time.Time
}

func (r *singleAuthRetries) observe(key authDelayKey, eligible bool, now time.Time, cfg Config) bool {
	if r.entries == nil {
		r.entries = make(map[authDelayKey]singleAuthEntry)
	}
	if now.Sub(r.lastPrune) >= time.Minute {
		for k, e := range r.entries {
			if now.Sub(e.first) >= cfg.SingleAuthRetryWindow {
				delete(r.entries, k)
			}
		}
		r.lastPrune = now
	}
	if !eligible || cfg.SingleAuthRetryThreshold == 0 {
		delete(r.entries, key)
		return false
	}
	e, ok := r.entries[key]
	if !ok || now.Sub(e.first) >= cfg.SingleAuthRetryWindow {
		e = singleAuthEntry{first: now}
	}
	e.count++
	if e.count >= cfg.SingleAuthRetryThreshold {
		delete(r.entries, key)
		return true
	}
	if ok || len(r.entries) < 100000 {
		r.entries[key] = e
	}
	return false
}

func (c *clientConn) singleAuthRetryEligible(status, reason string, stopping bool) bool {
	return !stopping && c.cfg.Mode == "ssh" && status == "auth_rejected" && reason == "authentication_failed" &&
		(c.peerClosed.Load() || c.peerAuthEnded) && !c.readTimedOut.Load() && !c.lifetimeExpired.Load() && !c.capacityEvicted.Load() &&
		!c.forceSlow.Load() && !c.simulated.Load() && c.markedUntil == 0 && c.probeUntil == 0 &&
		c.attemptCount == 1 && c.firstAuthMethod == "password" && c.rejectedAuth &&
		(c.cfg.MaxAuthTries == 0 || c.attemptCount < c.cfg.MaxAuthTries)
}
