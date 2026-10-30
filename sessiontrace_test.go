package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestShortSimulatedSessionsDoNotIncreaseAuthDelay(t *testing.T) {
	for _, kind := range []string{"authentication_only", "channel_only", "quick_exec"} {
		t.Run(kind, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Delay = 300 * time.Millisecond
			cfg.CooldownAuthDelay = 4 * time.Second
			cfg.CooldownAuthMax = 5 * time.Second
			s, store, c := simulatedTestServer(t, cfg)
			key := authDelayKey{"127.0.0.1", "SSH-2.0-network-jitter-test"}
			until, err := store.ClientCooldown(key.ip, key.version, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if kind != "authentication_only" {
				session, err := c.NewSession()
				if err != nil {
					t.Fatal(err)
				}
				if kind == "quick_exec" {
					if err = session.Start("DO-NOT-EXECUTE"); err != nil {
						t.Fatal(err)
					}
				}
			}
			if got := s.authDelays.base(key, until, time.Now(), cfg); got != 4*time.Second {
				t.Fatal("premature success increased auth delay", got)
			}
			c.Close()
			eventually(t, func() bool { return s.authDelays.base(key, until, time.Now(), cfg) == 3*time.Second })
			var trace *SessionTrace
			eventually(t, func() bool {
				page, err := store.List(context.Background(), RecordFilter{Status: "simulated_closed"})
				if err != nil || len(page.Items) != 1 {
					return false
				}
				row, _, err := store.Detail(context.Background(), page.Items[0].ID)
				trace = row.Session
				return err == nil && trace != nil && trace.EndedAt > 0
			})
			if trace.PostAuthMS < 0 || trace.LastStage == "session_held" || trace.EndError == "" {
				t.Fatal(trace)
			}
			if kind == "authentication_only" && trace.LastStage != "authenticated" {
				t.Fatal("wrong departure stage", trace)
			}
			for _, event := range trace.Events {
				if event.Stage == "session_held" {
					t.Fatal("short session counted as caught")
				}
			}
		})
	}
}

func TestSessionTraceBoundsPersistenceAndCascade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.sqlite")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	id := mustStart(t, store, "2001:db8::8", time.Now())
	s := &Server{store: store}
	c := &clientConn{server: s, recordID: id}
	c.sessionEvent("authenticated", "")
	for i := 0; i < 31; i++ {
		c.sessionEvent("request_received", "UNTRUSTED-COMMAND-OR-TYPE")
	}
	if _, err = store.db.Exec(`CREATE TEMP TRIGGER trace_write_limit BEFORE UPDATE ON simulated_sessions BEGIN SELECT RAISE(FAIL,'excess trace write'); END`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 80; i++ {
		c.sessionEvent("request_received", "other")
	}
	if s.storageErrors.Load() != 0 {
		t.Fatal("repeated packets kept writing after the trace limit")
	}
	if _, err = store.db.Exec(`DROP TRIGGER trace_write_limit`); err != nil {
		t.Fatal(err)
	}
	c.finishSessionTrace(errors.New(strings.Repeat("d", 1000)))
	row, _, err := store.Detail(context.Background(), id)
	if err != nil || row.Session == nil {
		t.Fatal(row, err)
	}
	trace := row.Session
	if len(trace.Events) != 32 || len(trace.EndError) != 512 || trace.LastStage != "request_received" || trace.Events[1].Kind != "other" {
		t.Fatal(trace)
	}
	if err = store.Finish(id, time.Second, "simulated_closed", "peer_closed", 0, 0); err != nil {
		t.Fatal(err)
	}
	store.Close()
	store, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	row, _, err = store.Detail(context.Background(), id)
	if err != nil || row.Session == nil || row.Session.AuthenticatedAt != trace.AuthenticatedAt {
		t.Fatal("session trace lost after restart", row, err)
	}
	if err = store.Prune(time.Now().Add(100*24*time.Hour), 90); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = store.db.QueryRow(`SELECT COUNT(*) FROM simulated_sessions`).Scan(&count); err != nil || count != 0 {
		t.Fatal("trace survived parent cleanup", count, err)
	}
}
