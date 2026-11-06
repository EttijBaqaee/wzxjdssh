package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// Only new connections use the changed identification. Reload and restart
// continue to use the existing configuration and environment precedence.
func (s *Server) setServerVersion(value string) error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	next := *s.cfg.Load()
	next.ServerVersion = value
	if err := next.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return errors.New("server stopped")
	}
	s.cfg.Store(&next)
	return nil
}

func (w *Web) bannerState(rw http.ResponseWriter, r *http.Request) {
	if w.emergency == nil {
		sendJSON(rw, 503, map[string]string{"error": "当前服务未提供横幅控制"})
		return
	}
	sendJSON(rw, 200, map[string]string{"server_version": w.emergency.cfg.Load().ServerVersion})
}

func (w *Web) bannerAction(rw http.ResponseWriter, r *http.Request) {
	if w.emergency == nil {
		sendJSON(rw, 503, map[string]string{"error": "当前服务未提供横幅控制"})
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		sendJSON(rw, 415, map[string]string{"error": "需要 JSON 请求"})
		return
	}
	var body struct {
		Password      string `json:"password"`
		ServerVersion string `json:"server_version"`
	}
	r.Body = http.MaxBytesReader(rw, r.Body, 4096)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&body) != nil || decoder.Decode(new(any)) != io.EOF {
		sendJSON(rw, 400, map[string]string{"error": "需要管理员密码和完整 SSH 横幅"})
		return
	}
	hash := sha256.Sum256([]byte(body.Password))
	body.Password = ""
	w.bannerMu.Lock()
	defer w.bannerMu.Unlock()
	now := time.Now()
	if !now.Before(w.bannerFailureUntil) {
		w.bannerFailures = 0
		w.bannerFailureUntil = now.Add(time.Minute)
	}
	if w.bannerFailures >= 5 {
		sendJSON(rw, 429, map[string]string{"error": "密码错误过多，请一分钟后重试"})
		return
	}
	if subtle.ConstantTimeCompare(hash[:], w.password[:]) != 1 {
		w.bannerFailures++
		sendJSON(rw, 403, map[string]string{"error": "管理员密码不正确，横幅未修改"})
		return
	}
	// Validate against the same rules used for configuration and environment.
	candidate := *w.emergency.cfg.Load()
	candidate.ServerVersion = body.ServerVersion
	if candidate.Validate() != nil {
		sendJSON(rw, 400, map[string]string{"error": "横幅必须以 SSH-2.0- 开头，软件名称不能为空，总长不超过 253 字节，仅允许可打印 ASCII 字符"})
		return
	}
	if err := w.emergency.setServerVersion(body.ServerVersion); err != nil {
		sendJSON(rw, 503, map[string]string{"error": "服务正在关闭，横幅未修改"})
		return
	}
	w.bannerState(rw, r)
}
