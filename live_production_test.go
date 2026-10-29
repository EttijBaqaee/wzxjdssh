package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestProductionLiveRankings(t *testing.T) {
	target, base := os.Getenv("WZX_TEST_TARGET"), os.Getenv("WZX_TEST_WEB_URL")
	if target == "" || base == "" {
		t.Skip("live production target is not configured")
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	payload, _ := json.Marshal(map[string]string{"username": "admin", "password": os.Getenv("WZXJDSSH_ADMIN_PASSWORD")})
	response, err := client.Post(base+"api/login", "application/json", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("live login", response.StatusCode)
	}
	get := func() LiveStats {
		t.Helper()
		r, err := client.Get(base + "api/stats")
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var data struct {
			Runtime struct {
				Live LiveStats `json:"live"`
			} `json:"runtime"`
		}
		if r.StatusCode != 200 {
			t.Fatal("live stats", r.StatusCode)
		}
		if err = json.NewDecoder(r.Body).Decode(&data); err != nil {
			t.Fatal(err)
		}
		return data.Runtime.Live
	}
	var raw []net.Conn
	done := make(chan error, 3)
	fixture := os.Getenv("WZX_LIVE_FIXTURE") == "1"
	for i := 0; i < 3; i++ {
		conn, err := net.DialTimeout("tcp", target, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, conn)
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(90 * time.Second))
		go func(conn net.Conn, index int) {
			auth := ssh.Password("live-ranking-check")
			if fixture {
				auth = ssh.RetryableAuthMethod(auth, 6)
			}
			c, _, _, err := ssh.NewClientConn(conn, target, &ssh.ClientConfig{User: fmt.Sprint("live-ranking-", index), Auth: []ssh.AuthMethod{auth}, HostKeyCallback: ssh.InsecureIgnoreHostKey()})
			if c != nil {
				c.Close()
			}
			done <- err
		}(conn, i)
	}
	deadline := time.Now().Add(7 * time.Second)
	var stats LiveStats
	for time.Now().Before(deadline) {
		stats = get()
		if stats.DelayingConnections >= 3 && len(stats.IPConcurrency) > 0 && stats.IPConcurrency[0].Connections >= 3 && len(stats.LongestConnections) >= 3 && stats.LongestConnections[0].DelayedMS > 20 {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if stats.DelayingIPs < 1 || stats.DelayingConnections < 3 || len(stats.LongestConnections) < 3 || stats.LongestConnections[0].DelayedMS <= 20 {
		t.Fatal("live connections absent", stats)
	}
	for i := 1; i < len(stats.LongestConnections); i++ {
		if stats.LongestConnections[i].DelayedMS > stats.LongestConnections[i-1].DelayedMS {
			t.Fatal("production delay ranking unsorted")
		}
	}
	before := stats.LongestConnections[0].DelayedMS
	time.Sleep(50 * time.Millisecond)
	if get().LongestConnections[0].DelayedMS <= before {
		t.Fatal("live delay did not advance")
	}
	if fixture {
		os.WriteFile("/results/live-ready", []byte("ready"), 0600)
		until := time.Now().Add(45 * time.Second)
		for time.Now().Before(until) {
			if _, err := os.Stat("/results/live-browser-done"); err == nil {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	for _, conn := range raw {
		conn.Close()
	}
	for range raw {
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("live authentication accepted")
			}
		case <-time.After(15 * time.Second):
			t.Fatal("live client did not close")
		}
	}
	deadline = time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		stats = get()
		if stats.DelayingConnections == 0 && len(stats.LongestConnections) == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if stats.DelayingConnections != 0 || len(stats.LongestConnections) != 0 {
		t.Fatal("disconnected connections remained in live rankings", stats)
	}
	t.Log("real SSH concurrency, IP counts, increasing delay, ordering and disconnect removal verified")
}
