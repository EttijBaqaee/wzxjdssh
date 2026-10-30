package main

import (
	"context"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func simulatedTestServer(t *testing.T, cfg Config) (*Server, *Store, *ssh.Client) {
	t.Helper()
	store := testStore(t)
	version := "SSH-2.0-network-jitter-test"
	id := mustStart(t, store, "127.0.0.1", time.Now())
	if _, err := store.MarkInterruptedClient(id, "127.0.0.1", version, time.Now()); err != nil {
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
	t.Cleanup(s.Stop)
	c, err := ssh.Dial("tcp", address(cfg), &ssh.ClientConfig{User: "virtual-user", ClientVersion: version, Auth: []ssh.AuthMethod{ssh.Password("virtual-password")}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return s, store, c
}

func TestSimulatedNetworkRefusesForwardingAndSubsystems(t *testing.T) {
	cfg := testConfig(t)
	cfg.Delay = 40 * time.Millisecond
	s, _, c := simulatedTestServer(t, cfg)
	if bad, _, err := c.OpenChannel("session", []byte{1}); err == nil {
		bad.Close()
		t.Fatal("malformed session open accepted")
	}
	session, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if extra, err := c.NewSession(); err == nil {
		extra.Close()
		t.Fatal("second live session allowed")
	}
	if forwarded, err := c.Dial("tcp", "127.0.0.1:9"); err == nil {
		forwarded.Close()
		t.Fatal("TCP forwarding allowed")
	}
	if ok, _, err := c.SendRequest("tcpip-forward", true, ssh.Marshal(struct {
		Host string
		Port uint32
	}{"127.0.0.1", 2224})); err != nil || ok {
		t.Fatal("remote forwarding allowed or transport closed", ok, err)
	}
	if err := session.RequestSubsystem("sftp"); err == nil {
		t.Fatal("SFTP accepted")
	}
	if err := session.Setenv("PATH", "/must/not/be/used"); err != nil {
		t.Fatal(err)
	}
	if ok, err := session.SendRequest("pty-req", true, []byte("invalid")); err != nil || ok {
		t.Fatal("malformed PTY accepted or transport closed", ok, err)
	}
	if err := session.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err := session.Shell(); err != nil {
		t.Fatal("inert shell request failed", err)
	}
	if ok, _, err := c.SendRequest("keepalive@openssh.com", true, nil); err != nil || ok {
		t.Fatal("transport did not remain alive", ok, err)
	}
	live := s.Snapshot()["live"].(LiveStats)
	if len(live.LongestConnections) != 1 || live.LongestConnections[0].DelayedMS == 0 {
		t.Fatal("network waits missing from live ranking", live)
	}
}

func TestSimulatedNetworkIgnoresAuthIdleAndLegacyLifetime(t *testing.T) {
	cfg := testConfig(t)
	cfg.Delay = 40 * time.Millisecond
	cfg.IdleTimeout = 100 * time.Millisecond
	cfg.MaxConnectionTime = 100 * time.Millisecond
	s, store, c := simulatedTestServer(t, cfg)
	time.Sleep(350 * time.Millisecond)
	if _, _, err := c.SendRequest("keepalive@openssh.com", true, nil); err != nil {
		t.Fatal("auth idle timeout closed simulated link", err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case err := <-done:
		t.Fatal("legacy lifetime released simulated connection", err)
	case <-time.After(100 * time.Millisecond):
	}
	c.Close()
	<-done
	eventually(t, func() bool { return s.stats.active.Load() == 0 })
	page, err := store.List(context.Background(), RecordFilter{Status: "simulated_closed"})
	if err != nil || page.Total != 1 || page.Items[0].Reason == "max_connection_time" {
		t.Fatal("simulated peer close not retained", page, err)
	}
}

func TestSimulatedNetworkShutdownCancelsLongDelay(t *testing.T) {
	cfg := testConfig(t)
	cfg.Delay = time.Hour
	s, _, c := simulatedTestServer(t, cfg)
	done := make(chan error, 1)
	go func() {
		session, err := c.NewSession()
		if session != nil {
			session.Close()
		}
		done <- err
	}()
	eventually(t, func() bool { return s.Snapshot()["live"].(LiveStats).DelayingConnections == 1 })
	started := time.Now()
	s.Stop()
	if time.Since(started) > time.Second {
		t.Fatal("shutdown waited for simulated delay")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("pending virtual session survived shutdown")
		}
	case <-time.After(time.Second):
		t.Fatal("session waiter leaked after shutdown")
	}
}
