package main

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestUnlimitedLifetimeConfiguration(t *testing.T) {
	if DefaultConfig().MaxConnectionTime != 0 {
		t.Fatal("default lifetime is bounded")
	}
	for _, value := range []string{"0", "3600000", "604800000"} {
		if _, err := parseConfig(strings.NewReader("MaxConnectionTime " + value + "\n")); err != nil {
			t.Fatal(value, err)
		}
	}
	for _, value := range []string{"-1", "604800001", "0 1"} {
		if _, err := parseConfig(strings.NewReader("MaxConnectionTime " + value + "\n")); err == nil {
			t.Fatal("invalid lifetime", value)
		}
	}
}

func TestSlowBannerOutlivesLegacyLifetimeWithoutCapacityPressure(t *testing.T) {
	cfg := testConfig(t)
	cfg.Mode = "banner"
	cfg.MaxConnectionTime = 40 * time.Millisecond
	cfg.Delay = 70 * time.Millisecond
	cfg.MaxClients = 1
	s := startTest(t, cfg)
	conn := connect(t, cfg)
	defer conn.Close()
	for i := 0; i < 4; i++ {
		bannerLine(t, conn)
	}
	if s.stats.active.Load() != 1 || s.stats.evicted.Load() != 0 {
		t.Fatal("slow connection did not survive", s.Snapshot())
	}
}

func TestDelayedAuthenticationOutlivesLegacyLifetime(t *testing.T) {
	cfg := testConfig(t)
	cfg.MaxConnectionTime = 150 * time.Millisecond
	cfg.Delay = 400 * time.Millisecond
	s := startTest(t, cfg)
	started := time.Now()
	conn, err := ssh.Dial("tcp", address(cfg), &ssh.ClientConfig{User: "long-delay", Auth: []ssh.AuthMethod{ssh.Password("long-delay-password")}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: time.Second})
	if conn != nil {
		conn.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "unable to authenticate") || time.Since(started) < cfg.Delay {
		t.Fatal("lifetime interrupted authentication delay", err)
	}
	if s.stats.evicted.Load() != 0 {
		t.Fatal("spontaneous capacity eviction")
	}
}

func TestCapacityReplacesOldestSlowAndRecordsReason(t *testing.T) {
	cfg := testConfig(t)
	cfg.Mode = "banner"
	cfg.Delay = 25 * time.Millisecond
	cfg.MaxClients = 2
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
	first := connect(t, cfg)
	defer first.Close()
	bannerLine(t, first)
	second := connect(t, cfg)
	defer second.Close()
	bannerLine(t, second)
	time.Sleep(70 * time.Millisecond)
	if s.stats.evicted.Load() != 0 {
		t.Fatal("being full without a new connection caused eviction")
	}
	third := connect(t, cfg)
	defer third.Close()
	bannerLine(t, third)
	eventually(t, func() bool {
		return s.stats.accepted.Load() == 3 && s.stats.active.Load() == 2 && s.stats.evicted.Load() == 1
	})
	page, err := store.List(context.Background(), RecordFilter{Status: "capacity_evicted"})
	if err != nil || page.Total != 1 || page.Items[0].Peer != first.LocalAddr().String() || page.Items[0].Reason != "connection_capacity" || page.Items[0].ClientMark != "" {
		t.Fatal(page, err)
	}
	// Newer existing connection remains responsive; a recycled slot is usable.
	bannerLine(t, second)
	bannerLine(t, third)
	for i := 0; i < 8; i++ {
		next := connect(t, cfg)
		bannerLine(t, next)
		peer := next.LocalAddr().String()
		next.Close()
		// Wait for this worker to release its slot; a count of two can still
		// include the just-closed connection during asynchronous cleanup.
		eventually(t, func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			for c := range s.clients {
				if c.RemoteAddr().String() == peer {
					return false
				}
			}
			return true
		})
	}
	if s.stats.active.Load() > 2 || s.stats.rejected.Load() != 0 {
		t.Fatal("capacity accounting", s.Snapshot())
	}
}

