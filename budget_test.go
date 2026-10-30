package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestDatabaseCapacityEvictsOldestAndReclaimsFiles(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	ids := []int64{}
	for i := 0; i < 36; i++ {
		id := mustStart(t, s, "192.0.2.1", now.Add(time.Duration(i)*time.Second))
		ids = append(ids, id)
		_, err := s.Attempt(AuthRecord{ConnectionID: id, At: now.UnixMilli(), Username: "size-test", Method: "password", Credential: strings.Repeat("x", 60000), Encoding: "utf8"})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Finish(id, time.Second, "closed", "", 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	before, err := s.DatabaseBytes()
	if err != nil || before < 1024*1024 {
		t.Fatal(before, err)
	}
	if err = s.SetDatabaseBudget(1024 * 1024); err != nil {
		t.Fatal(err)
	}
	after, err := s.DatabaseBytes()
	if err != nil || after > 1024*1024 || after >= before {
		t.Fatal(before, after, err)
	}
	if _, _, err = s.Detail(context.Background(), ids[0]); err == nil {
		t.Fatal("oldest survived")
	}
	if _, a, err := s.Detail(context.Background(), ids[len(ids)-1]); err != nil || len(a) != 1 {
		t.Fatal("newest removed", err)
	}
	var orphans int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM auth_attempts WHERE connection_id NOT IN (SELECT id FROM connections)`).Scan(&orphans); err != nil || orphans != 0 {
		t.Fatal(orphans, err)
	}
	var integrity string
	if err = s.db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatal(integrity, err)
	}
	// Disabled is a true opt-out, including after an earlier limit was active.
	if err = s.SetDatabaseBudget(0); err != nil {
		t.Fatal(err)
	}
	remaining := s.capacityDeleted.Load()
	for i := 0; i < 24; i++ {
		id := mustStart(t, s, "192.0.2.2", now)
		if _, err = s.Attempt(AuthRecord{ConnectionID: id, At: now.UnixMilli(), Method: "password", Credential: strings.Repeat("y", 60000)}); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.EnforceBudget(); err != nil || s.capacityDeleted.Load() != remaining {
		t.Fatal("disabled cleanup", err)
	}
}

func TestCapacityPreservesLiveConnectionAndCleansItsOldAttempts(t *testing.T) {
	s := testStore(t)
	id := mustStart(t, s, "2001:db8::1", time.Now())
	for i := 0; i < 30; i++ {
		if _, err := s.Attempt(AuthRecord{ConnectionID: id, At: int64(i), Method: "password", Credential: strings.Repeat("z", 60000)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetDatabaseBudget(1024 * 1024); err != nil {
		t.Fatal(err)
	}
	c, a, err := s.Detail(context.Background(), id)
	if err != nil || c.EndedAt != nil || c.AuthAttempts != int64(len(a)) || len(a) >= 30 {
		t.Fatal(c, len(a), err)
	}
	// Per-write enforcement prevents a rapid flood from outrunning maintenance.
	for i := 0; i < 30; i++ {
		if _, err = s.Attempt(AuthRecord{ConnectionID: id, At: time.Now().UnixMilli(), Method: "password", Credential: strings.Repeat("n", 60000)}); err != nil {
			t.Fatal(err)
		}
		size, err := s.DatabaseBytes()
		if err != nil || size > 1024*1024 {
			t.Fatal(size, err)
		}
	}
}

func TestBudgetConfiguration(t *testing.T) {
	t.Setenv("WZXJDSSH_WEB_ENABLED", "false")
	for _, value := range []string{"", "0", "1024"} {
		t.Setenv("WZXJDSSH_DATABASE_MAX_MB", value)
		c, err := runtimeConfig()
		if err != nil {
			t.Fatal(c, err)
		}
		if value != "1024" && c.DatabaseMaxMB != 0 {
			t.Fatal(c)
		}
	}
	for _, value := range []string{"-1", "1.5", "abc", "1048577"} {
		t.Setenv("WZXJDSSH_DATABASE_MAX_MB", value)
		if _, err := runtimeConfig(); err == nil {
			t.Fatal(value)
		}
	}
}
