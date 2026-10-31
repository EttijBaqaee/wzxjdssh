package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "records.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func mustStart(t *testing.T, s *Store, ip string, at time.Time) int64 {
	t.Helper()
	id, err := s.Start(ip+":1234", ip, "ssh", at)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func mustAttempt(t *testing.T, s *Store, id int64, at time.Time, username, method string) int64 {
	t.Helper()
	a, err := s.Attempt(AuthRecord{ConnectionID: id, At: at.UnixMilli(), Username: username, Method: method, Credential: "test-secret", Encoding: "utf8"})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestStorePersistenceAndRestartRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "records.sqlite")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	id := mustStart(t, s, "192.0.2.1", time.Now().Add(-time.Second))
	mustAttempt(t, s, id, time.Now(), "root", "password")
	if err = s.Transport(id, "SSH-2.0-test-client"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, a, err := s.Detail(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != "interrupted" || c.Reason != "process_restart" || c.EndedAt == nil || c.DurationMS < 1000 || c.ClientVersion != "SSH-2.0-test-client" || len(a) != 1 || a[0].Credential != "test-secret" || a[0].Result != "interrupted" {
		t.Fatalf("restart lost data: %+v %+v", c, a)
	}
}

func TestStoreRetentionAndStatistics(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	old := now.Add(-91 * 24 * time.Hour)
	recent := now.Add(-2 * 24 * time.Hour)
	oldID := mustStart(t, s, "192.0.2.9", old)
	mustAttempt(t, s, oldID, old, "old", "password")
	if err := s.Finish(oldID, time.Second, "closed", "peer_closed", 1, 2); err != nil {
		t.Fatal(err)
	}
	active := mustStart(t, s, "192.0.2.1", recent)
	a := mustAttempt(t, s, active, recent, "root", "password")
	if err := s.AttemptResult(a, "rejected"); err != nil {
		t.Fatal(err)
	}
	id := mustStart(t, s, "192.0.2.1", now)
	mustAttempt(t, s, id, now, "root", "publickey_probe")
	if err := s.Finish(id, time.Second, "auth_rejected", "authentication_failed", 100, 200); err != nil {
		t.Fatal(err)
	}
	if err := s.Reject("192.0.2.2:2345", "192.0.2.2", "ssh", "max_clients"); err != nil {
		t.Fatal(err)
	}
	if err := s.Prune(now, 90); err != nil {
		t.Fatal(err)
	}
	stats, err := s.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Connections != 3 || stats.Active != 1 || stats.Attempts != 2 || stats.UniqueIPs != 2 || stats.Rejected != 1 || stats.Today != 2 || len(stats.Daily) != 30 || stats.Usernames[0].Name != "root" || stats.Usernames[0].Count != 2 {
		t.Fatalf("bad stats: %+v", stats)
	}
	if stats.Daily[27].Connections != 1 || stats.Daily[27].Attempts != 1 || stats.Daily[29].Connections != 2 || stats.Daily[29].Attempts != 1 {
		t.Fatalf("bad UTC trend: %+v", stats.Daily)
	}
	page, err := s.List(context.Background(), RecordFilter{Page: 1, Limit: 1, Username: "root"})
	if err != nil || page.Total != 2 || len(page.Items) != 1 || page.Items[0].ID != id {
		t.Fatalf("pagination: %+v %v", page, err)
	}
	page, err = s.List(context.Background(), RecordFilter{Username: "' OR 1=1 --"})
	if err != nil || page.Total != 0 {
		t.Fatal("unsafe filter", err, page)
	}
	if _, attempts, err := s.Detail(context.Background(), active); err != nil || len(attempts) != 1 {
		t.Fatal("active data removed", err)
	}
	if err := s.Finish(active, time.Second, "closed", "peer_closed", 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Prune(now, 1); err != nil {
		t.Fatal(err)
	}
	stats, err = s.Stats(context.Background())
	if err != nil || stats.Connections != 2 || stats.Attempts != 1 {
		t.Fatalf("custom retention: %+v %v", stats, err)
	}
	var orphan int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM auth_attempts WHERE connection_id=?`, oldID).Scan(&orphan); err != nil || orphan != 0 {
		t.Fatal("orphaned credentials", orphan, err)
	}
}

func TestCredentialEncodingAndBounds(t *testing.T) {
	value, encoding, cut := credentialBytes([]byte{0xff, 0, 1})
	if encoding != "base64" || cut {
		t.Fatal(value, encoding, cut)
	}
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(raw) != 3 || raw[0] != 0xff {
		t.Fatal("binary credential corrupted")
	}
	value, _, cut = credentialBytes([]byte(strings.Repeat("x", 70000)))
	if !cut || len(value) != 65536 {
		t.Fatal("credential bound")
	}
	s := testStore(t)
	id := mustStart(t, s, "192.0.2.1", time.Now())
	mustAttempt(t, s, id, time.Now(), strings.Repeat("中", 2000), "password")
	_, a, err := s.Detail(context.Background(), id)
	if err != nil || !a[0].Truncated || len(a[0].Username) > 4096 {
		t.Fatal("username bound", err)
	}
}

func TestSSHAuthenticationRecorded(t *testing.T) {
	s := testStore(t)
	c := testConfig(t)
	log := &synchronizedBuffer{}
	server, err := NewServer(c, log)
	if err != nil {
		t.Fatal(err)
	}
	server.store = s
	if err = server.Start(); err != nil {
		t.Fatal(err)
	}
	defer server.Stop()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(key)
	methods := []ssh.AuthMethod{ssh.Password("captured-password"), ssh.PublicKeys(signer), ssh.KeyboardInteractive(func(_, _ string, _ []string, _ []bool) ([]string, error) {
		return []string{"interactive-password"}, nil
	})}
	for i, method := range methods {
		conn, _ := ssh.Dial("tcp", address(c), &ssh.ClientConfig{User: fmt.Sprint("user", i), Auth: []ssh.AuthMethod{method}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 2 * time.Second})
		if conn != nil {
			conn.Close()
			t.Fatal("authenticated unexpectedly")
		}
	}
	eventually(t, func() bool {
		stats, err := s.Stats(context.Background())
		return err == nil && stats.Active == 0 && stats.Attempts == 3
	})
	page, err := s.List(context.Background(), RecordFilter{})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]AuthRecord{}
	for _, row := range page.Items {
		conn, a, err := s.Detail(context.Background(), row.ID)
		if err != nil || len(a) != 1 || conn.ClientVersion == "" || conn.BytesReceived == 0 || conn.BytesSent == 0 || conn.Status != "auth_rejected" || a[0].Result != "rejected" {
			t.Fatalf("missing record: %+v %+v %v", conn, a, err)
		}
		seen[a[0].Method] = a[0]
	}
	if seen["password"].Credential != "captured-password" || seen["keyboard-interactive"].Credential != `["interactive-password"]` || seen["publickey_probe"].Fingerprint != ssh.FingerprintSHA256(signer.PublicKey()) || seen["publickey_probe"].PublicKey == "" {
		t.Fatalf("incorrect credentials: %+v", seen)
	}
	if strings.Contains(log.String(), "captured-password") || strings.Contains(log.String(), "interactive-password") {
		t.Fatal("credential leaked into stdout")
	}
}

func TestRecordedHandshakeTimeout(t *testing.T) {
	store := testStore(t)
	c := testConfig(t)
	c.HandshakeTimeout = 80 * time.Millisecond
	s, err := NewServer(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.store = store
	if err = s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	conn := connect(t, c)
	bannerLine(t, conn)
	defer conn.Close()
	eventually(t, func() bool {
		p, e := store.List(context.Background(), RecordFilter{})
		return e == nil && len(p.Items) == 1 && p.Items[0].Status == "probe_detected" && p.Items[0].Reason == "no_credentials" && p.Items[0].ClientMark == ""
	})
}

func TestSSHUsernameWithoutCredential(t *testing.T) {
	store := testStore(t)
	c := testConfig(t)
	s, err := NewServer(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.store = store
	if err = s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	conn, _ := ssh.Dial("tcp", address(c), &ssh.ClientConfig{User: "username-only", HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: time.Second})
	if conn != nil {
		conn.Close()
	}
	eventually(t, func() bool {
		p, e := store.List(context.Background(), RecordFilter{Username: "username-only"})
		return e == nil && p.Total == 1 && p.Items[0].Username == "username-only" && p.Items[0].AuthAttempts == 0
	})
}

func TestSnapshotShowsLiveDelayStrategy(t *testing.T) {
	c := testConfig(t)
	s := startTest(t, c)
	policy := s.Snapshot()["strategy"].(map[string]any)
	if policy["mode"] != "ssh" || policy["delay_ms"] != c.Delay.Milliseconds() || policy["max_auth_tries"] != c.MaxAuthTries || policy["client_cooldown_ms"] != int64(3600000) {
		t.Fatal("incorrect strategy", policy)
	}
	c.Delay = 75 * time.Millisecond
	c.MaxAuthTries = 3
	if err := s.Reload(c); err != nil {
		t.Fatal(err)
	}
	policy = s.Snapshot()["strategy"].(map[string]any)
	if policy["delay_ms"] != int64(75) || policy["max_auth_tries"] != 3 {
		t.Fatal("strategy did not follow configuration reload", policy)
	}
}

func TestStoreWebPreferencesSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.sqlite")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetWebIdleMinutes(context.Background(), 15); err != nil {
		t.Fatal(err)
	}
	if err = store.Prune(time.Now(), 90); err != nil {
		t.Fatal(err)
	}
	store.Close()
	store, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if minutes, err := store.WebIdleMinutes(); err != nil || minutes != 15 {
		t.Fatal("web preference lost", minutes, err)
	}
}

func TestStoreSchemaUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.sqlite")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	id := mustStart(t, s, "192.0.2.1", time.Now())
	mustAttempt(t, s, id, time.Now(), "upgrade-user", "password")
	if _, err = s.db.Exec(`ALTER TABLE connections DROP COLUMN username; PRAGMA user_version=1;`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Username(id, "upgrade-user"); err != nil {
		t.Fatal(err)
	}
	c, a, err := s.Detail(context.Background(), id)
	if err != nil || c.Username != "upgrade-user" || len(a) != 1 || a[0].Credential != "test-secret" {
		t.Fatal("schema upgrade lost data", c, a, err)
	}
	if _, err = s.db.Exec(`PRAGMA user_version=999`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if future, err := OpenStore(path); err == nil {
		future.Close()
		t.Fatal("newer schema accepted")
	}
}
