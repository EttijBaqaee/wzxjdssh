package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

type DatabaseClearStatus struct {
	Running     bool   `json:"running"`
	FinishedAt  int64  `json:"finished_at,omitempty"`
	Cleared     bool   `json:"cleared"`
	Connections int64  `json:"deleted_connections"`
	Error       string `json:"error,omitempty"`
}

// Clear records in the existing open database. Never unlink an in-use SQLite
// file: all foreign-key dependents and persisted policy tags share the commit.
func (s *Store) ClearRecords(ctx context.Context) (int64, bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()
	var count int64
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM connections`).Scan(&count); err != nil {
		return 0, false, err
	}
	for _, table := range []string{"auth_attempts", "connection_marks", "simulated_sessions", "connections", "client_cooldowns", "ip_slowdowns", "ip_probes"} {
		if _, err = tx.ExecContext(ctx, `DELETE FROM `+table); err != nil {
			return 0, false, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, false, err
	}
	s.capacityDeleted.Store(0)
	if _, err = s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return count, true, err
	}
	if _, err = s.db.ExecContext(ctx, `VACUUM`); err != nil {
		return count, true, err
	}
	_, err = s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return count, true, err
}

func (s *Server) clearRecords(ctx context.Context) (int64, bool, error) {
	// acceptConnection is the sole admission writer. The gate waits for any
	// in-flight admission/rejection write before taking the client snapshot.
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	if s.store == nil {
		return 0, false, errors.New("no database")
	}
	if _, err := s.emergencyDrop(ctx); err != nil {
		return 0, false, err
	}
	s.mu.Lock()
	clients := make([]*clientConn, 0, len(s.clients))
	for c := range s.clients {
		c.emergencyEvicted.Store(true)
		clients = append(clients, c)
	}
	s.mu.Unlock()
	for _, c := range clients {
		c.cancel()
		c.Conn.Close()
	}
	for _, c := range clients {
		select {
		case <-c.done:
		case <-ctx.Done():
			return 0, false, ctx.Err()
		}
	}
	s.policyMu.Lock()
	defer s.policyMu.Unlock()
	n, cleared, err := s.store.ClearRecords(ctx)
	if cleared {
		s.singleAuth = singleAuthRetries{}
		s.stats.accepted.Store(0)
		s.stats.rejected.Store(0)
		s.stats.acceptErrors.Store(0)
		s.stats.closed.Store(0)
		s.stats.evicted.Store(0)
		s.stats.bytesSent.Store(0)
		s.stats.authAttempts.Store(0)
		s.stats.closedNanos.Store(0)
		s.storageErrors.Store(0)
		s.authDelays.mu.Lock()
		s.authDelays.entries = nil
		s.authDelays.lastPrune = time.Time{}
		s.authDelays.mu.Unlock()
		s.mu.Lock()
		s.buckets = map[string]rateBucket{}
		s.mu.Unlock()
		s.log(0, "database_cleared", map[string]any{"connections": n})
	}
	return n, cleared, err
}

func (w *Web) databaseClearState(rw http.ResponseWriter, r *http.Request) {
	w.clearMu.Lock()
	status := w.clearStatus
	w.clearMu.Unlock()
	sendJSON(rw, 200, status)
}
func (w *Web) databaseClearAction(rw http.ResponseWriter, r *http.Request) {
	if w.emergency == nil {
		sendJSON(rw, 503, map[string]string{"error": "当前服务未提供安全清理控制"})
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		sendJSON(rw, 415, map[string]string{"error": "需要 JSON 请求"})
		return
	}
	var body struct {
		Password string `json:"password"`
		Confirm  string `json:"confirm"`
	}
	r.Body = http.MaxBytesReader(rw, r.Body, 4096)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&body) != nil || body.Confirm != "CLEAR_DATABASE" {
		sendJSON(rw, 400, map[string]string{"error": "需要管理员密码和清理确认"})
		return
	}
	hash := sha256.Sum256([]byte(body.Password))
	body.Password = ""
	w.clearMu.Lock()
	now := time.Now()
	if now.After(w.clearFailureUntil) {
		w.clearFailures = 0
		w.clearFailureUntil = now.Add(time.Minute)
	}
	if w.clearFailures >= 5 {
		w.clearMu.Unlock()
		sendJSON(rw, 429, map[string]string{"error": "密码错误过多，请一分钟后重试"})
		return
	}
	if subtle.ConstantTimeCompare(hash[:], w.password[:]) != 1 {
		w.clearFailures++
		w.clearMu.Unlock()
		sendJSON(rw, 403, map[string]string{"error": "管理员密码不正确，未清理任何数据"})
		return
	}
	if w.clearClosing {
		w.clearMu.Unlock()
		sendJSON(rw, 503, map[string]string{"error": "服务正在关闭"})
		return
	}
	if w.clearStatus.Running {
		status := w.clearStatus
		w.clearMu.Unlock()
		sendJSON(rw, http.StatusAccepted, status)
		return
	}
	w.clearStatus = DatabaseClearStatus{Running: true}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	w.clearCancel = cancel
	w.clearWG.Add(1)
	status := w.clearStatus
	w.clearMu.Unlock()
	go func() {
		defer w.clearWG.Done()
		defer cancel()
		n, cleared, err := w.emergency.clearRecords(ctx)
		next := DatabaseClearStatus{FinishedAt: time.Now().UnixMilli(), Cleared: cleared, Connections: n}
		if err != nil {
			if cleared {
				next.Error = "记录已清空，但数据库空间回收未完成，请检查容器日志和磁盘空间"
			} else {
				next.Error = "清理未完成，原数据库仍保留；请检查容器日志"
			}
		}
		w.clearMu.Lock()
		w.clearStatus = next
		w.clearCancel = nil
		w.clearMu.Unlock()
	}()
	sendJSON(rw, http.StatusAccepted, status)
}
