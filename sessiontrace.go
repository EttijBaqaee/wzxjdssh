package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
)

type SessionEvent struct {
	At    int64  `json:"at"`
	Stage string `json:"stage"`
	Kind  string `json:"kind,omitempty"`
}

type SessionTrace struct {
	AuthenticatedAt int64          `json:"authenticated_at"`
	EndedAt         int64          `json:"ended_at,omitempty"`
	PostAuthMS      int64          `json:"post_auth_ms"`
	LastStage       string         `json:"last_stage"`
	EndError        string         `json:"end_error,omitempty"`
	Events          []SessionEvent `json:"events"`
}

type sessionTraceState struct {
	mu      sync.Mutex
	started time.Time
	trace   SessionTrace
}

func (s *Store) saveSessionTrace(id int64, trace SessionTrace) error {
	data, err := json.Marshal(trace)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err = s.db.Exec(`INSERT INTO simulated_sessions(connection_id,trace) VALUES(?,?) ON CONFLICT(connection_id) DO UPDATE SET trace=excluded.trace`, id, string(data))
	if err != nil {
		return err
	}
	return s.enforceBudgetLocked()
}

func (s *Store) loadSessionTrace(ctx context.Context, id int64) (*SessionTrace, error) {
	var data string
	err := s.db.QueryRowContext(ctx, `SELECT trace FROM simulated_sessions WHERE connection_id=?`, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var trace SessionTrace
	if err = json.Unmarshal([]byte(data), &trace); err != nil {
		return nil, err
	}
	return &trace, nil
}

// Keep a bounded timeline of protocol stages. No commands, environment values,
// input payloads or credentials are copied into the trace.
func (c *clientConn) sessionEvent(stage, kind string) {
	state := &c.sessionTrace
	state.mu.Lock()
	defer state.mu.Unlock()
	now := time.Now()
	if state.started.IsZero() {
		if stage != "authenticated" {
			return
		}
		state.started = now
		state.trace.AuthenticatedAt = now.UnixMilli()
	}
	state.trace.LastStage = stage
	state.trace.PostAuthMS = time.Since(state.started).Milliseconds()
	if len(state.trace.Events) >= 32 {
		return // Preserve the final summary in memory; flush it when transport ends.
	}
	state.trace.Events = append(state.trace.Events, SessionEvent{At: now.UnixMilli(), Stage: stage, Kind: protocolKind(kind)})
	if c.server.store != nil && c.recordID != 0 {
		c.server.storageError(c.server.store.saveSessionTrace(c.recordID, state.trace))
	}
}

func protocolKind(kind string) string {
	switch kind {
	case "", "session", "direct-tcpip", "forwarded-tcpip", "keepalive@openssh.com", "tcpip-forward", "cancel-tcpip-forward", "env", "pty-req", "window-change", "shell", "exec", "subsystem":
		return kind
	default:
		return "other"
	}
}

func (c *clientConn) finishSessionTrace(err error) {
	state := &c.sessionTrace
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.started.IsZero() {
		return
	}
	state.trace.EndedAt = time.Now().UnixMilli()
	state.trace.PostAuthMS = time.Since(state.started).Milliseconds()
	if err != nil {
		message := strings.ToValidUTF8(err.Error(), "�")
		runes := []rune(message)
		if len(runes) > 512 {
			runes = runes[:512]
		}
		state.trace.EndError = string(runes)
	}
	if c.server.store != nil && c.recordID != 0 {
		c.server.storageError(c.server.store.saveSessionTrace(c.recordID, state.trace))
	}
}

func (c *clientConn) learnHeldSession(ctx context.Context, done *sync.WaitGroup) {
	defer done.Done()
	timer := time.NewTimer(c.server.cfg.Load().Delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		if ctx.Err() == nil && c.ctx.Err() == nil {
			c.sessionEvent("session_held", "")
			c.learnAuthDelay(true)
		}
	case <-ctx.Done():
	}
}
