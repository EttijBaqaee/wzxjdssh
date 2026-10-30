package main

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestSingleAuthRetryCooldownPersistsAndExpires(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.sqlite")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	ip, version := "2001:db8::44", "SSH-2.0-Go"
	id := mustStart(t, store, ip, at)
	until, err := store.markClientCooldown(id, ip, version, "single_auth_retry", at, 10*time.Minute)
	if err != nil || until != at.Add(10*time.Minute).UnixMilli() {
		t.Fatal(until, err)
	}
	if later, err := store.markClientCooldown(id, ip, version, "single_auth_retry", at.Add(time.Minute), time.Hour); err != nil || later != until {
		t.Fatal("active cooldown renewed", later, err)
	}
	store.Close()
	store, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if got, err := store.ClientCooldown(ip, version, at.Add(9*time.Minute)); err != nil || got != until {
		t.Fatal("persistent cooldown lost", got, err)
	}
	if got, err := store.ClientCooldown(ip, version, at.Add(10*time.Minute)); err != nil || got != 0 {
		t.Fatal("cooldown did not expire", got, err)
	}
	record, _, err := store.Detail(context.Background(), id)
	if err != nil || record.ClientMark != "single_auth_retry" || record.MarkExpiresAt != until {
		t.Fatal(record, err)
	}
}

func TestSingleAuthRetryWindowAndIsolation(t *testing.T) {
	cfg := DefaultConfig()
	now := time.Now()
	k := authDelayKey{"2001:db8::1", "SSH-2.0-Go"}
	r := singleAuthRetries{}
	if r.observe(k, true, now, cfg) || r.observe(k, true, now.Add(time.Minute), cfg) {
		t.Fatal("early trigger")
	}
	if r.observe(authDelayKey{k.ip, "SSH-2.0-other"}, true, now, cfg) || r.observe(authDelayKey{"2001:db8::2", k.version}, true, now, cfg) {
		t.Fatal("mixed clients")
	}
	if !r.observe(k, true, now.Add(2*time.Minute), cfg) {
		t.Fatal("third rejection not marked")
	}
	if r.observe(k, true, now.Add(3*time.Minute), cfg) {
		t.Fatal("trigger count retained")
	}
	r.observe(k, false, now.Add(4*time.Minute), cfg)
	if r.observe(k, true, now.Add(5*time.Minute), cfg) || r.observe(k, true, now.Add(6*time.Minute), cfg) {
		t.Fatal("nonmatching outcome did not reset streak")
	}
	if r.observe(k, true, now.Add(20*time.Minute), cfg) {
		t.Fatal("old window retained")
	}
	cfg.SingleAuthRetryThreshold = 0
	if r.observe(k, true, now.Add(21*time.Minute), cfg) || len(r.entries) != 0 {
		t.Fatal("disabled rule retained counts")
	}
}

func TestSingleAuthRetryExcludesOtherOutcomes(t *testing.T) {
	if !isPeerAuthEnd(io.EOF) || !isPeerAuthEnd(errors.New("ssh: disconnect, reason 14: No supported authentication methods")) || !isPeerAuthEnd(&ssh.ServerAuthError{Errors: []error{io.EOF}}) || isPeerAuthEnd(errors.New("server auth limit")) {
		t.Fatal("incorrect peer-disconnect classification")
	}
	makeConn := func() *clientConn {
		c := &clientConn{cfg: DefaultConfig(), attemptCount: 1, firstAuthMethod: "password", rejectedAuth: true}
		c.peerClosed.Store(true)
		return c
	}
	if !makeConn().singleAuthRetryEligible("auth_rejected", "authentication_failed", false) {
		t.Fatal("ordinary single rejection excluded")
	}
	protocol := makeConn()
	protocol.peerClosed.Store(false)
	protocol.peerAuthEnded = true
	if !protocol.singleAuthRetryEligible("auth_rejected", "authentication_failed", false) {
		t.Fatal("SSH disconnect excluded")
	}
	for name, mutate := range map[string]func(*clientConn){
		"multiple": func(c *clientConn) { c.attemptCount = 2 }, "publickey": func(c *clientConn) { c.firstAuthMethod = "publickey_probe" },
		"parallel": func(c *clientConn) { c.forceSlow.Store(true) }, "capacity": func(c *clientConn) { c.capacityEvicted.Store(true) },
		"timeout": func(c *clientConn) { c.readTimedOut.Store(true) }, "lifetime": func(c *clientConn) { c.lifetimeExpired.Store(true) },
		"marked": func(c *clientConn) { c.markedUntil = 1 }, "pending": func(c *clientConn) { c.rejectedAuth = false },
		"serverLimit": func(c *clientConn) { c.cfg.MaxAuthTries = 1 }, "notPeerClosed": func(c *clientConn) { c.peerClosed.Store(false) },
	} {
		t.Run(name, func(t *testing.T) {
			c := makeConn()
			mutate(c)
			if c.singleAuthRetryEligible("auth_rejected", "authentication_failed", false) {
				t.Fatal("ineligible result counted")
			}
		})
	}
	if makeConn().singleAuthRetryEligible("auth_rejected", "authentication_failed", true) {
		t.Fatal("shutdown counted")
	}
}

