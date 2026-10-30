package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRuntimeConfiguration(t *testing.T) {
	names := []string{"WZXJDSSH_DATABASE_PATH", "WZXJDSSH_RETENTION_DAYS", "WZXJDSSH_WEB_ENABLED", "WZXJDSSH_WEB_LISTEN", "WZXJDSSH_WEB_BASE_PATH", "WZXJDSSH_ADMIN_USERNAME", "WZXJDSSH_ADMIN_PASSWORD", "WZXJDSSH_WEB_SECURE_COOKIE"}
	for _, name := range names {
		t.Setenv(name, "")
	}
	t.Setenv("WZXJDSSH_DATABASE_PATH", "/tmp/test.sqlite")
	t.Setenv("WZXJDSSH_WEB_LISTEN", ":2223")
	t.Setenv("WZXJDSSH_ADMIN_USERNAME", "admin")
	if _, err := runtimeConfig(); err == nil {
		t.Fatal("accepted missing password")
	}
	t.Setenv("WZXJDSSH_ADMIN_PASSWORD", "test-admin-password")
	c, err := runtimeConfig()
	if err != nil || c.RetentionDays != 90 || c.WebListen != ":2223" {
		t.Fatalf("bad defaults: %+v %v", c, err)
	}
	for _, tc := range []struct{ name, value string }{{"WZXJDSSH_RETENTION_DAYS", "0"}, {"WZXJDSSH_RETENTION_DAYS", "36501"}, {"WZXJDSSH_WEB_BASE_PATH", "prefix"}, {"WZXJDSSH_WEB_BASE_PATH", "/../secret"}, {"WZXJDSSH_WEB_BASE_PATH", "/x//y"}, {"WZXJDSSH_WEB_BASE_PATH", "/x?y"}, {"WZXJDSSH_WEB_LISTEN", ":0"}, {"WZXJDSSH_WEB_ENABLED", "maybe"}} {
		t.Run(tc.name+tc.value, func(t *testing.T) {
			t.Setenv(tc.name, tc.value)
			if _, err := runtimeConfig(); err == nil {
				t.Fatal("accepted", tc)
			}
		})
	}
	t.Setenv("WZXJDSSH_WEB_BASE_PATH", "/ssh/panel/")
	c, err = runtimeConfig()
	if err != nil || c.WebBasePath != "/ssh/panel" {
		t.Fatal(c, err)
	}
}

func TestAdminUsernameEmptyDefaultsAndCustomLogin(t *testing.T) {
	t.Setenv("WZXJDSSH_ADMIN_PASSWORD", "custom-admin-password")
	t.Setenv("WZXJDSSH_WEB_ENABLED", "true")
	for _, name := range []string{"", "admin", "custom-admin"} {
		t.Setenv("WZXJDSSH_ADMIN_USERNAME", name)
		cfg, err := runtimeConfig()
		if err != nil { t.Fatal(err) }
		want := name
		if want == "" { want = "admin" }
		if cfg.AdminUsername != want { t.Fatal(cfg.AdminUsername, want) }
		w, err := NewWeb(cfg, testStore(t), nil)
		if err != nil { t.Fatal(err) }
		body, _ := json.Marshal(map[string]string{"username": want, "password": cfg.AdminPassword})
		r := httptest.NewRequest("POST", "http://example.test/api/login", strings.NewReader(string(body)))
		r.Header.Set("Content-Type", "application/json")
		rw := httptest.NewRecorder()
		w.Handler().ServeHTTP(rw, r)
		if rw.Code != 200 { t.Fatal(name, rw.Code, rw.Body.String()) }
	}
}

