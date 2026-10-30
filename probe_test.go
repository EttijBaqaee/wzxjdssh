package main

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestProbeConsecutivePerIPAndFixedCooldown(t *testing.T) {
	s := testStore(t)
	cfg := DefaultConfig()
	now := time.Now()
	ip := "192.0.2.70"
	result := func(ip string, bad bool, at time.Time) int64 {
		t.Helper()
		until, err := s.ProbeResult(0, ip, bad, at, cfg)
		if err != nil {
			t.Fatal(err)
		}
		return until
	}
	for i := 0; i < 2; i++ {
		if result(ip, true, now) != 0 {
			t.Fatal("flagged early")
		}
	}
	result(ip, false, now) // A completed handshake interrupts the streak.
	for i := 0; i < 2; i++ {
		if result(ip, true, now) != 0 {
			t.Fatal("success did not reset")
		}
	}
	if result("192.0.2.71", true, now) != 0 {
		t.Fatal("mixed different IPs")
	}
	until := result(ip, true, now)
	if until != now.Add(15*time.Minute).UnixMilli() {
		t.Fatal(until)
	}
	if got := result(ip, true, now.Add(time.Minute)); got != until {
		t.Fatal("reconnect extended fixed cooldown")
	}
	if got, err := s.ProbeCooldown(ip, now.Add(15*time.Minute)); err != nil || got != 0 {
		t.Fatal("expired probe retained", got, err)
	}
	if result(ip, true, now.Add(15*time.Minute)) != 0 {
		t.Fatal("expiry did not reset streak")
	}
}

func TestProbeSilentlyDiscardsAcrossSourcePorts(t *testing.T) {
	cfg := testConfig(t)
	cfg.ProbeCooldown = 500 * time.Millisecond
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
	ports := map[string]bool{}
	for i := 0; i < 3; i++ {
		conn := connect(t, cfg)
		ports[conn.LocalAddr().String()] = true
		bannerLine(t, conn)
		conn.Close()
		eventually(t, func() bool {
			page, err := store.List(context.Background(), RecordFilter{})
			if err != nil {
				return false
			}
			n := 0
			for _, r := range page.Items {
				if r.Reason == "handshake_incomplete" {
					n++
				}
			}
			return n == i+1
		})
	}
	if len(ports) != 3 {
		t.Fatal("test did not use different source ports")
	}
	eventually(t, func() bool { until, _ := store.ProbeCooldown("127.0.0.1", time.Now()); return until > 0 })
	conn := connect(t, cfg)
	defer conn.Close()
	conn.Write([]byte("SSH-2.0-probe-test\r\nignored"))
	conn.SetReadDeadline(time.Now().Add(75 * time.Millisecond))
	b := make([]byte, 256)
	n, err := conn.Read(b)
	if n != 0 {
		t.Fatalf("probe received payload: %q", b[:n])
	}
	if e, ok := err.(net.Error); !ok || !e.Timeout() {
		t.Fatal("probe was not silently held", err)
	}
	eventually(t, func() bool {
		page, _ := store.List(context.Background(), RecordFilter{})
		for _, r := range page.Items {
			if r.ClientMark == "handshake_probe" && r.EndedAt == nil {
				return true
			}
		}
		return false
	})
	conn.SetReadDeadline(time.Now().Add(time.Second))
	n, err = conn.Read(b)
	if n != 0 || err == nil {
		t.Fatal("probe did not close at cooldown expiry", n, err)
	}
	next := connect(t, cfg)
	defer next.Close()
	bannerLine(t, next)
}

func TestProbeCooldownPersistsRestart(t *testing.T) {
	path := t.TempDir() + "/records.sqlite"
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	now := time.Now()
	var until int64
	for i := 0; i < 3; i++ {
		until, err = s.ProbeResult(0, "2001:db8::a", true, now, cfg)
		if err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, err := s.ProbeCooldown("2001:db8::a", now); err != nil || got != until {
		t.Fatal("restart changed fixed cooldown", got, until, err)
	}
}

func TestSSHHealthDoesNotCreateProbeCooldown(t *testing.T) {
	cfg := testConfig(t)
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
	for i := 0; i < 4; i++ {
		if err = health(cfg); err != nil {
			t.Fatal(err)
		}
	}
	if until, err := store.ProbeCooldown("127.0.0.1", time.Now()); err != nil || until != 0 {
		t.Fatal("health was flagged as probe", until, err)
	}
}