func TestSingleAuthRetryRealSSHReconnect(t *testing.T) {
	cfg := testConfig(t)
	cfg.Delay = 20 * time.Millisecond
	cfg.DelayJitter = 10
	store := testStore(t)
	s, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.store = store
	if err = s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	client := &ssh.ClientConfig{User: "single-auth-test", ClientVersion: "SSH-2.0-Go-single-test", Auth: []ssh.AuthMethod{ssh.Password("synthetic-only")}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 20 * time.Second}
	var until int64
	for i := 1; i <= 3; i++ {
		c, err := ssh.Dial("tcp", address(cfg), client)
		if c != nil {
			c.Close()
		}
		if err == nil {
			t.Fatal("unmarked client accepted")
		}
		eventually(t, func() bool { return s.stats.closed.Load() >= uint64(i) })
		// Wait for final database writes, which follow the active counter update.
		eventually(t, func() bool {
			p, _ := store.List(context.Background(), RecordFilter{})
			return len(p.Items) == i && p.Items[0].EndedAt != nil
		})
		if i == 3 {
			eventually(t, func() bool {
				until, _ = store.ClientCooldown("127.0.0.1", client.ClientVersion, time.Now())
				return until > 0
			})
		} else if u, _ := store.ClientCooldown("127.0.0.1", client.ClientVersion, time.Now()); u != 0 {
			t.Fatal("early mark")
		}
	}
	page, _ := store.List(context.Background(), RecordFilter{})
	if page.Items[0].ClientMark != "single_auth_retry" {
		t.Fatal("wrong mark", page.Items[0])
	}
	started := time.Now()
	c, err := ssh.Dial("tcp", address(cfg), client)
	if err != nil {
		t.Fatal("marked client failed", err)
	}
	if time.Since(started) < cfg.CooldownAuthMin {
		t.Fatal("authentication delay bypassed")
	}
	session, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err = session.Start("echo MUST-NOT-EXECUTE"); err != nil {
		t.Fatal(err)
	}
	c.Close()
	eventually(t, func() bool { return s.stats.active.Load() == 0 })
	if u, _ := store.ClientCooldown("127.0.0.1", client.ClientVersion, time.Now()); u != until {
		t.Fatal("reconnect extended cooldown")
	}
}

func TestSingleAuthRetryConfiguration(t *testing.T) {
	c, err := parseConfig(strings.NewReader("SingleAuthRetryThreshold 4\nSingleAuthRetryWindow 60000\nSingleAuthRetryCooldown 120000\n"))
	if err != nil || c.SingleAuthRetryThreshold != 4 || c.SingleAuthRetryWindow != time.Minute || c.SingleAuthRetryCooldown != 2*time.Minute {
		t.Fatal(c, err)
	}
	c, err = configEnvironment(c, func(k string) string {
		return map[string]string{"WZXJDSSH_SINGLE_AUTH_RETRY_THRESHOLD": "0", "WZXJDSSH_SINGLE_AUTH_RETRY_WINDOW_MS": "900000", "WZXJDSSH_SINGLE_AUTH_RETRY_COOLDOWN_MS": "3600000"}[k]
	})
	if err != nil || c.SingleAuthRetryThreshold != 0 || c.SingleAuthRetryWindow != 15*time.Minute || c.SingleAuthRetryCooldown != time.Hour {
		t.Fatal(c, err)
	}
	for _, values := range []map[string]string{{"WZXJDSSH_SINGLE_AUTH_RETRY_THRESHOLD": "-1"}, {"WZXJDSSH_SINGLE_AUTH_RETRY_THRESHOLD": "100001"}, {"WZXJDSSH_SINGLE_AUTH_RETRY_WINDOW_MS": "0"}, {"WZXJDSSH_SINGLE_AUTH_RETRY_WINDOW_MS": "86400001"}, {"WZXJDSSH_SINGLE_AUTH_RETRY_COOLDOWN_MS": "0"}, {"WZXJDSSH_SINGLE_AUTH_RETRY_COOLDOWN_MS": "604800001"}} {
		if _, err = configEnvironment(DefaultConfig(), func(k string) string { return values[k] }); err == nil {
			t.Fatal("invalid config accepted", values)
		}
	}
}
