package main

import (
	"context"
	"time"
)

// This is a conservative liveness boundary, not proof that the remote scanner
// stopped doing other work. Never turn server elapsed time or TCP ACKs into it.
func updateVerifiedTime(trace *SessionTrace) {
	if trace.VerificationVersion != 1 {
		return // Older traces have no complete verification history.
	}
	trace.VerifiedPostAuthMS = max(int64(0), min(trace.PostAuthMS, trace.LastVerifiedAt-trace.AuthenticatedAt))
	trace.UnverifiedPostAuthMS = max(int64(0), trace.PostAuthMS-trace.VerifiedPostAuthMS)
}

func (c *clientConn) checkpointPeerVerification() {
	state := &c.sessionTrace
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.started.IsZero() || state.trace.EndedAt != 0 {
		return
	}
	state.trace.PostAuthMS = time.Since(state.started).Milliseconds()
	state.trace.LastReceivedAt = c.lastReceivedAt.Load()
	state.trace.LastVerifiedAt = c.peer.verifiedAt.Load()
	updateVerifiedTime(&state.trace)
	if c.server.store != nil && c.recordID != 0 {
		c.server.storageError(c.server.store.saveSessionTrace(c.recordID, state.trace))
	}
}

// Merge all live samples with stored checkpoints, including sessions which end
// between snapshot and query. The maximum per ID prevents double counting.
func (s *Store) TotalVerifiedPostAuth(ctx context.Context, active []liveDelay) (int64, error) {
	return s.overlayTotal(ctx, "verified_ms", active)
}
