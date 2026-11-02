package main

import (
	"context"
	"encoding/json"
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
	data, err := json.Marshal(active)
	if err != nil {
		return 0, err
	}
	var total int64
	err = s.db.QueryRowContext(ctx, `WITH samples AS (
	 SELECT connection_id AS id,MAX(0,COALESCE(json_extract(trace,'$.verified_post_auth_ms'),0)) AS verified
	 FROM simulated_sessions WHERE json_extract(trace,'$.verification_version')=1
	 UNION ALL
	 SELECT CAST(json_extract(value,'$.id') AS INTEGER),MAX(0,COALESCE(CAST(json_extract(value,'$.verified_post_auth_ms') AS INTEGER),0))
	 FROM json_each(?) WHERE EXISTS(SELECT 1 FROM connections WHERE id=CAST(json_extract(value,'$.id') AS INTEGER))
	), per_connection AS (SELECT id,MAX(verified) AS verified FROM samples GROUP BY id)
	 SELECT COALESCE(SUM(verified),0) FROM per_connection`, string(data)).Scan(&total)
	return total, err
}
