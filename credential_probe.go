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
	left := c.cfg.CredentialTimeout - time.Since(c.started)
	timer := time.AfterFunc(max(time.Duration(0), left), func() {
		if c.credentialState.CompareAndSwap(credentialWaiting, credentialExpired) {
			c.cancel()
			c.Conn.Close()
		}
	})
	return func() { timer.Stop() }
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
