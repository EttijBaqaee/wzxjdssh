package main

import (
	"bufio"
	"context"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestOrdinaryDelayJitterBoundsAndConfiguration(t *testing.T) {
	for _, base := range []time.Duration{time.Millisecond, 10 * time.Second, 24 * time.Hour} {
		seen := map[time.Duration]bool{}
		for i := 0; i < 100; i++ {
			got, err := jitterDelay(base, 10)
			low, high := jitterBounds(base, 10)
			if err != nil || got < low || got > high || got < time.Millisecond {
				t.Fatal(base, got, err)
			}
			seen[got] = true
		}
		if len(seen) < 2 {
			t.Fatal("ordinary delay jitter is fixed")
		}
	}
	cfg, err := configEnvironment(DefaultConfig(), func(k string) string {
		if k == "WZXJDSSH_DELAY_JITTER_PERCENT" {
			return "25"
		}
		return ""
	})
	if err != nil || cfg.DelayJitter != 25 {
		t.Fatal(cfg, err)
	}
	for _, value := range []string{"-1", "51", "abc"} {
		if _, err := configEnvironment(DefaultConfig(), func(k string) string {
			if k == "WZXJDSSH_DELAY_JITTER_PERCENT" {
				return value
			}
			return ""
		}); err == nil {
			t.Fatal("bad jitter accepted", value)
		}
	}
}

func TestJitterAppliedToAuthenticationAndBanner(t *testing.T) {
	for _, mode := range []string{"ssh", "banner"} {
		t.Run(mode, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Mode = mode
			cfg.Delay = 200 * time.Millisecond
			cfg.DelayJitter = 50
			s := startTest(t, cfg)
			var measurements []time.Duration
			if mode == "banner" {
				conn := connect(t, cfg)
				defer conn.Close()
				reader := bufio.NewReader(conn)
				for i := 0; i < 4; i++ {
					start := time.Now()
					conn.SetReadDeadline(start.Add(2 * time.Second))
					if _, err := reader.ReadString('\n'); err != nil {
						t.Fatal(err)
					}
					measurements = append(measurements, time.Since(start))
				}
			} else {
				for i := 0; i < 4; i++ {
					start := time.Now()
					c, err := ssh.Dial("tcp", address(cfg), &ssh.ClientConfig{User: "jitter", Auth: []ssh.AuthMethod{ssh.Password("jitter-only")}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 2 * time.Second})
					if c != nil {
						c.Close()
					}
					if err == nil {
						t.Fatal("ordinary authentication accepted")
					}
					measurements = append(measurements, time.Since(start))
				}
			}
			for _, elapsed := range measurements {
				if elapsed < 100*time.Millisecond || elapsed > 600*time.Millisecond {
					t.Fatal(mode, elapsed)
				}
			}
			min, max := measurements[0], measurements[0]
			for _, elapsed := range measurements {
				if elapsed < min {
					min = elapsed
				}
				if elapsed > max {
					max = elapsed
				}
			}
			if max-min < time.Millisecond {
				t.Fatal("waits did not vary", measurements)
			}
			policy := s.Snapshot()["strategy"].(map[string]any)
			if policy["delay_jitter_percent"] != 50 || policy["delay_min_ms"] != int64(100) || policy["delay_max_ms"] != int64(300) {
				t.Fatal(policy)
			}
		})
	}
}

func TestJitteredPauseStillCancels(t *testing.T) {
	cfg := testConfig(t)
	cfg.Delay = time.Hour
	cfg.DelayJitter = 50
	s := startTest(t, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if err := s.pause(ctx); err == nil || time.Since(started) > time.Second {
		t.Fatal(err)
	}
}
