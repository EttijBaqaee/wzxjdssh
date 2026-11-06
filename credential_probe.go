package main

import (
	"errors"
	"time"
)

const (
	credentialWaiting uint32 = iota
	credentialSubmitted
	credentialExpired
)

// Classification belongs to this connection. Never create an IP cooldown or
// feed the legacy consecutive-handshake detector for no-credential probes.
func (c *clientConn) credentialWatch() func() {
	if c.cfg.Mode != "ssh" || c.probeUntil > 0 {
		return func() {}
	}
	c.readDeadlineMu.Lock()
	c.credentialRemaining = c.cfg.CredentialTimeout
	c.credentialSince = time.Now()
	c.credentialTimer = time.AfterFunc(c.credentialRemaining, func() {
		c.readDeadlineMu.Lock()
		if c.credentialTimer == nil || c.serverWork > 0 || c.credentialState.Load() != credentialWaiting {
			c.readDeadlineMu.Unlock()
			return
		}
		left := c.credentialRemaining - time.Since(c.credentialSince)
		if left > 0 {
			c.credentialTimer.Reset(left)
			c.readDeadlineMu.Unlock()
			return
		}
		expired := c.credentialState.CompareAndSwap(credentialWaiting, credentialExpired)
		c.readDeadlineMu.Unlock()
		if expired {
			c.cancel()
			c.Conn.Close()
		}
	})
	c.readDeadlineMu.Unlock()
	return func() {
		c.readDeadlineMu.Lock()
		defer c.readDeadlineMu.Unlock()
		c.credentialTimer.Stop()
		c.credentialTimer = nil
	}
}

func (c *clientConn) submittedCredential(username string) error {
	if username == "" {
		if c.credentialState.CompareAndSwap(credentialWaiting, credentialExpired) {
			c.cancel()
			c.Conn.Close()
		}
		return errors.New("missing credential username")
	}
	if c.credentialState.CompareAndSwap(credentialWaiting, credentialSubmitted) || c.credentialState.Load() == credentialSubmitted {
		return nil
	}
	return errors.New("credential window expired")
}
