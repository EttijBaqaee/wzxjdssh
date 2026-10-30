package main

import (
	"context"
	"testing"
	"time"
)

func TestConnectionDurationsUseServerClock(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	live := mustStart(t, s, "192.0.2.10", time.Now().Add(-42*time.Second))
	future := mustStart(t, s, "192.0.2.11", time.Now().Add(time.Hour))
	ended := mustStart(t, s, "192.0.2.12", time.Now().Add(time.Hour))
	if err := s.Finish(ended, 7*time.Second, "closed", "", 0, 0); err != nil {
		t.Fatal(err)
	}
	page, err := s.List(ctx, RecordFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range page.Items {
		if c.DurationMS < 0 {
			t.Fatal("negative duration", c)
		}
		switch c.ID {
		case live:
			if c.DurationMS < 42000 || c.DurationMS > 45000 {
				t.Fatal(c)
			}
		case future:
			if c.DurationMS != 0 {
				t.Fatal(c)
			}
		case ended:
			if c.DurationMS != 7000 {
				t.Fatal(c)
			}
		}
		detail, _, err := s.Detail(ctx, c.ID)
		if err != nil || detail.DurationMS < c.DurationMS {
			t.Fatal(detail, err)
		}
	}
}
