package main

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEmergencyReleasesClientsAndDoesNotRenew(t *testing.T) {
	cfg := testConfig(t)
	s := startTest(t, cfg)
	conn := connect(t, cfg)
	defer conn.Close()
	eventually(t, func() bool { return s.stats.active.Load() == 1 })
	status, err := s.emergencyDrop(context.Background())
	if err != nil || !status.Active || status.Port != cfg.Port {
		t.Fatal(status, err)
	}
	eventually(t, func() bool { return s.stats.active.Load() == 0 })
	next, err := s.emergencyDrop(context.Background())
	if err != nil || next.Until != status.Until {
		t.Fatal("repeat action renewed the hold", next, err)
	}
	s.mu.Lock()
	reason := s.admit("192.0.2.1", cfg)
	s.emergencyUntil = time.Now().Add(-time.Second)
	resumed := s.admit("192.0.2.1", cfg)
	s.mu.Unlock()
	if reason != "emergency_drop" || resumed != "" {
		t.Fatal(reason, resumed)
	}
}

func TestEmergencyAPIRequiresAdminSessionAndSameOrigin(t *testing.T) {
	store := testStore(t)
	cfg := testConfig(t)
	s, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewWeb(RuntimeConfig{AdminPassword: "synthetic-admin-password", WebBasePath: "/prefix"}, store, s.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	w.emergency = s
	call := func(method, origin, token string) int {
		r := httptest.NewRequest(method, "http://test/prefix/api/emergency", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if token != "" {
			r.AddCookie(w.sessionCookie(token, 3600))
		}
		rw := httptest.NewRecorder()
		w.Handler().ServeHTTP(rw, r)
		return rw.Code
	}
	if code := call("POST", "", ""); code != 401 {
		t.Fatal(code)
	}
	token, err := w.sessionToken()
	if err != nil {
		t.Fatal(err)
	}
	if code := call("POST", "http://evil.invalid", token); code != 403 {
		t.Fatal(code)
	}
	if code := call("GET", "", token); code != 200 {
		t.Fatal(code)
	}
	if code := call("POST", "http://test", token); code != 200 {
		t.Fatal(code)
	}
}
