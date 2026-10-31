package main

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestIPGroupingFiltersAndBoundaryPages(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	ctx := context.Background()
	for i := 0; i < 32; i++ {
		id := mustStart(t, s, "2001:db8:ffff:ffff:ffff:ffff:ffff:ffff", now.Add(time.Duration(i)*time.Second))
		mustAttempt(t, s, id, now, "same-ip-user", "password")
	}
	for i := 0; i < 26; i++ {
		mustStart(t, s, fmt.Sprintf("192.0.2.%d", i+1), now.Add(-time.Duration(i+1)*time.Minute))
	}
	first, err := s.ListIPs(ctx, RecordFilter{Limit: 25})
	if err != nil || first.Total != 27 || len(first.Items) != 25 || first.Items[0].Connections != 32 || first.Items[0].AuthAttempts != 32 || first.Items[0].Active != 32 {
		t.Fatal(first, err)
	}
	last, err := s.ListIPs(ctx, RecordFilter{Limit: 25, Last: true})
	if err != nil || last.Page != 2 || len(last.Items) != 2 || last.Items[1].IP != "192.0.2.26" {
		t.Fatal(last, err)
	}
	filtered, err := s.ListIPs(ctx, RecordFilter{Username: "same-ip-user", Last: true})
	if err != nil || filtered.Total != 1 || filtered.Page != 1 {
		t.Fatal(filtered, err)
	}
	page, err := s.List(ctx, RecordFilter{IP: first.Items[0].IP, Last: true})
	if err != nil || page.Total != 32 || page.Page != 2 || len(page.Items) != 7 {
		t.Fatal(page, err)
	}
}

func TestPasswordRankingAndPersistentIPDelay(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()
	a := mustStart(t, s, "192.0.2.1", now)
	b := mustStart(t, s, "192.0.2.1", now)
	c := mustStart(t, s, "2001:db8::1", now)
	for _, attempt := range []AuthRecord{
		{ConnectionID: a, At: now.UnixMilli(), Method: "password", Credential: "<img src=x>"},
		{ConnectionID: a, At: now.UnixMilli(), Method: "password", Credential: "<img src=x>"},
		{ConnectionID: b, At: now.UnixMilli(), Method: "keyboard-interactive", Credential: `["<img src=x>"]`},
		{ConnectionID: b, At: now.UnixMilli(), Method: "keyboard-interactive", Credential: `["otp","answer"]`},
		{ConnectionID: b, At: now.UnixMilli(), Method: "keyboard-interactive", Credential: `not json`},
		{ConnectionID: c, At: now.UnixMilli(), Method: "password", Credential: ""},
		{ConnectionID: c, At: now.UnixMilli(), Method: "publickey_probe", Credential: "never-password"},
	} {
		attempt.Encoding = "utf8"
		if _, err := s.Attempt(attempt); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := s.Stats(ctx)
	if err != nil || len(stats.Passwords) != 2 || stats.Passwords[0].Name != "<img src=x>" || stats.Passwords[0].Count != 3 {
		t.Fatal(stats.Passwords, err)
	}
	if err = s.finish(a, time.Second, "closed", "", 0, 0, 400*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err = s.finish(b, time.Second, "closed", "", 0, 0, 300*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	rank, err := s.DelayRanking(ctx, []liveDelay{{ID: a, DelayedMS: 400, DurationMS: 2000}, {ID: c, DelayedMS: 600, DurationMS: 2000}, {ID: c, DelayedMS: 500, DurationMS: 1500}})
	if err != nil || len(rank) != 2 || rank[0].IP != "2001:db8::1" || rank[0].LongestMS != 2000 || rank[0].LongestConnectionID != c || rank[0].DelayedMS != 600 || rank[1].DelayedMS != 700 {
		t.Fatal(rank, err)
	}
	// An overlay must not double-count a connection that finished during stats.
	if rank[1].LongestMS != 1000 || rank[1].MaxDelayedMS != 400 {
		t.Fatal(rank)
	}
}

func TestWriteBrowserIPFixture(t *testing.T) {
	path := os.Getenv("WZX_UI_FIXTURE_PATH")
	if path == "" {
		t.Skip("browser fixture not requested")
	}
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var existing int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM connections`).Scan(&existing); err != nil {
		t.Fatal(err)
	}
	if existing > 0 {
		return
	}
	now := time.Now().Add(-time.Hour)
	for _, ip := range []string{"8.8.8.8", "223.5.5.5", "2606:4700:4700::1111"} {
		id := mustStart(t, s, ip, now)
		if err = s.finish(id, 43*time.Minute+54*time.Second, "closed", "peer_closed", 10, 20, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 32; i++ {
		id := mustStart(t, s, fmt.Sprintf("192.0.2.%d", i+1), now.Add(time.Duration(i)*time.Second))
		if err = s.finish(id, time.Minute, "auth_rejected", "authentication_failed", 10, 20, time.Duration(i+1)*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	ip := "2001:db8:ffff:ffff:ffff:ffff:ffff:ffff"
	for i := 0; i < 35; i++ {
		id, err := s.Start(fmt.Sprintf("[%s]:%d", ip, 40000+i), ip, "ssh", now.Add(time.Duration(40+i)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.Attempt(AuthRecord{ConnectionID: id, At: now.UnixMilli(), Username: "fixture-user", Method: "password", Credential: "<img src=x onerror=alert(1)>", Encoding: "utf8"}); err != nil {
			t.Fatal(err)
		}
		if err = s.MarkParallelConnection(id); err != nil {
			t.Fatal(err)
		}
		if err = s.finish(id, time.Minute, "auth_rejected", "authentication_failed", 10, 20, time.Duration(i+10)*time.Second); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIPLongestLifetimeMatchesDetailNotImposedDelay(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()
	ip := "77.239.124.161"
	short := mustStart(t, s, ip, now.Add(-10*time.Minute))
	longest := mustStart(t, s, ip, now.Add(-time.Hour))
	if err := s.finish(short, 8*time.Minute, "closed", "peer_closed", 0, 0, 7*time.Minute+8*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := s.finish(longest, 43*time.Minute+54*time.Second, "simulated_closed", "peer_closed", 0, 0, time.Minute); err != nil {
		t.Fatal(err)
	}
	rank, err := s.DelayRanking(ctx, nil)
	if err != nil || len(rank) != 1 || rank[0].LongestConnectionID != longest || rank[0].LongestMS != (43*time.Minute+54*time.Second).Milliseconds() || rank[0].DelayedMS != (8*time.Minute+8*time.Second).Milliseconds() {
		t.Fatal(rank, err)
	}
	page, err := s.ListIPs(ctx, RecordFilter{IP: ip})
	if err != nil || page.Items[0].LongestMS != rank[0].LongestMS {
		t.Fatal(page, err)
	}
}
