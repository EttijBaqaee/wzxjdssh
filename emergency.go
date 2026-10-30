package main

import (
	"context"
	"net/http"
	"time"
)

const emergencyDuration = time.Minute

type EmergencyStatus struct {
	Available   bool   `json:"available"`
	Active      bool   `json:"active"`
	Until       int64  `json:"until"`
	RemainingMS int64  `json:"remaining_ms"`
	Port        int    `json:"port"`
	Rejected    uint64 `json:"rejected"`
}

func (s *Server) emergencyStatus() EmergencyStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := EmergencyStatus{Available: true, Port: s.cfg.Load().Port, Rejected: s.stats.emergencyRejected.Load()}
	if left := time.Until(s.emergencyUntil); left > 0 {
		out.Active = true
		out.Until = s.emergencyUntil.UnixMilli()
		out.RemainingMS = left.Milliseconds()
	}
	return out
}

func (s *Server) emergencyDrop(ctx context.Context) (EmergencyStatus, error) {
	s.emergencyMu.Lock()
	defer s.emergencyMu.Unlock()
	if err := ctx.Err(); err != nil {
		return s.emergencyStatus(), err
	}
	s.mu.Lock()
	if time.Now().Before(s.emergencyUntil) {
		s.mu.Unlock()
		return s.emergencyStatus(), nil
	}
	s.emergencyUntil = time.Now().Add(emergencyDuration)
	s.stats.emergencyRejected.Store(0)
	clients := make([]*clientConn, 0, len(s.clients))
	for c := range s.clients {
		c.emergencyEvicted.Store(true)
		clients = append(clients, c)
	}
	port := s.cfg.Load().Port
	s.mu.Unlock()
	// Close real sockets and cancel SSH workers, rather than just clearing a map.
	// New arrivals are closed by admit() before any SSH banner or processing.
	for _, c := range clients {
		c.cancel()
		c.Conn.Close()
	}
	s.log(0, "emergency_release", map[string]any{"port": port, "duration_seconds": 60, "connections": len(clients)})
	return s.emergencyStatus(), nil
}

func (w *Web) emergencyState(rw http.ResponseWriter, r *http.Request) {
	if w.emergency == nil {
		sendJSON(rw, 200, map[string]any{"available": false, "reason": "当前服务未提供紧急连接控制"})
		return
	}
	sendJSON(rw, 200, w.emergency.emergencyStatus())
}
func (w *Web) emergencyAction(rw http.ResponseWriter, r *http.Request) {
	if w.emergency == nil {
		sendJSON(rw, 503, map[string]string{"error": "当前服务未提供紧急连接控制"})
		return
	}
	status, err := w.emergency.emergencyDrop(r.Context())
	if err != nil {
		sendJSON(rw, 503, map[string]string{"error": "紧急连接释放未完成"})
		return
	}
	sendJSON(rw, 200, status)
}
