package main

import (
	"context"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestParallelIPOverridesCooldownAndDoesNotGetTimeoutMark(t *testing.T) {
	cfg := testConfig(t)
	cfg.Delay = 400 * time.Millisecond
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
	version := "SSH-2.0-parallel-test"
	id := mustStart(t, store, "127.0.0.1", time.Now())
	if _, err = store.MarkInterruptedClient(id, "127.0.0.1", version, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = store.Finish(id, time.Second, "closed", "", 0, 0); err != nil {
		t.Fatal(err)
	}
	// First transport is already online when the second connection arrives.
	first, err := net.Dial("tcp", address(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	eventually(t, func() bool { return s.stats.active.Load() == 1 })
	second, err := net.Dial("tcp", address(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.clients) != 2 {
			return false
		}
		for c := range s.clients {
			if !c.forceSlow.Load() {
				return false
			}
		}
		return true
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, _, _, _ := ssh.NewClientConn(second, address(cfg), &ssh.ClientConfig{User: "parallel", ClientVersion: version, Auth: []ssh.AuthMethod{ssh.Password("parallel-password")}, HostKeyCallback: ssh.InsecureIgnoreHostKey()})
		if conn != nil {
			conn.Close()
		}
	}()
	eventually(t, func() bool { return s.Snapshot()["live"].(LiveStats).DelayingConnections == 1 })
	second.Close()
	<-done
	first.Close()
	eventually(t, func() bool { return s.stats.active.Load() == 0 })
	if until, err := store.ClientCooldown("127.0.0.1", version, time.Now()); err != nil || until != 0 {
		t.Fatal("parallel became timeout client", until, err)
	}
	page, err := store.List(context.Background(), RecordFilter{Username: "parallel"})
	if err != nil || page.Total != 1 || page.Items[0].ClientMark != "parallel_slow" || page.Items[0].Status == "auth_interrupted" {
		t.Fatal(page, err)
	}
	if _, err = store.db.Exec(`UPDATE ip_slowdowns SET expires_at=?`, time.Now().Add(-time.Second).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if until, err := store.ParallelUntil("127.0.0.1", time.Now()); err != nil || until != 0 {
		t.Fatal("parallel expiry", until, err)
	}
}

func TestUnlimitedAuthenticationSurvivesMoreThanSixFailures(t *testing.T) {
	c := testConfig(t)
	c.Delay = 5 * time.Millisecond
	s := startTest(t, c)
	attempts := 0
	_, err := ssh.Dial("tcp", address(c), &ssh.ClientConfig{User: "retry", Auth: []ssh.AuthMethod{ssh.RetryableAuthMethod(ssh.PasswordCallback(func() (string, error) { attempts++; return "retry-password", nil }), 12)}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: time.Second})
	if err == nil || attempts != 12 || s.stats.authAttempts.Load() < 12 {
		t.Fatal("authentication capped early", attempts, s.stats.authAttempts.Load(), err)
	}
}
