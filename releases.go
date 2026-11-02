package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/semver"
)

const latestReleasePage = "https://github.com/EttijBaqaee/wzxjdssh/releases/latest"
const latestReleaseAPI = "https://api.github.com/repos/EttijBaqaee/wzxjdssh/releases/latest"

type ReleaseStatus struct {
	Current         string `json:"current"`
	Latest          string `json:"latest"`
	UpdateAvailable bool   `json:"update_available"`
	Checking        bool   `json:"checking"`
	CheckedAt       int64  `json:"checked_at"`
	Error           string `json:"error,omitempty"`
	URL             string `json:"url"`
}

type ReleaseChecker struct {
	mu        sync.Mutex
	wg        sync.WaitGroup
	ctx       context.Context
	cancel    context.CancelFunc
	closed    bool
	client    *http.Client
	endpoint  string
	now       func() time.Time
	next      time.Time
	attempted time.Time
	etag      string
	status    ReleaseStatus
}

func NewReleaseChecker(current string) *ReleaseChecker {
	ctx, cancel := context.WithCancel(context.Background())
	return &ReleaseChecker{ctx: ctx, cancel: cancel, client: &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, endpoint: latestReleaseAPI, now: time.Now, status: ReleaseStatus{Current: current, URL: latestReleasePage}}
}

func releaseVersion(s string) string {
	if len(s) > 128 {
		return ""
	}
	return semver.Canonical("v" + strings.TrimPrefix(strings.TrimSpace(s), "v"))
}

// Administrative polling never waits for GitHub. One shared in-flight request
// and a global cooldown bound both outgoing work and manual refresh attempts.
func (c *ReleaseChecker) Refresh(force bool) ReleaseStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if releaseVersion(c.status.Current) == "" {
		c.status.Error = "当前构建没有可比较的版本号"
		return c.status
	}
	if c.closed || c.status.Checking || now.Before(c.next) && (!force || now.Before(c.attempted.Add(30*time.Second))) {
		return c.status
	}
	c.status.Checking = true
	c.status.Error = ""
	c.attempted = now
	c.wg.Add(1)
	go c.fetch(c.etag)
	return c.status
}

func (c *ReleaseChecker) Snapshot() ReleaseStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

func (c *ReleaseChecker) Close() {
	c.mu.Lock()
	c.closed = true
	c.cancel()
	c.mu.Unlock()
	c.wg.Wait()
}

func (c *ReleaseChecker) fetch(etag string) {
	defer c.wg.Done()
	tag, nextETag, err := c.query(etag)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status.Checking = false
	if c.closed {
		return
	}
	now := c.now()
	if err != nil {
		c.status.Error = err.Error()
		c.next = now.Add(10 * time.Minute)
		return // Retain the last known result if a later request fails.
	}
	if tag != "" {
		c.status.Latest = tag
	}
	c.etag = nextETag
	c.status.UpdateAvailable = semver.Compare(releaseVersion(c.status.Latest), releaseVersion(c.status.Current)) > 0
	c.status.CheckedAt = now.UnixMilli()
	c.status.Error = ""
	c.next = now.Add(time.Hour)
}

func (c *ReleaseChecker) query(etag string) (string, string, error) {
	req, err := http.NewRequestWithContext(c.ctx, http.MethodGet, c.endpoint, nil)
	if err != nil {
		return "", "", errors.New("无法创建版本检查请求")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "wzxjdssh/"+c.status.Current)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", "", errors.New("无法连接 GitHub，请稍后重试")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified && etag != "" {
		return "", etag, nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", errors.New("GitHub 暂未提供版本信息，请稍后重试")
	}
	const limit = 512 * 1024
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(data) > limit {
		return "", "", errors.New("GitHub 版本响应无法读取")
	}
	var release struct {
		TagName    string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if json.Unmarshal(data, &release) != nil || release.Draft || release.Prerelease || releaseVersion(release.TagName) == "" || strings.Contains(releaseVersion(release.TagName), "-") {
		return "", "", errors.New("GitHub 未返回有效的正式版本")
	}
	nextETag := resp.Header.Get("ETag")
	if len(nextETag) > 256 {
		nextETag = ""
	}
	return release.TagName, nextETag, nil
}

func (w *Web) releaseState(rw http.ResponseWriter, r *http.Request) {
	sendJSON(rw, 200, w.releases.Refresh(false))
}
func (w *Web) releaseCheck(rw http.ResponseWriter, r *http.Request) {
	sendJSON(rw, 200, w.releases.Refresh(true))
}
