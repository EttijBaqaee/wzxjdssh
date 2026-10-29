package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestProductionWebAndPersistence(t *testing.T) {
	base := os.Getenv("WZX_TEST_WEB_URL")
	if base == "" {
		t.Skip("production Web target only available in Compose")
	}
	password := os.Getenv("WZXJDSSH_ADMIN_PASSWORD")
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 15 * time.Second}
	get := func(path string, out any) {
		t.Helper()
		r, err := client.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		if r.StatusCode != 200 {
			t.Fatal(path, r.StatusCode)
		}
		if err = json.NewDecoder(r.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	body, _ := json.Marshal(map[string]string{"username": "admin", "password": password})
	r, err := client.Post(base+"api/login", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatal("production login", r.StatusCode)
	}
	var prior struct {
		ID int64 `json:"id"`
	}
	if data, err := os.ReadFile("/results/production-v2-id.json"); err == nil {
		if err = json.Unmarshal(data, &prior); err != nil {
			t.Fatal(err)
		}
		var d struct {
			Attempts []AuthRecord `json:"attempts"`
		}
		get(fmt.Sprint("api/connections/", prior.ID), &d)
		if len(d.Attempts) != 1 || d.Attempts[0].Credential != "production-test-password" {
			t.Fatal("record lost after recreation", d)
		}
	}
	conn, err := ssh.Dial("tcp", os.Getenv("WZX_TEST_TARGET"), &ssh.ClientConfig{User: "v2-production-probe", Auth: []ssh.AuthMethod{ssh.Password("production-test-password")}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 20 * time.Second})
	if conn != nil {
		conn.Close()
	}
	if err == nil {
		t.Fatal("production authentication accepted")
	}
	var records RecordPage
	get("api/connections?username="+url.QueryEscape("v2-production-probe"), &records)
	if records.Total < 1 {
		t.Fatal("SSH attempt missing from Web")
	}
	var detail struct {
		Connection ConnectionRecord `json:"connection"`
		Attempts   []AuthRecord     `json:"attempts"`
	}
	get(fmt.Sprint("api/connections/", records.Items[0].ID), &detail)
	if len(detail.Attempts) != 1 || detail.Attempts[0].Username != "v2-production-probe" || detail.Attempts[0].Credential != "production-test-password" || detail.Connection.BytesReceived == 0 {
		t.Fatal("production SSH record incomplete", detail)
	}
	data, _ := json.Marshal(map[string]int64{"id": records.Items[0].ID})
	if err = os.WriteFile("/results/production-v2-id.json", data, 0600); err != nil {
		t.Fatal(err)
	}
	var before, after struct {
		Stored StoredStats `json:"stored"`
	}
	get("api/stats", &before)
	for i := 0; i < 3; i++ {
		cmd := exec.Command(os.Getenv("WZX_TEST_BINARY"), "-health")
		cmd.Env = append(os.Environ(), "WZXJDSSH_WEB_LISTEN=wzxjdssh:2223", "WZXJDSSH_WEB_BASE_PATH=/sshpanel")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatal("health failed", err, string(out))
		}
	}
	get("api/stats", &after)
	if before.Stored.Connections != after.Stored.Connections {
		t.Fatal("health probes polluted SSH statistics")
	}
	t.Logf("Web prefix/authentication/SSH credential/health verified; prior persistent record=%d, current=%d", prior.ID, records.Items[0].ID)
}
