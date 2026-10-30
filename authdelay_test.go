package main

import (
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestAuthDelayLearningBoundsExpiryAndMemoryOnly(t *testing.T) {
	cfg := DefaultConfig()
	var learning authDelayLearning
	now := time.Now()
	until := now.Add(time.Hour).UnixMilli()
	key := authDelayKey{"2001:db8::1", "SSH-2.0-adaptive"}
	if got := learning.base(key, until, now, cfg); got != 10*time.Second {
		t.Fatal(got)
	}
	for i := 0; i < 30; i++ {
		learning.observe(key, until, now, false, cfg)
	}
	if got := learning.base(key, until, now, cfg); got != 3*time.Second {
		t.Fatal(got)
	}
	for i := 0; i < 30; i++ {
		learning.observe(key, until, now, true, cfg)
	}
	if got := learning.base(key, until, now, cfg); got != 15*time.Second {
		t.Fatal(got)
	}
	other := authDelayKey{key.ip, "SSH-2.0-other"}
	if got := learning.base(other, until, now, cfg); got != cfg.CooldownAuthDelay {
		t.Fatal("client identities shared learning", got)
	}
	var restarted authDelayLearning
	if got := restarted.base(key, until, now, cfg); got != cfg.CooldownAuthDelay {
		t.Fatal("learning survived restart", got)
	}
	later := now.Add(time.Hour)
	learning.base(key, until, later, cfg)
	if len(learning.entries) != 0 {
		t.Fatal("expired learning retained", learning.entries)
	}
	newUntil := later.Add(time.Hour).UnixMilli()
	if got := learning.base(key, newUntil, later, cfg); got != cfg.CooldownAuthDelay {
		t.Fatal("new cooldown reused old learning", got)
	}
	learning.observe(key, until, later, true, cfg)
	if got := learning.base(key, newUntil, later, cfg); got != cfg.CooldownAuthDelay {
		t.Fatal("old connection changed new cooldown", got)
	}
}

func TestAuthDelayJitterAlwaysWithinBounds(t *testing.T) {
	cfg := DefaultConfig()
	for _, base := range []time.Duration{3 * time.Second, 10 * time.Second, 15 * time.Second} {
		seen := map[time.Duration]bool{}
		for i := 0; i < 100; i++ {
			got, err := jitterAuthDelay(base, cfg)
			spread := base * time.Duration(cfg.CooldownAuthJitter) / 100
			if err != nil || got < cfg.CooldownAuthMin || got > cfg.CooldownAuthMax || got < base-spread || got > base+spread {
				t.Fatal(base, got, err)
			}
			seen[got] = true
		}
		if len(seen) < 2 {
			t.Fatal("authentication jitter is fixed")
		}
	}
}

func TestAdaptiveAuthenticationLearnsPeerOutcome(t *testing.T) {
	cfg := testConfig(t)
	cfg.CooldownAuthDelay = 4 * time.Second
	cfg.CooldownAuthMax = 5 * time.Second
	store := testStore(t)
	version := "SSH-2.0-adaptive-outcome"
	key := authDelayKey{"127.0.0.1", version}
	id := mustStart(t, store, key.ip, time.Now())
	until, err := store.MarkInterruptedClient(id, key.ip, version, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.store = store
	if err = s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	clientCfg := &ssh.ClientConfig{User: "adaptive", ClientVersion: version, Auth: []ssh.AuthMethod{ssh.Password("adaptive-password")}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second}
	raw, err := net.Dial("tcp", address(cfg))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		c, _, _, _ := ssh.NewClientConn(raw, address(cfg), clientCfg)
		if c != nil {
			c.Close()
		}
		close(done)
	}()
	eventually(t, func() bool { return s.Snapshot()["live"].(LiveStats).DelayingConnections == 1 })
	raw.Close()
	<-done
	eventually(t, func() bool { return s.stats.active.Load() == 0 })
	if got := s.authDelays.base(key, until, time.Now(), cfg); got != 3*time.Second {
		t.Fatal("early departure did not shorten delay", got)
	}
	started := time.Now()
	c, err := ssh.Dial("tcp", address(cfg), clientCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if elapsed := time.Since(started); elapsed < 3*time.Second || elapsed > 4*time.Second {
		t.Fatal("adaptive wait not used", elapsed)
	}
	session, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if got := s.authDelays.base(key, until, time.Now(), cfg); got != 4*time.Second {
		t.Fatal("continued session did not increase delay", got)
	}
	if _, _, err = c.SendRequest("keepalive@openssh.com", true, nil); err != nil {
		t.Fatal(err)
	}
	if got := s.authDelays.base(key, until, time.Now(), cfg); got != 4*time.Second {
		t.Fatal("one connection learned repeatedly", got)
	}
	c.Close()
	eventually(t, func() bool { return s.stats.active.Load() == 0 })
	if got := s.authDelays.base(key, until, time.Now(), cfg); got != 4*time.Second {
		t.Fatal("successful client later departure reduced delay", got)
	}
}

func TestAdaptiveAuthenticationConfig(t *testing.T) {
	values := map[string]string{"WZXJDSSH_COOLDOWN_AUTH_DELAY_MS": "8000", "WZXJDSSH_COOLDOWN_AUTH_DELAY_MIN_MS": "4000", "WZXJDSSH_COOLDOWN_AUTH_DELAY_MAX_MS": "20000", "WZXJDSSH_COOLDOWN_AUTH_DELAY_STEP_MS": "500", "WZXJDSSH_COOLDOWN_AUTH_JITTER_PERCENT": "15"}
	cfg, err := configEnvironment(DefaultConfig(), func(k string) string { return values[k] })
	if err != nil || cfg.CooldownAuthDelay != 8*time.Second || cfg.CooldownAuthMin != 4*time.Second || cfg.CooldownAuthMax != 20*time.Second || cfg.CooldownAuthStep != 500*time.Millisecond || cfg.CooldownAuthJitter != 15 {
		t.Fatal(cfg, err)
	}
	for key, values := range map[string][]string{"DELAY_MS": {"0", "2999", "15001"}, "DELAY_MIN_MS": {"0", "2999", "10001"}, "DELAY_MAX_MS": {"9999", "60001"}, "DELAY_STEP_MS": {"0", "60001"}, "JITTER_PERCENT": {"-1", "51", "abc"}} {
		for _, value := range values {
			if _, err := configEnvironment(DefaultConfig(), func(k string) string {
				if k == "WZXJDSSH_COOLDOWN_AUTH_"+key {
					return value
				}
				return ""
			}); err == nil {
				t.Fatal("invalid adaptive setting accepted", key, value)
			}
		}
	}
}
