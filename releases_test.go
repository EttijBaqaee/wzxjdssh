package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func releaseTestChecker(t *testing.T, current string, handler http.HandlerFunc) *ReleaseChecker {
	t.Helper()
	server := httptest.NewServer(handler)
	c := NewReleaseChecker(current)
	c.endpoint = server.URL
	t.Cleanup(func() { c.Close(); server.Close() })
	return c
}

func TestReleaseVersionComparison(t *testing.T) {
	for _, tc := range []struct {
		current, latest string
		update          bool
	}{
		{"2.0.9", "v2.0.10", true}, {"2.1.0", "v2.1.0", false}, {"2.2.0", "v2.1.0", false},
		{"2.1.0-rc.1", "v2.1.0", true}, {"v2.0.6", "v2.1.0", true},
	} {
		t.Run(tc.current+tc.latest, func(t *testing.T) {
			c := releaseTestChecker(t, tc.current, func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, `{"tag_name":%q}`, tc.latest) })
			if !c.Refresh(false).Checking {
				t.Fatal("check did not start")
			}
			c.wg.Wait()
			s := c.Snapshot()
			if s.Error != "" || s.UpdateAvailable != tc.update || s.Latest != tc.latest || s.CheckedAt == 0 || s.URL != latestReleasePage {
				t.Fatalf("%+v", s)
			}
		})
	}
}

func TestReleaseCacheForceAndETag(t *testing.T) {
	var calls atomic.Int32
	c := releaseTestChecker(t, "2.0.6", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "wzxjdssh/2.0.6" {
			t.Error("missing client identification")
		}
		if calls.Add(1) > 1 {
			if r.Header.Get("If-None-Match") != `"one"` {
				t.Error("missing etag")
			}
			w.WriteHeader(304)
			return
		}
		w.Header().Set("ETag", `"one"`)
		fmt.Fprint(w, `{"tag_name":"v2.1.0"}`)
	})
	now := time.Unix(100000, 0)
	c.now = func() time.Time { return now }
	c.Refresh(false)
	c.wg.Wait()
	first := c.Snapshot()
	c.Refresh(false)
	c.Refresh(true)
	c.wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("refresh bypassed cooldown")
	}
	now = now.Add(31 * time.Second)
	c.Refresh(true)
	c.wg.Wait()
	if calls.Load() != 2 || c.Snapshot().Latest != first.Latest || !c.Snapshot().UpdateAvailable {
		t.Fatal(calls.Load(), c.Snapshot())
	}
	now = now.Add(59 * time.Minute)
	c.Refresh(false)
	c.wg.Wait()
	if calls.Load() != 2 {
		t.Fatal("cache expired early")
	}
	now = now.Add(2 * time.Minute)
	c.Refresh(false)
	c.wg.Wait()
	if calls.Load() != 3 {
		t.Fatal("cache never expired")
	}
}

func TestReleaseErrorsRetainResultAndBackoff(t *testing.T) {
	var fail atomic.Bool
	var calls atomic.Int32
	c := releaseTestChecker(t, "2.0.6", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if fail.Load() {
			w.WriteHeader(503)
			return
		}
		fmt.Fprint(w, `{"tag_name":"v2.1.0"}`)
	})
	now := time.Unix(100000, 0)
	c.now = func() time.Time { return now }
	c.Refresh(false)
	c.wg.Wait()
	first := c.Snapshot()
	fail.Store(true)
	now = now.Add(time.Hour)
	c.Refresh(false)
	c.wg.Wait()
	s := c.Snapshot()
	if s.Error == "" || s.CheckedAt != first.CheckedAt || !s.UpdateAvailable || s.Latest != first.Latest {
		t.Fatal(s)
	}
	now = now.Add(9 * time.Minute)
	c.Refresh(false)
	c.wg.Wait()
	if calls.Load() != 2 {
		t.Fatal("retry too early")
	}
	now = now.Add(time.Minute)
	c.Refresh(false)
	c.wg.Wait()
	if calls.Load() != 3 {
		t.Fatal("retry missing")
	}
}

func TestReleaseRejectsInvalidResponses(t *testing.T) {
	for _, body := range []string{`{`, `{}`, `{"tag_name":"development"}`, `{"tag_name":"v2.2.0","draft":true}`, `{"tag_name":"v2.2.0","prerelease":true}`, `{"tag_name":"v2.2.0-rc.1"}`, strings.Repeat("x", 512*1024+1)} {
		c := releaseTestChecker(t, "2.1.0", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
		c.Refresh(false)
		c.wg.Wait()
		s := c.Snapshot()
		if s.Error == "" || s.UpdateAvailable || s.CheckedAt != 0 {
			t.Fatal(s)
		}
	}
}

func TestReleaseSingleRequestAndCancellation(t *testing.T) {
	started, done := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	c := releaseTestChecker(t, "2.1.0", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		close(started)
		<-r.Context().Done()
		close(done)
	})
	c.Refresh(false)
	<-started
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.Refresh(true) }()
	}
	wg.Wait()
	c.Close()
	<-done
	c.Refresh(true)
	if calls.Load() != 1 || c.Snapshot().Checking {
		t.Fatal(calls.Load(), c.Snapshot())
	}
}

func TestReleaseInvalidBuildDoesNotConnect(t *testing.T) {
	c := releaseTestChecker(t, "development", func(w http.ResponseWriter, r *http.Request) { t.Error("invalid build attempted network") })
	if s := c.Refresh(true); s.Checking || s.Error == "" {
		t.Fatal(s)
	}
}

func TestReleaseRejectsRedirect(t *testing.T) {
	c := releaseTestChecker(t, "2.1.0", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/elsewhere", 302) })
	c.Refresh(false)
	c.wg.Wait()
	if s := c.Snapshot(); s.Error == "" || s.UpdateAvailable {
		t.Fatal(s)
	}
}

func TestReleaseAPIAuthenticationAndOrigin(t *testing.T) {
	w, err := NewWeb(RuntimeConfig{AdminUsername: "admin", AdminPassword: "test-admin-password", WebBasePath: "/panel"}, testStore(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.releases.Close()
	w.releases = releaseTestChecker(t, "2.1.0", func(rw http.ResponseWriter, r *http.Request) { fmt.Fprint(rw, `{"tag_name":"v2.2.0"}`) })
	h := w.Handler()
	call := func(method, path, body, origin string, cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://example.test/panel"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, r)
		return rw
	}
	for _, path := range []string{"/api/version", "/api/version/check"} {
		method := "GET"
		if strings.HasSuffix(path, "check") {
			method = "POST"
		}
		if r := call(method, path, "", "", nil); r.Code != 401 {
			t.Fatal(r.Code)
		}
	}
	login := call("POST", "/api/login", `{"username":"admin","password":"test-admin-password"}`, "", nil)
	if login.Code != 200 {
		t.Fatal(login.Code)
	}
	cookie := login.Result().Cookies()[0]
	if r := call("POST", "/api/version/check", "", "https://evil.test", cookie); r.Code != 403 {
		t.Fatal("cross-origin accepted", r.Code)
	}
	if r := call("GET", "/api/version", "", "", cookie); r.Code != 200 || !strings.Contains(r.Body.String(), `"checking":true`) {
		t.Fatal(r.Code, r.Body.String())
	}
	w.releases.wg.Wait()
	if r := call("GET", "/api/version", "", "", cookie); !strings.Contains(r.Body.String(), `"update_available":true`) {
		t.Fatal(r.Body.String())
	}
}
