package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestClientCooldownExpiresAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.sqlite")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	id := mustStart(t, store, "2001:db8::1", at)
	version := "SSH-2.0-cooldown-check"
	until, err := store.MarkInterruptedClient(id, "2001:db8::1", version, at)
	if err != nil || until != at.Add(time.Hour).UnixMilli() {
		t.Fatal(until, err)
	}
	if again, err := store.MarkInterruptedClient(id, "2001:db8::1", version, at.Add(time.Minute)); err != nil || again != until {
		t.Fatal("concurrent interruption extended active cooldown", again, err)
	}
	if err = store.MarkReturningClient(id, until); err != nil {
		t.Fatal(err)
	}
	if err = store.RecordSimulatedSession(id, until); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		ip, version string
		now         time.Time
		want        int64
	}{{"2001:db8::1", version, at.Add(59 * time.Minute), until}, {"2001:db8::1", version, at.Add(time.Hour), 0}, {"2001:db8::2", version, at, 0}, {"2001:db8::1", "SSH-2.0-other", at, 0}} {
		got, err := store.ClientCooldown(tc.ip, tc.version, tc.now)
		if err != nil || got != tc.want {
			t.Fatal("cooldown mismatch", got, tc.want, err)
		}
	}
	store.Close()
	store, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if got, err := store.ClientCooldown("2001:db8::1", version, at.Add(30*time.Minute)); err != nil || got != until {
		t.Fatal("restart lost cooldown", got, err)
	}
	if err = store.Prune(at.Add(time.Hour), 90); err != nil {
		t.Fatal(err)
	}
	if got, err := store.ClientCooldown("2001:db8::1", version, at); err != nil || got != 0 {
		t.Fatal("expired mark not pruned", got, err)
	}
	record, _, err := store.Detail(context.Background(), id)
	if err != nil || record.ClientMark != "simulated_network" || record.MarkExpiresAt != until {
		t.Fatal("historical label lost", record, err)
	}
}

func TestInterruptedClientGetsSimulatedNetworkUntilCooldown(t *testing.T) {
	cfg := testConfig(t)
	cfg.Delay = 300 * time.Millisecond
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
	version := "SSH-2.0-cooldown-client"
	clientCfg := &ssh.ClientConfig{User: "cooldown-user", ClientVersion: version, Auth: []ssh.AuthMethod{ssh.Password("cooldown-password")}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: time.Second}
	raw, err := net.Dial("tcp", address(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	firstDone := make(chan struct{})
	go func() {
		c, _, _, _ := ssh.NewClientConn(raw, address(cfg), clientCfg)
		if c != nil {
			c.Close()
		}
		close(firstDone)
	}()
	eventually(t, func() bool { return s.Snapshot()["live"].(LiveStats).DelayingConnections == 1 })
	raw.Close()
	<-firstDone
	var until int64
	eventually(t, func() bool {
		until, err = store.ClientCooldown("127.0.0.1", version, time.Now())
		return err == nil && until > 0 && s.stats.active.Load() == 0
	})
	page, err := store.List(context.Background(), RecordFilter{})
	if err != nil || len(page.Items) != 1 || page.Items[0].ClientMark != "auth_interrupted" {
		t.Fatal("interruption not labelled", page, err)
	}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(key)
	methods := []ssh.AuthMethod{ssh.Password("returned-password"), ssh.KeyboardInteractive(func(_, _ string, q []string, _ []bool) ([]string, error) { return []string{"returned-answer"}, nil }), ssh.PublicKeys(signer)}
	for _, method := range methods {
		next := *clientCfg
		next.Auth = []ssh.AuthMethod{method}
		started := time.Now()
		c, err := ssh.Dial("tcp", address(cfg), &next)
		if err != nil {
			t.Fatal("marked client did not receive auth success", err)
		}
		if time.Since(started) >= cfg.Delay {
			t.Fatal("marked client unnecessarily delayed")
		}
		started = time.Now()
		session, err := c.NewSession()
		if err != nil {
			c.Close()
			t.Fatal("virtual session was closed immediately", err)
		}
		if time.Since(started) < cfg.Delay/20 {
			t.Fatal("channel confirmation did not simulate network delay")
		}
		stdout, err := session.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err = session.Start("echo WZXJDSSH-MUST-NOT-EXECUTE"); err != nil {
			t.Fatal(err)
		}
		output := make(chan []byte, 1)
		go func() { data, _ := io.ReadAll(stdout); output <- data }()
		select {
		case data := <-output:
			t.Fatal("virtual command returned or disconnected", string(data))
		case <-time.After(100 * time.Millisecond):
		}
		active, err := store.List(context.Background(), RecordFilter{Status: "simulated_network"})
		if err != nil || active.Total != 1 || active.Items[0].EndedAt != nil {
			t.Fatal("virtual session not shown as active", active, err)
		}
		c.Close()
		if data := <-output; len(data) != 0 {
			t.Fatal("virtual command produced system output", string(data))
		}
		eventually(t, func() bool { return s.stats.active.Load() == 0 })
		got, err := store.ClientCooldown("127.0.0.1", version, time.Now())
		if err != nil || got != until {
			t.Fatal("return visit extended cooldown", got, until, err)
		}
	}
	page, err = store.List(context.Background(), RecordFilter{Status: "simulated_closed"})
	if err != nil || page.Total != 3 {
		t.Fatal("simulated closes not recorded", page, err)
	}
	for _, record := range page.Items {
		_, attempts, err := store.Detail(context.Background(), record.ID)
		if err != nil || record.ClientMark != "simulated_network" || len(attempts) == 0 || attempts[0].Result != "simulated_success" {
			t.Fatal(record, attempts, err)
		}
	}
	if _, err = store.db.Exec(`UPDATE client_cooldowns SET expires_at=?`, time.Now().Add(-time.Second).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	c, err := ssh.Dial("tcp", address(cfg), clientCfg)
	if c != nil {
		c.Close()
	}
	if err == nil || time.Since(started) < cfg.Delay {
		t.Fatal("expired mark did not restore delayed rejection", err)
	}
}