func TestRateLimitedConnectionDoesNotEvictSlowClient(t *testing.T) {
	cfg := testConfig(t)
	cfg.Mode = "banner"
	cfg.MaxClients = 1
	cfg.ConnectionsPerMinute = 1
	s := startTest(t, cfg)
	first := connect(t, cfg)
	defer first.Close()
	bannerLine(t, first)
	second := connect(t, cfg)
	second.SetReadDeadline(time.Now().Add(time.Second))
	_, err := second.Read(make([]byte, 1))
	second.Close()
	if err == nil {
		t.Fatal("rate-limited connection served")
	}
	eventually(t, func() bool { return s.stats.rejected.Load() == 1 })
	if s.stats.evicted.Load() != 0 || s.stats.active.Load() != 1 {
		t.Fatal("rate rejection evicted slow client")
	}
	bannerLine(t, first)
}

func TestCapacityEvictionDoesNotMarkAuthenticationTimeout(t *testing.T) {
	cfg := testConfig(t)
	cfg.MaxClients = 1
	cfg.Delay = time.Hour
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
	version := "SSH-2.0-capacity-recycle-test"
	done := make(chan error, 1)
	go func() {
		c, err := ssh.Dial("tcp", address(cfg), &ssh.ClientConfig{User: "capacity-user", ClientVersion: version, Auth: []ssh.AuthMethod{ssh.Password("capacity-password")}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: time.Second})
		if c != nil {
			c.Close()
		}
		done <- err
	}()
	eventually(t, func() bool { return s.Snapshot()["live"].(LiveStats).DelayingConnections == 1 })
	next := connect(t, cfg)
	defer next.Close()
	bannerLine(t, next)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("authentication unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("capacity did not cancel old delay")
	}
	page, err := store.List(context.Background(), RecordFilter{Status: "capacity_evicted"})
	if err != nil || page.Total != 1 || page.Items[0].Reason != "connection_capacity" {
		t.Fatal(page, err)
	}
	if until, err := store.ClientCooldown("127.0.0.1", version, time.Now()); err != nil || until != 0 {
		t.Fatal("capacity eviction created timeout cooldown", until, err)
	}
}

func TestPerIPCapacityVictimSelectionAndNoPartialEviction(t *testing.T) {
	cfg := testConfig(t)
	cfg.MaxClients = 3
	cfg.MaxClientsPerIP = 2
	s, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	makeClient := func(ip string, age time.Duration, slow bool) *clientConn {
		local, remote := net.Pipe()
		t.Cleanup(func() { local.Close(); remote.Close() })
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		c := &clientConn{Conn: local, server: s, cfg: cfg, ip: ip, ctx: ctx, cancel: cancel, started: time.Now().Add(-age)}
		c.longLived.Store(slow)
		s.clients[c] = struct{}{}
		s.ipCount[ip]++
		return c
	}
	oldestOtherIP := makeClient("192.0.2.2", time.Hour, true)
	oldestSameIP := makeClient("192.0.2.1", time.Minute, true)
	makeClient("192.0.2.1", time.Second, true)
	victims, reason := s.capacityVictimsLocked("192.0.2.1", cfg)
	if reason != "" || len(victims) != 1 || victims[0] != oldestSameIP {
		t.Fatal("per-IP capacity evicted unrelated IP", reason, victims)
	}
	victims, reason = s.capacityVictimsLocked("192.0.2.3", cfg)
	if reason != "" || len(victims) != 1 || victims[0] != oldestOtherIP {
		t.Fatal("global capacity victim incorrect", reason, victims)
	}
	cfg.MaxClients = 1
	oldestOtherIP.longLived.Store(false)
	victims, reason = s.capacityVictimsLocked("192.0.2.3", cfg)
	if reason != "max_clients" || len(victims) != 0 {
		t.Fatal("partially satisfiable capacity must reject without eviction", reason, len(victims))
	}
}
