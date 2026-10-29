package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed web/*
var webFiles embed.FS

type Web struct {
	cfg        RuntimeConfig
	store      *Store
	snapshot   func() map[string]any
	secret     [32]byte
	password   [32]byte
	server     *http.Server
	errors     chan error
	mu         sync.Mutex
	failures   map[string]loginFailures
	sessions   map[[32]byte]time.Time
	idleNanos  atomic.Int64
	settingsMu sync.Mutex
}
type loginFailures struct {
	Count int
	Until time.Time
}

func NewWeb(c RuntimeConfig, s *Store, snapshot func() map[string]any) (*Web, error) {
	if len(c.AdminPassword) < 12 {
		return nil, errors.New("admin password must be at least 12 bytes")
	}
	w := &Web{cfg: c, store: s, snapshot: snapshot, errors: make(chan error, 1), failures: map[string]loginFailures{}, password: sha256.Sum256([]byte(c.AdminPassword))}
	w.cfg.AdminPassword = ""
	minutes, err := s.WebIdleMinutes()
	if err != nil {
		return nil, err
	}
	w.idleNanos.Store(int64(time.Duration(minutes) * time.Minute))
	w.sessions = make(map[[32]byte]time.Time)
	if _, err := rand.Read(w.secret[:]); err != nil {
		return nil, err
	}
	w.server = &http.Server{Handler: w.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384}
	return w, nil
}
func (w *Web) Start() error {
	l, err := net.Listen("tcp", w.cfg.WebListen)
	if err != nil {
		return err
	}
	go func() {
		err := w.server.Serve(l)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			w.errors <- err
		}
	}()
	return nil
}
func (w *Web) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if w.server.Shutdown(ctx) != nil {
		w.server.Close()
	}
}
func (w *Web) sessionCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: "wzxjdssh_session", Value: value, Path: w.cfg.WebBasePath + "/", HttpOnly: true, Secure: w.cfg.SecureCookie, SameSite: http.SameSiteStrictMode, MaxAge: maxAge}
}
func (w *Web) idleTimeout() time.Duration { return time.Duration(w.idleNanos.Load()) }
func (w *Web) sessionToken() (string, error) {
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	payload := strconv.FormatInt(time.Now().Unix(), 10) + "." + base64.RawURLEncoding.EncodeToString(nonce[:])
	m := hmac.New(sha256.New, w.secret[:])
	m.Write([]byte(payload))
	token := payload + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	for k, v := range w.sessions {
		if !v.After(now) {
			delete(w.sessions, k)
		}
	}
	if len(w.sessions) >= 4096 {
		return "", errors.New("too many active sessions")
	}
	w.sessions[sha256.Sum256([]byte(token))] = now.Add(w.idleTimeout())
	return token, nil
}
func (w *Web) authenticated(r *http.Request) bool {
	cookie, err := r.Cookie("wzxjdssh_session")
	if err != nil || len(cookie.Value) > 256 {
		return false
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 3 {
		return false
	}
	issued, err := strconv.ParseInt(parts[0], 10, 64)
	now := time.Now().Unix()
	if err != nil || issued > now+60 {
		return false
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, w.secret[:])
	m.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(signature, m.Sum(nil)) {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	key := sha256.Sum256([]byte(cookie.Value))
	until, ok := w.sessions[key]
	if ok && !until.After(time.Now()) {
		delete(w.sessions, key)
		return false
	}
	return ok && until.After(time.Now())
}
func sendJSON(rw http.ResponseWriter, status int, v any) {
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.WriteHeader(status)
	json.NewEncoder(rw).Encode(v)
}
func (w *Web) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(rw http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if w.store.db.PingContext(ctx) != nil {
			sendJSON(rw, 503, map[string]bool{"ok": false})
			return
		}
		sendJSON(rw, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/login", w.login)
	mux.HandleFunc("POST /api/activity", func(rw http.ResponseWriter, r *http.Request) {
		if !w.authenticated(r) {
			sendJSON(rw, 401, map[string]string{"error": "请先登录"})
			return
		}
		cookie, _ := r.Cookie("wzxjdssh_session")
		key := sha256.Sum256([]byte(cookie.Value))
		now := time.Now()
		w.mu.Lock()
		until, ok := w.sessions[key]
		valid := ok && until.After(now)
		if valid {
			w.sessions[key] = now.Add(w.idleTimeout())
		}
		w.mu.Unlock()
		if !valid {
			sendJSON(rw, 401, map[string]string{"error": "请先登录"})
			return
		}
		http.SetCookie(rw, w.sessionCookie(cookie.Value, int(w.idleTimeout().Seconds())))
		sendJSON(rw, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/logout", func(rw http.ResponseWriter, r *http.Request) {
		if !w.authenticated(r) {
			sendJSON(rw, 401, map[string]string{"error": "请先登录"})
			return
		}
		cookie, _ := r.Cookie("wzxjdssh_session")
		w.mu.Lock()
		delete(w.sessions, sha256.Sum256([]byte(cookie.Value)))
		w.mu.Unlock()
		http.SetCookie(rw, w.sessionCookie("", -1))
		sendJSON(rw, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /api/session", func(rw http.ResponseWriter, r *http.Request) {
		if !w.authenticated(r) {
			sendJSON(rw, 401, map[string]string{"error": "请先登录"})
			return
		}
		sendJSON(rw, 200, map[string]any{"username": w.cfg.AdminUsername, "retention_days": w.cfg.RetentionDays, "version": version, "idle_timeout_seconds": int(w.idleTimeout().Seconds())})
	})
	mux.HandleFunc("GET /api/settings", w.protected(w.settings))
	mux.HandleFunc("POST /api/settings", w.protected(w.settings))
	mux.HandleFunc("GET /api/stats", w.protected(w.stats))
	mux.HandleFunc("GET /api/connections", w.protected(w.connections))
	mux.HandleFunc("GET /api/connections/{id}", w.protected(w.detail))
	files, _ := fs.Sub(webFiles, "web")
	mux.HandleFunc("GET /", func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(rw, r)
			return
		}
		data, _ := fs.ReadFile(files, "index.html")
		rw.Header().Set("Content-Type", "text/html; charset=utf-8")
		rw.Write(data)
	})
	mux.HandleFunc("GET /app.js", func(rw http.ResponseWriter, r *http.Request) {
		data, _ := fs.ReadFile(files, "app.js")
		rw.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		rw.Write(data)
	})
	mux.HandleFunc("GET /theme.js", func(rw http.ResponseWriter, r *http.Request) {
		data, _ := fs.ReadFile(files, "theme.js")
		rw.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		rw.Write(data)
	})
	mux.HandleFunc("GET /style.css", func(rw http.ResponseWriter, r *http.Request) {
		data, _ := fs.ReadFile(files, "style.css")
		rw.Header().Set("Content-Type", "text/css; charset=utf-8")
		rw.Write(data)
	})
	var handler http.Handler = mux
	base := w.cfg.WebBasePath
	if base != "" {
		handler = http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			if r.URL.Path == base {
				http.Redirect(rw, r, base+"/", http.StatusPermanentRedirect)
				return
			}
			if !strings.HasPrefix(r.URL.Path, base+"/") {
				http.NotFound(rw, r)
				return
			}
			http.StripPrefix(base, mux).ServeHTTP(rw, r)
		})
	}
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("X-Session-Idle-Seconds", strconv.Itoa(int(w.idleTimeout().Seconds())))
		rw.Header().Set("Cache-Control", "no-store")
		rw.Header().Set("X-Content-Type-Options", "nosniff")
		rw.Header().Set("Referrer-Policy", "no-referrer")
		rw.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		if r.Method == http.MethodPost {
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || u.Host != r.Host || (u.Scheme != "http" && u.Scheme != "https") {
					sendJSON(rw, 403, map[string]string{"error": "来源校验失败"})
					return
				}
			}
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				sendJSON(rw, 403, map[string]string{"error": "来源校验失败"})
				return
			}
		}
		handler.ServeHTTP(rw, r)
	})
}
func (w *Web) protected(next http.HandlerFunc) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		if !w.authenticated(r) {
			sendJSON(rw, 401, map[string]string{"error": "请先登录"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		next(rw, r.WithContext(ctx))
	}
}
func (w *Web) login(rw http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		sendJSON(rw, 415, map[string]string{"error": "需要 JSON 请求"})
		return
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	w.mu.Lock()
	now := time.Now()
	for k, v := range w.failures {
		if now.After(v.Until) {
			delete(w.failures, k)
		}
	}
	entry := w.failures[ip]
	limited := entry.Count >= 10 && now.Before(entry.Until)
	full := len(w.failures) >= 4096 && entry.Count == 0
	if !limited && !full {
		entry.Count++
		if entry.Until.IsZero() {
			entry.Until = now.Add(5 * time.Minute)
		}
		w.failures[ip] = entry
	}
	w.mu.Unlock()
	if limited || full {
		rw.Header().Set("Retry-After", "300")
		sendJSON(rw, 429, map[string]string{"error": "登录尝试过多，请稍后重试"})
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	r.Body = http.MaxBytesReader(rw, r.Body, 16384)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		sendJSON(rw, 400, map[string]string{"error": "请求格式错误"})
		return
	}
	given := sha256.Sum256([]byte(body.Password))
	name := sha256.Sum256([]byte(body.Username))
	expected := sha256.Sum256([]byte(w.cfg.AdminUsername))
	if subtle.ConstantTimeCompare(given[:], w.password[:])&subtle.ConstantTimeCompare(name[:], expected[:]) != 1 {
		sendJSON(rw, 401, map[string]string{"error": "用户名或密码错误"})
		return
	}
	token, err := w.sessionToken()
	if err != nil {
		sendJSON(rw, 503, map[string]string{"error": "暂时无法创建会话"})
		return
	}
	w.mu.Lock()
	delete(w.failures, ip)
	w.mu.Unlock()
	http.SetCookie(rw, w.sessionCookie(token, int(w.idleTimeout().Seconds())))
	sendJSON(rw, 200, map[string]bool{"ok": true})
}
func (w *Web) stats(rw http.ResponseWriter, r *http.Request) {
	stats, err := w.store.Stats(r.Context())
	if err != nil {
		sendJSON(rw, 500, map[string]string{"error": "无法读取统计数据"})
		return
	}
	live := map[string]any{}
	if w.snapshot != nil {
		live = w.snapshot()
	}
	sendJSON(rw, 200, map[string]any{"stored": stats, "runtime": live, "retention_days": w.cfg.RetentionDays, "timezone": "UTC"})
}

func (w *Web) settings(rw http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			sendJSON(rw, 415, map[string]string{"error": "需要 JSON 请求"})
			return
		}
		var body struct {
			Minutes int `json:"idle_timeout_minutes"`
		}
		r.Body = http.MaxBytesReader(rw, r.Body, 4096)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil || body.Minutes < 1 || body.Minutes > 1440 {
			sendJSON(rw, 400, map[string]string{"error": "闲置退出时间须为 1–1440 分钟"})
			return
		}
		w.settingsMu.Lock()
		defer w.settingsMu.Unlock()
		if err := w.store.SetWebIdleMinutes(r.Context(), body.Minutes); err != nil {
			sendJSON(rw, 500, map[string]string{"error": "无法保存管理设置"})
			return
		}
		cookie, _ := r.Cookie("wzxjdssh_session")
		current := sha256.Sum256([]byte(cookie.Value))
		now := time.Now()
		next := time.Duration(body.Minutes) * time.Minute
		w.mu.Lock()
		previous := w.idleTimeout()
		activeUntil, active := w.sessions[current]
		active = active && activeUntil.After(now)
		w.idleNanos.Store(int64(next))
		for key, until := range w.sessions {
			if !until.After(now) {
				delete(w.sessions, key)
				continue
			}
			updated := until.Add(-previous).Add(next)
			if !updated.After(now) {
				delete(w.sessions, key)
			} else {
				w.sessions[key] = updated
			}
		}
		if active {
			w.sessions[current] = now.Add(next)
		}
		w.mu.Unlock()
		if !active {
			sendJSON(rw, 401, map[string]string{"error": "请先登录"})
			return
		}
		http.SetCookie(rw, w.sessionCookie(cookie.Value, int(next.Seconds())))
	}
	sendJSON(rw, 200, map[string]int{"idle_timeout_minutes": int(w.idleTimeout() / time.Minute), "idle_timeout_seconds": int(w.idleTimeout() / time.Second)})
}
func (w *Web) connections(rw http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	f := RecordFilter{Page: page, Limit: limit, IP: q.Get("ip"), Username: q.Get("username"), Status: q.Get("status")}
	if len(f.IP) > 128 || len(f.Username) > 4096 || len(f.Status) > 64 {
		sendJSON(rw, 400, map[string]string{"error": "筛选条件过长"})
		return
	}
	out, err := w.store.List(r.Context(), f)
	if err != nil {
		sendJSON(rw, 500, map[string]string{"error": "无法读取连接记录"})
		return
	}
	sendJSON(rw, 200, out)
}
func (w *Web) detail(rw http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		sendJSON(rw, 400, map[string]string{"error": "无效连接编号"})
		return
	}
	c, a, err := w.store.Detail(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		sendJSON(rw, 404, map[string]string{"error": "记录不存在"})
		return
	}
	if err != nil {
		sendJSON(rw, 500, map[string]string{"error": "无法读取详情"})
		return
	}
	sendJSON(rw, 200, map[string]any{"connection": c, "attempts": a})
}
