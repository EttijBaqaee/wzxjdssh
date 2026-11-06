package main

import (
	"context"
)

// Merge the complete live snapshot with stored records by connection ID.
// A connection which finished after the snapshot is counted once at its larger
// persisted value. Never derive this sum from the truncated TOP 10 ranking.
func (s *Store) TotalDelay(ctx context.Context, active []liveDelay) (int64, error) {
	return s.overlayTotal(ctx, "delayed_ms", active)
}
