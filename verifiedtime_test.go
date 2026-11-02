package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestVerifiedTimeDoesNotCountSilentTailOrOldTraces(t *testing.T) {
	trace := SessionTrace{VerificationVersion: 1, AuthenticatedAt: 1000, PostAuthMS: 10000}
	updateVerifiedTime(&trace)
	if trace.VerifiedPostAuthMS != 0 || trace.UnverifiedPostAuthMS != 10000 {
		t.Fatal(trace)
	}
	trace.LastVerifiedAt = 4000
	updateVerifiedTime(&trace)
	if trace.VerifiedPostAuthMS != 3000 || trace.UnverifiedPostAuthMS != 7000 {
		t.Fatal(trace)
	}
	trace.PostAuthMS = 20000
	updateVerifiedTime(&trace)
	if trace.VerifiedPostAuthMS != 3000 || trace.UnverifiedPostAuthMS != 17000 {
		t.Fatal("silent time grew verified total", trace)
	}
	trace.LastVerifiedAt = 40000
	updateVerifiedTime(&trace)
	if trace.VerifiedPostAuthMS != 20000 || trace.UnverifiedPostAuthMS != 0 {
		t.Fatal(trace)
	}
	old := SessionTrace{AuthenticatedAt: 1000, PostAuthMS: 10000, LastVerifiedAt: 5000}
	updateVerifiedTime(&old)
	if old.VerifiedPostAuthMS != 0 {
		t.Fatal("backfilled old record", old)
	}
}

func TestVerifiedTotalPersistsAndDeduplicatesAllLiveConnections(t *testing.T) {
	s := testStore(t)
	var live []liveDelay
	for i := 0; i < 14; i++ {
		id := mustStart(t, s, "192.0.2.1", time.Now())
		if err := s.saveSessionTrace(id, SessionTrace{VerificationVersion: 1, VerifiedPostAuthMS: 100}); err != nil {
			t.Fatal(err)
		}
		live = append(live, liveDelay{ID: id, VerifiedPostAuthMS: 200}, liveDelay{ID: id, VerifiedPostAuthMS: 150})
	}
	old := mustStart(t, s, "192.0.2.2", time.Now())
	if err := s.saveSessionTrace(old, SessionTrace{LastVerifiedAt: 99999999, PostAuthMS: 9999999}); err != nil {
		t.Fatal(err)
	}
	live = append(live, liveDelay{ID: 999999, VerifiedPostAuthMS: 999999})
	got, err := s.TotalVerifiedPostAuth(context.Background(), live)
	if err != nil || got != 2800 {
		t.Fatal(got, err)
	}
	got, err = s.TotalVerifiedPostAuth(context.Background(), nil)
	if err != nil || got != 1400 {
		t.Fatal(got, err)
	}
}

func TestSilentAuthOnlyClientIsVerifiedImmediatelyAndRetired(t *testing.T) {
	cfg := testConfig(t)
	cfg.Delay = 20 * time.Millisecond
	cfg.SSHKeepaliveInterval = time.Hour
	cfg.SSHKeepaliveTimeout = 30 * time.Millisecond
	cfg.SSHKeepaliveCountMax = 2
	s, store, _, _ := silentSSHPeer(t, cfg)
	eventually(t, func() bool { return s.stats.active.Load() == 0 })
	rows, err := store.List(context.Background(), RecordFilter{Status: "simulated_closed"})
	if err != nil || len(rows.Items) != 1 {
		t.Fatal(rows, err)
	}
	row, _, err := store.Detail(context.Background(), rows.Items[0].ID)
	if err != nil || row.Reason != "ssh_keepalive_timeout" || row.Session == nil || row.Session.VerifiedPostAuthMS != 0 || row.Session.UnverifiedPostAuthMS <= 0 {
		t.Fatal(row, err)
	}
}

func TestVerifiedTimeFreezesWhenResponsiveSSHClientGoesSilent(t *testing.T) {
	cfg := testConfig(t)
	cfg.Delay = 20 * time.Millisecond
	cfg.SSHKeepaliveInterval = 30 * time.Millisecond
	cfg.SSHKeepaliveTimeout = 300 * time.Millisecond
	cfg.SSHKeepaliveCountMax = 1
	var answered atomic.Int32
	s, store, _, _ := silentSSHPeer(t, cfg, func(requests <-chan *ssh.Request) {
		for r := range requests {
			if answered.Load() < 2 && r.Type == "keepalive@openssh.com" && r.WantReply {
				if r.Reply(answered.Load() == 0, nil) == nil {
					answered.Add(1)
				}
			}
		}
	})
	var id int64
	eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		for c := range s.clients {
			id = c.recordID
		}
		if id == 0 || answered.Load() != 2 {
			return false
		}
		for c := range s.clients {
			if c.peer.pending.Load() {
				return false
			}
		}
		return true
	})
	a, ok := s.ConnectionDetail(id)
	if !ok || a.Session == nil || a.Session.VerifiedPostAuthMS <= 0 {
		t.Fatal(a, ok)
	}
	time.Sleep(80 * time.Millisecond)
	b, ok := s.ConnectionDetail(id)
	if !ok || b.Session.VerifiedPostAuthMS != a.Session.VerifiedPostAuthMS || b.Session.UnverifiedPostAuthMS <= a.Session.UnverifiedPostAuthMS {
		t.Fatal("silent tail was counted as confirmed", a, b)
	}
	eventually(t, func() bool { return s.stats.active.Load() == 0 })
	row, _, err := store.Detail(context.Background(), id)
	if err != nil || row.Reason != "ssh_keepalive_timeout" || row.Session.VerifiedPostAuthMS != b.Session.VerifiedPostAuthMS {
		t.Fatal(row, err)
	}
	got, err := store.TotalVerifiedPostAuth(context.Background(), nil)
	if err != nil || got != row.Session.VerifiedPostAuthMS {
		t.Fatal("checkpoint not persisted", got, row.Session, err)
	}
}

func TestSilentPeerCannotExemptItselfForeverWithLocalDelay(t *testing.T) {
	cfg := testConfig(t)
	cfg.Delay = 20 * time.Millisecond
	cfg.WriteTimeout = 20 * time.Millisecond
	cfg.SSHKeepaliveInterval = time.Hour
	cfg.SSHKeepaliveTimeout = 30 * time.Millisecond
	cfg.SSHKeepaliveCountMax = 2
	received := make(chan struct{}, 1)
	s, store, _, _ := silentSSHPeer(t, cfg, func(requests <-chan *ssh.Request) {
		for range requests {
			select {
			case received <- struct{}{}:
			default:
			}
		}
	})
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("initial check was not sent")
	}
	// Model an ongoing local response delay after the heartbeat reached the
	// client. The ordinary response windows exclude it; the hard limit must not.
	s.mu.Lock()
	var id int64
	for c := range s.clients {
		c.delayStarted = time.Now()
		id = c.recordID
	}
	s.mu.Unlock()
	if id == 0 {
		t.Fatal("client disappeared before local delay")
	}
	eventually(t, func() bool { return s.stats.active.Load() == 0 })
	row, _, err := store.Detail(context.Background(), id)
	if err != nil || row.Reason != "ssh_keepalive_timeout" || row.Session == nil || row.Session.PostAuthMS > 2000 {
		t.Fatal(row, err)
	}
}
