package main

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestHeldConnectionsCountBetweenResponses(t *testing.T) {
	s := &Server{clients: map[*clientConn]struct{}{}}
	now := time.Now()
	add := func(ip string, delay time.Duration, waiting, simulated, probe bool) {
		a, b := net.Pipe()
		t.Cleanup(func() { a.Close(); b.Close() })
		c := &clientConn{Conn: a, ip: ip, started: now.Add(-time.Hour), delayed: delay}
		if waiting {
			c.delayStarted = now.Add(-time.Second)
		}
		c.simulated.Store(simulated)
		if probe {
			c.probeUntil = now.Add(time.Hour).UnixMilli()
		}
		s.clients[c] = struct{}{}
	}
	add("2001:db8::1", time.Second, false, false, false)
	add("2001:db8::1", 0, true, false, false)
	add("2001:db8::2", 0, false, true, false)
	add("2001:db8::3", 0, false, false, false)
	add("2001:db8::4", 0, true, false, true)
	live := s.liveSnapshotLocked(now)
	if live.HeldIPs != 2 || live.HeldConnections != 3 || live.DelayingConnections != 2 || live.DelayingIPs != 2 || live.ActiveIPs != 4 {
		t.Fatal(live)
	}
}

func TestDelayTotalDeduplicatesAllConnectionsAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.sqlite")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now()
	var samples []liveDelay
	var want int64
	for i := 1; i <= 15; i++ {
		id := mustStart(t, s, fmt.Sprint("192.0.2.", i), now)
		ms := int64(i * 1000)
		if err = s.finish(id, time.Hour, "closed", "peer_closed", 0, 0, time.Duration(ms)*time.Millisecond); err != nil {
			t.Fatal(err)
		}
		want += ms
		samples = append(samples, liveDelay{ID: id, DelayedMS: ms / 2})
	}
	active := mustStart(t, s, "2001:db8::5", now)
	samples = append(samples, liveDelay{ID: active, DelayedMS: 2500}, liveDelay{ID: active, DelayedMS: 2000}, liveDelay{ID: 999999, DelayedMS: 999999999})
	if got, err := s.TotalDelay(ctx, samples); err != nil || got != want+2500 {
		t.Fatal("snapshot overlap or top10 truncation", got, want, err)
	}
	if err = s.finish(active, time.Hour, "closed", "peer_closed", 0, 0, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if got, err := s.TotalDelay(ctx, samples); err != nil || got != want+3000 {
		t.Fatal("closing transition double counted", got, err)
	}
	s.Close()
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, err := s.TotalDelay(ctx, nil); err != nil || got != want+3000 {
		t.Fatal("restart lost real delay", got, err)
	}
	if err = s.Prune(now.Add(100*24*time.Hour), 90); err != nil {
		t.Fatal(err)
	}
	if got, err := s.TotalDelay(ctx, samples); err != nil || got != 0 {
		t.Fatal("deleted records resurrected by old snapshot", got, err)
	}
}

func TestAuthenticationAndAdmissionRejectionsAreSeparate(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	id := mustStart(t, s, "192.0.2.1", now)
	for _, result := range []string{"rejected", "rejected", "interrupted", "simulated_success", "pending"} {
		a := mustAttempt(t, s, id, now, "test-user", "password")
		if err := s.AttemptResult(a, result); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Reject("192.0.2.2:1234", "192.0.2.2", "ssh", "rate_limit"); err != nil {
		t.Fatal(err)
	}
	probe := mustStart(t, s, "192.0.2.3", now)
	if err := s.Finish(probe, time.Second, "probe_blocked", "probe_cooldown", 0, 0); err != nil {
		t.Fatal(err)
	}
	stats, err := s.Stats(context.Background())
	if err != nil || stats.AuthRejected != 2 || stats.Rejected != 1 || stats.ProbeBlocked != 1 {
		t.Fatal(stats, err)
	}
}
