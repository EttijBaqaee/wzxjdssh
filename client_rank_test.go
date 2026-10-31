package main

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestClientVersionRankingCountsConnectionsAndLimitsTop10(t *testing.T) {
	s := testStore(t)
	for i := 0; i < 12; i++ {
		for j := 0; j <= i; j++ {
			id := mustStart(t, s, "192.0.2.1", time.Now())
			if err := s.Transport(id, fmt.Sprintf("SSH-2.0-client-%02d", i)); err != nil {
				t.Fatal(err)
			}
			// Multiple credential attempts must not inflate the connection count.
			for k := 0; k < 2; k++ {
				mustAttempt(t, s, id, time.Now(), "synthetic", "password")
			}
		}
	}
	// Unknown identification strings never become a blank ranking entry.
	mustStart(t, s, "192.0.2.2", time.Now())
	stats, err := s.Stats(context.Background())
	if err != nil || len(stats.ClientVersions) != 10 {
		t.Fatal("client ranking missing or not limited", stats.ClientVersions, err)
	}
	for i, item := range stats.ClientVersions {
		if item.Name != fmt.Sprintf("SSH-2.0-client-%02d", 11-i) || item.Count != int64(12-i) {
			t.Fatal("client ranking does not count connections", stats.ClientVersions)
		}
	}
	// Equal counts use the complete identifier for stable ordering.
	id := mustStart(t, s, "192.0.2.3", time.Now())
	if err = s.Transport(id, "SSH-2.0-client-10"); err != nil {
		t.Fatal(err)
	}
	stats, err = s.Stats(context.Background())
	if err != nil || stats.ClientVersions[0].Count != 12 || stats.ClientVersions[1].Count != 12 || stats.ClientVersions[0].Name != "SSH-2.0-client-10" || stats.ClientVersions[1].Name != "SSH-2.0-client-11" {
		t.Fatal("tie ordering unstable", stats.ClientVersions, err)
	}
}
