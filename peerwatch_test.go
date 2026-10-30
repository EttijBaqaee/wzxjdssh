package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// Unlike ssh.NewClient, this client deliberately does not answer incoming SSH
// requests. Its TCP socket and SSH packet reader remain open and healthy.
func silentSSHPeer(t *testing.T, cfg Config) (*Server, *Store, ssh.Conn, *net.TCPConn) {
	t.Helper()
	store := testStore(t)
	const ident = "SSH-2.0-silent-peer-test"
	id := mustStart(t, store, "127.0.0.1", time.Now())
	if _, err := store.MarkInterruptedClient(id, "127.0.0.1", ident, time.Now()); err != nil {
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
	raw, err := net.DialTimeout("tcp", address(cfg), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	conn, _, _, err := ssh.NewClientConn(raw, address(cfg), &ssh.ClientConfig{User: "silent-user", ClientVersion: ident, Auth: []ssh.AuthMethod{ssh.Password("synthetic-password")}, HostKeyCallback: ssh.InsecureIgnoreHostKey()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return s, store, conn, raw.(*net.TCPConn)
}

func TestSilentSSHPeerReclaimedWithoutSuccessfulLearning(t *testing.T) {
	cfg := testConfig(t)
	cfg.Delay = 40 * time.Millisecond
	cfg.SSHKeepaliveInterval = time.Hour
	cfg.SSHKeepaliveTimeout = 50 * time.Millisecond
	cfg.SSHKeepaliveCountMax = 2
	s, store, conn, _ := silentSSHPeer(t, cfg)
	channel, _, err := conn.OpenChannel("session", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := channel.SendRequest("exec", true, ssh.Marshal(struct{ Command string }{"DO-NOT-EXECUTE"})); err != nil || !ok {
		t.Fatal(ok, err)
	}
	eventually(t, func() bool { return s.stats.active.Load() == 0 })
	page, err := store.List(context.Background(), RecordFilter{Status: "simulated_closed"})
	if err != nil || len(page.Items) != 1 {
		t.Fatal(page, err)
	}
	row, _, err := store.Detail(context.Background(), page.Items[0].ID)
	if err != nil || row.Reason != "ssh_keepalive_timeout" || row.Session == nil || row.Session.EndedAt == 0 || row.Session.LastVerifiedAt != 0 {
		t.Fatal(row, err)
	}
	for _, e := range row.Session.Events {
		if e.Stage == "session_held" {
			t.Fatal("silent transport was counted as a caught client")
		}
	}
	if row.BytesReceived == 0 || row.BytesSent == 0 {
		t.Fatal("final wire counters lost", row)
	}
}

func TestSSHKeepaliveRejectIsAliveAndLocalDelayIsExcluded(t *testing.T) {
	cfg := testConfig(t)
	cfg.Delay = 800 * time.Millisecond
	cfg.SSHKeepaliveInterval = 20 * time.Millisecond
	cfg.SSHKeepaliveTimeout = 15 * time.Millisecond
	cfg.SSHKeepaliveCountMax = 2
	s, _, client := simulatedTestServer(t, cfg)
	eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		for c := range s.clients {
			if c.peer.verifiedAt.Load() > 0 {
				return true
			}
		}
		return false
	})
	if s.stats.active.Load() != 1 {
		t.Fatal("local artificial delay was treated as a missing peer reply")
	}
	client.Close()
	eventually(t, func() bool { return s.stats.active.Load() == 0 })
}

func TestSimulatedFINAndRSTReleaseConnections(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run(strconv.FormatBool(reset), func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Delay = 20 * time.Millisecond
			s, store, _, raw := silentSSHPeer(t, cfg)
			if reset {
				if err := raw.SetLinger(0); err != nil {
					t.Fatal(err)
				}
			}
			raw.Close()
			eventually(t, func() bool { return s.stats.active.Load() == 0 })
			page, err := store.List(context.Background(), RecordFilter{Status: "simulated_closed"})
			if err != nil || len(page.Items) != 1 || page.Items[0].EndedAt == nil || page.Items[0].Reason == "ssh_keepalive_timeout" {
				t.Fatal(page, err)
			}
		})
	}
}

func TestOnlineDetailHasLiveBytesLivenessAndElapsedTime(t *testing.T) {
	cfg := testConfig(t)
	cfg.Delay = 20 * time.Millisecond
	s, store, client := simulatedTestServer(t, cfg)
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err = session.Start("DO-NOT-EXECUTE"); err != nil {
		t.Fatal(err)
	}
	var id int64
	eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		for c := range s.clients {
			if c.peer.verifiedAt.Load() > 0 {
				id = c.recordID
				return true
			}
		}
		return false
	})
	w, err := NewWeb(RuntimeConfig{AdminPassword: "synthetic-admin-password"}, store, s.Snapshot, s.ConnectionDetail)
	if err != nil {
		t.Fatal(err)
	}
	get := func() ConnectionRecord {
		r := httptest.NewRequest("GET", "/api/connections/"+strconv.FormatInt(id, 10), nil)
		r.SetPathValue("id", strconv.FormatInt(id, 10))
		rw := httptest.NewRecorder()
		w.detail(rw, r)
		if rw.Code != 200 {
			t.Fatal(rw.Body.String())
		}
		var body struct {
			Connection ConnectionRecord `json:"connection"`
		}
		if err := json.Unmarshal(rw.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Connection
	}
	a := get()
	if a.BytesSent == 0 || a.BytesReceived == 0 || a.Liveness == nil || a.Liveness.LastVerifiedAt == 0 || a.Session == nil || a.Session.LastVerifiedAt == 0 {
		t.Fatal(a)
	}
	time.Sleep(50 * time.Millisecond)
	b := get()
	if b.Session.PostAuthMS <= a.Session.PostAuthMS || b.DurationMS <= a.DurationMS {
		t.Fatal("detail remained frozen at last protocol event", a, b)
	}
	if b.EndedAt != nil {
		t.Fatal("responsive idle session was retired")
	}
}

func TestSSHKeepaliveConfiguration(t *testing.T) {
	values := map[string]string{"WZXJDSSH_SSH_KEEPALIVE_INTERVAL_MS": "45000", "WZXJDSSH_SSH_KEEPALIVE_TIMEOUT_MS": "12000", "WZXJDSSH_SSH_KEEPALIVE_COUNT_MAX": "2"}
	c, err := configEnvironment(DefaultConfig(), func(k string) string { return values[k] })
	if err != nil || c.SSHKeepaliveInterval != 45*time.Second || c.SSHKeepaliveTimeout != 12*time.Second || c.SSHKeepaliveCountMax != 2 {
		t.Fatal(c, err)
	}
	for k, vs := range map[string][]string{"SSH_KEEPALIVE_INTERVAL_MS": {"0", "3600001"}, "SSH_KEEPALIVE_TIMEOUT_MS": {"0", "3600001"}, "SSH_KEEPALIVE_COUNT_MAX": {"0", "11"}} {
		for _, v := range vs {
			if _, err := configEnvironment(DefaultConfig(), func(key string) string {
				if key == "WZXJDSSH_"+k {
					return v
				}
				return ""
			}); err == nil {
				t.Fatal(k, v)
			}
		}
	}
}