func TestWebAuthenticationPrefixAndRecords(t *testing.T) {
	s := testStore(t)
	id := mustStart(t, s, "192.0.2.1", time.Now())
	mustAttempt(t, s, id, time.Now(), "<script>alert(1)</script>", "password")
	w, err := NewWeb(RuntimeConfig{AdminUsername: "admin", AdminPassword: "test-admin-password", WebBasePath: "/sshpanel", SecureCookie: true, RetentionDays: 90}, s, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := w.Handler()
	call := func(method, path, body string, cookie *http.Cookie, origin string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "https://example.test"+path, strings.NewReader(body))
		r.RemoteAddr = "192.0.2.10:1234"
		r.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, r)
		return rw
	}
	for _, path := range []string{"/sshpanel/api/stats", "/sshpanel/api/ips", "/sshpanel/api/connections", fmt.Sprint("/sshpanel/api/connections/", id), "/sshpanel/api/session", "/sshpanel/api/settings"} {
		if r := call("GET", path, "", nil, ""); r.Code != 401 || strings.Contains(r.Body.String(), "test-secret") {
			t.Fatal(path, r.Code, r.Body.String())
		}
	}
	if r := call("GET", "/api/stats", "", nil, ""); r.Code != 404 {
		t.Fatal("root API exposed", r.Code)
	}
	if r := call("GET", "/sshpanel", "", nil, ""); r.Code != http.StatusPermanentRedirect || r.Header().Get("Location") != "/sshpanel/" {
		t.Fatal("prefix redirect", r.Code, r.Header())
	}
	for _, path := range []string{"/sshpanel/", "/sshpanel/app.js", "/sshpanel/theme.js", "/sshpanel/style.css"} {
		r := call("GET", path, "", nil, "")
		if r.Code != 200 || r.Header().Get("Content-Security-Policy") == "" || r.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(path, r.Code)
		}
	}
	body := `{"username":"admin","password":"test-admin-password"}`
	if r := call("POST", "/sshpanel/api/login", body, nil, "https://evil.test"); r.Code != 403 {
		t.Fatal("cross origin login allowed")
	}
	r := call("POST", "/sshpanel/api/login", body, nil, "https://example.test")
	cookies := r.Result().Cookies()
	if r.Code != 200 || len(cookies) != 1 {
		t.Fatal("login failed", r.Code, r.Body.String())
	}
	cookie := cookies[0]
	if !cookie.HttpOnly || !cookie.Secure || cookie.Path != "/sshpanel/" || cookie.SameSite != http.SameSiteStrictMode || cookie.MaxAge != 3600 {
		t.Fatalf("cookie flags: %+v", cookie)
	}
	if r = call("GET", "/sshpanel/api/stats", "", cookie, ""); r.Code != 200 {
		t.Fatal("stats", r.Code, r.Body.String())
	}
	if r = call("GET", "/sshpanel/api/connections", "", cookie, ""); r.Code != 200 || strings.Contains(r.Body.String(), "test-secret") {
		t.Fatal("list disclosed secret", r.Code, r.Body.String())
	}
	r = call("GET", fmt.Sprint("/sshpanel/api/connections/", id), "", cookie, "")
	var result struct {
		Attempts []AuthRecord `json:"attempts"`
	}
	if err = json.Unmarshal(r.Body.Bytes(), &result); err != nil || r.Code != 200 || len(result.Attempts) != 1 || result.Attempts[0].Credential != "test-secret" || result.Attempts[0].Username != "<script>alert(1)</script>" {
		t.Fatal("detail", err, r.Body.String())
	}
	if r = call("POST", "/sshpanel/api/logout", "", cookie, "https://evil.test"); r.Code != 403 {
		t.Fatal("cross origin logout")
	}
	if r = call("POST", "/sshpanel/api/logout", "", cookie, ""); r.Code != 200 {
		t.Fatal("logout", r.Code)
	}
	if r = call("GET", "/sshpanel/api/session", "", cookie, ""); r.Code != 401 {
		t.Fatal("replayed logged-out cookie accepted")
	}
}

