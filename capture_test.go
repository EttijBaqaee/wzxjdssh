package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestProductionCredentialCapture(t *testing.T) {
	target, base := os.Getenv("WZX_CAPTURE_SSH"), os.Getenv("WZX_CAPTURE_WEB")
	if target == "" || base == "" {
		t.Skip("published credential capture target is not configured")
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 15 * time.Second}
	payload, _ := json.Marshal(map[string]string{"username": "admin", "password": os.Getenv("WZXJDSSH_ADMIN_PASSWORD")})
	r, err := client.Post(base+"api/login", "application/json", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatal("administrator login", r.StatusCode)
	}
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
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(private)
	cases := []struct {
		user, method, password string
		auth                   ssh.AuthMethod
	}{{"ui-password-check", "password", "wzxjdssh-ui-password-check", ssh.Password("wzxjdssh-ui-password-check")}, {"ui-publickey-check", "publickey_probe", "", ssh.PublicKeys(signer)}}
	ids := map[string]int64{}
	for _, tc := range cases {
		conn, err := ssh.Dial("tcp", target, &ssh.ClientConfig{User: tc.user, Auth: []ssh.AuthMethod{tc.auth}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 20 * time.Second})
		if conn != nil {
			conn.Close()
		}
		if err == nil {
			t.Fatal("honeypot accepted authentication")
		}
		var list RecordPage
		get("api/connections?username="+url.QueryEscape(tc.user), &list)
		if len(list.Items) == 0 {
			t.Fatal("connection not recorded", tc.user)
		}
		var detail struct {
			Attempts []AuthRecord `json:"attempts"`
		}
		id := list.Items[0].ID
		get(fmt.Sprint("api/connections/", id), &detail)
		if len(detail.Attempts) != 1 {
			t.Fatal("authentication missing", tc.user)
		}
		a := detail.Attempts[0]
		if a.Username != tc.user || a.Method != tc.method {
			t.Fatal("wrong authentication record", a.Username, a.Method)
		}
		if tc.method == "password" && a.Credential != tc.password {
			t.Fatal("password was not recorded")
		}
		if tc.method == "publickey_probe" && (a.PublicKey != string(ssh.MarshalAuthorizedKey(signer.PublicKey())) || a.Fingerprint != ssh.FingerprintSHA256(signer.PublicKey())) {
			t.Fatal("public key was not recorded")
		}
		ids[tc.method] = id
	}
	data, _ := json.MarshalIndent(ids, "", "  ")
	if err = os.WriteFile("/results/credential-capture-v2.json", data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Log("published SSH password and public key captured and readable through the Web API")
}
