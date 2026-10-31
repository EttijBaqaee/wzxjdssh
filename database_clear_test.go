package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClearDatabaseRequiresPasswordAndPreservesSettings(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	id := mustStart(t, store, "192.0.2.5", time.Now())
	mustAttempt(t, store, id, time.Now(), "synthetic-user", "password")
	if err := store.SetWebIdleMinutes(ctx, 17); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t)
	s, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.store = store
	if err = s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	conn := connect(t, cfg)
	defer conn.Close()
	bannerLine(t, conn)
	w, err := NewWeb(RuntimeConfig{AdminUsername: "admin", AdminPassword: "synthetic-admin-password", WebBasePath: "/prefix"}, store, s.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	w.emergency = s
	defer w.Close()
	token, err := w.sessionToken()
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, body, origin, session string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://test/prefix/api/database/clear", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if session != "" {
			r.AddCookie(w.sessionCookie(session, 3600))
		}
		out := httptest.NewRecorder()
		w.Handler().ServeHTTP(out, r)
		return out
	}
	body := `{"password":"synthetic-admin-password","confirm":"CLEAR_DATABASE"}`
	if r := call("POST", body, "", ""); r.Code != 401 {
		t.Fatal(r.Code)
	}
	if r := call("POST", body, "http://evil.invalid", token); r.Code != 403 {
		t.Fatal(r.Code)
	}
	if r := call("POST", `{"password":"wrong","confirm":"CLEAR_DATABASE"}`, "http://test", token); r.Code != 403 {
		t.Fatal(r.Code)
	}
	if page, err := store.List(ctx, RecordFilter{}); err != nil || page.Total < 2 {
		t.Fatal("wrong password changed records", page, err)
	}
	if r := call("POST", body, "http://test", token); r.Code != 202 {
		t.Fatal(r.Code, r.Body.String())
	}
	eventually(t, func() bool {
		r := call("GET", "", "", token)
		var status DatabaseClearStatus
		if json.Unmarshal(r.Body.Bytes(), &status) != nil {
			return false
		}
		return !status.Running && status.Cleared && status.Error == ""
	})
	for _, table := range []string{"connections", "auth_attempts", "connection_marks", "simulated_sessions", "client_cooldowns", "ip_slowdowns", "ip_probes"} {
		var n int
		if err = store.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatal(table, n, err)
		}
	}
	if minutes, err := store.WebIdleMinutes(); err != nil || minutes != 17 {
		t.Fatal("settings lost", minutes, err)
	}
	if s.stats.active.Load() != 0 || !s.emergencyStatus().Active {
		t.Fatal("clients not released or admission not paused")
	}
	time.Sleep(30 * time.Millisecond)
	if page, err := store.List(ctx, RecordFilter{}); err != nil || page.Total != 0 {
		t.Fatal("old worker rewrote cleared data", page, err)
	}
}