func TestWebIdleTimeoutIsNotExtendedByPolling(t *testing.T) {
	w, err := NewWeb(RuntimeConfig{AdminUsername: "admin", AdminPassword: "test-admin-password"}, testStore(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	h := w.Handler()
	call := func(method, path, body string, cookie *http.Cookie, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://example.test"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, r)
		return rw
	}
	response := call("POST", "/api/login", `{"username":"admin","password":"test-admin-password"}`, nil, "")
	if response.Code != 200 {
		t.Fatal(response.Code)
	}
	cookie := response.Result().Cookies()[0]
	if cookie.MaxAge != 3600 {
		t.Fatal("wrong idle cookie lifetime", cookie.MaxAge)
	}
	key := sha256.Sum256([]byte(cookie.Value))
	nearExpiry := time.Now().Add(time.Minute)
	w.mu.Lock()
	w.sessions[key] = nearExpiry
	w.mu.Unlock()
	for i := 0; i < 3; i++ {
		if r := call("GET", "/api/stats", "", cookie, ""); r.Code != 200 {
			t.Fatal(r.Code)
		}
	}
	w.mu.Lock()
	until := w.sessions[key]
	w.mu.Unlock()
	if !until.Equal(nearExpiry) {
		t.Fatal("automatic polling renewed session")
	}
	if r := call("POST", "/api/activity", "", cookie, "https://evil.test"); r.Code != 403 {
		t.Fatal("cross origin renewal allowed")
	}
	response = call("POST", "/api/activity", "", cookie, "https://example.test")
	if response.Code != 200 || response.Result().Cookies()[0].MaxAge != 3600 {
		t.Fatal("activity did not renew session")
	}
	w.mu.Lock()
	until = w.sessions[key]
	w.mu.Unlock()
	if until.Before(time.Now().Add(59 * time.Minute)) {
		t.Fatal("activity failed to reset idle deadline")
	}
	w.mu.Lock()
	w.sessions[key] = time.Now().Add(-time.Second)
	w.mu.Unlock()
	if r := call("GET", "/api/session", "", cookie, ""); r.Code != 401 {
		t.Fatal("idle session accepted")
	}
	if r := call("POST", "/api/activity", "", cookie, ""); r.Code != 401 {
		t.Fatal("expired session resurrected")
	}
}

func TestWebConcurrentLoginLimit(t *testing.T) {
	w, err := NewWeb(RuntimeConfig{AdminUsername: "admin", AdminPassword: "test-admin-password"}, testStore(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	h := w.Handler()
	var group sync.WaitGroup
	var rejected, limited atomic.Int32
	for i := 0; i < 32; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			r := httptest.NewRequest("POST", "http://example.test/api/login", strings.NewReader(`{"username":"admin","password":"wrong"}`))
			r.RemoteAddr = "192.0.2.11:42"
			r.Header.Set("Content-Type", "application/json")
			rw := httptest.NewRecorder()
			h.ServeHTTP(rw, r)
			if rw.Code == 401 {
				rejected.Add(1)
			}
			if rw.Code == 429 {
				limited.Add(1)
			}
		}()
	}
	group.Wait()
	if rejected.Load() != 10 || limited.Load() != 22 {
		t.Fatal("concurrent limit bypass", rejected.Load(), limited.Load())
	}
}

func TestWebIdleSettingsPersistAndApply(t *testing.T) {
	store := testStore(t)
	cfg := RuntimeConfig{AdminUsername: "admin", AdminPassword: "test-admin-password"}
	w, err := NewWeb(cfg, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := w.Handler()
	call := func(method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://example.test"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, r)
		return rw
	}
	if r := call("POST", "/api/settings", `{"idle_timeout_minutes":30}`, nil); r.Code != 401 {
		t.Fatal("settings saved anonymously")
	}
	response := call("POST", "/api/login", `{"username":"admin","password":"test-admin-password"}`, nil)
	cookie := response.Result().Cookies()[0]
	for _, value := range []string{"0", "1441", "1.5", `"30"`} {
		if r := call("POST", "/api/settings", `{"idle_timeout_minutes":`+value+`}`, cookie); r.Code != 400 {
			t.Fatal("invalid idle setting accepted", value, r.Code)
		}
	}
	oldToken, err := w.sessionToken()
	if err != nil {
		t.Fatal(err)
	}
	oldKey := sha256.Sum256([]byte(oldToken))
	w.mu.Lock()
	w.sessions[oldKey] = time.Now().Add(-time.Second)
	w.mu.Unlock()
	response = call("POST", "/api/settings", `{"idle_timeout_minutes":30}`, cookie)
	if response.Code != 200 || response.Result().Cookies()[0].MaxAge != 1800 {
		t.Fatal("settings not applied", response.Code, response.Body.String())
	}
	if minutes, err := store.WebIdleMinutes(); err != nil || minutes != 30 {
		t.Fatal("setting not saved", minutes, err)
	}
	w.mu.Lock()
	_, resurrected := w.sessions[oldKey]
	w.mu.Unlock()
	if resurrected {
		t.Fatal("expired session resurrected")
	}
	again, err := NewWeb(cfg, store, nil)
	if err != nil || again.idleTimeout() != 30*time.Minute {
		t.Fatal("stored setting not loaded", err)
	}
	if r := call("GET", "/api/settings", "", cookie); r.Code != 200 || !strings.Contains(r.Body.String(), `"idle_timeout_minutes":30`) {
		t.Fatal("settings not readable", r.Body.String())
	}
}
