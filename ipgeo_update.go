package main

import (
	"bufio"
	"compress/gzip"
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/oschwald/maxminddb-golang"
)

func validateGeoURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return errors.New("use an HTTPS database URL without embedded credentials or fragment")
	}
	return nil
}

func (g *GeoDB) RequestUpdate() bool {
	if g == nil || !g.cfg.Enabled || g.ctx.Err() != nil || !g.updating.CompareAndSwap(false, true) {
		return false
	}
	select {
	case g.requests <- struct{}{}:
		return true
	case <-g.ctx.Done():
		g.updating.Store(false)
		return false
	}
}

func (g *GeoDB) maintain() {
	defer g.wg.Done()
	check := func() {
		if g.cfg.AutoUpdate && g.updating.CompareAndSwap(false, true) {
			g.update(false)
		}
	}
	check()
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-g.ctx.Done():
			return
		case <-ticker.C:
			check()
		case <-g.requests:
			g.update(true)
		}
	}
}
func (g *GeoDB) update(force bool) {
	defer g.updating.Store(false)
	for i := range geoNames {
		if g.ctx.Err() != nil {
			return
		}
		g.mu.Lock()
		d := g.databases[i]
		g.mu.Unlock()
		if !force && d.Ready && time.Since(time.UnixMilli(d.UpdatedAt)) < time.Duration(g.cfg.UpdateDays)*24*time.Hour {
			continue
		}
		var err error
		for _, source := range g.cfg.URLs[i] {
			err = g.download(g.ctx, i, source)
			if err == nil || g.ctx.Err() != nil {
				break
			}
		}
		if err != nil {
			g.mu.Lock()
			g.databases[i].Error = err.Error()
			g.mu.Unlock()
		}
	}
}

// Download and validate a sibling temporary file before replacing the live
// database. The old reader and cache remain usable throughout network IO.
func (g *GeoDB) download(ctx context.Context, index int, source string) error {
	if err := os.MkdirAll(g.cfg.Directory, 0700); err != nil {
		return errors.New("无法写入 IP 数据库目录")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return errors.New("数据库下载地址无效")
	}
	req.Header.Set("User-Agent", "wzxjdssh/"+version)
	resp, err := g.client.Do(req)
	if err != nil {
		return errors.New("数据库下载失败或超时；原库继续可用")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("数据库下载返回 HTTP %d；原库继续可用", resp.StatusCode)
	}
	const limit int64 = 256 * 1024 * 1024
	if resp.ContentLength > limit {
		return errors.New("数据库下载超过 256 MiB 限制")
	}
	input := bufio.NewReader(io.LimitReader(resp.Body, limit+1))
	header, _ := input.Peek(2)
	var reader io.Reader = input
	if len(header) == 2 && header[0] == 0x1f && header[1] == 0x8b {
		zipped, err := gzip.NewReader(input)
		if err != nil {
			return errors.New("数据库压缩格式无效")
		}
		defer zipped.Close()
		reader = zipped
	}
	f, err := os.CreateTemp(g.cfg.Directory, ".ipdb-*.tmp")
	if err != nil {
		return errors.New("无法创建数据库临时文件")
	}
	path := f.Name()
	defer os.Remove(path)
	n, copyErr := io.Copy(f, io.LimitReader(reader, limit+1))
	syncErr := f.Sync()
	closeErr := f.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil || n > limit {
		return errors.New("数据库下载不完整或超过 256 MiB 限制")
	}
	r, err := maxminddb.Open(path)
	if err != nil {
		return errors.New("下载内容不是有效 MMDB，保留原库")
	}
	defer func() {
		if r != nil {
			r.Close()
		}
	}()
	err = verifyGeoReader(index, r)
	if err != nil {
		return fmt.Errorf("MMDB 类型或完整性校验失败，保留原库: %w", err)
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return errors.New("无法读取数据库临时文件")
	}
	target := filepath.Join(g.cfg.Directory, geoNames[index]+".mmdb")
	// Keep the already verified reader open across rename; no second open can
	// fail after the on-disk old database has been replaced.
	g.mu.Lock()
	if err = os.Rename(path, target); err == nil {
		if old := g.databases[index].reader; old != nil {
			old.Close()
		}
		g.databases[index] = geoDatabase{Name: geoNames[index], Ready: true, BuildAt: int64(r.Metadata.BuildEpoch) * 1000, UpdatedAt: info.ModTime().UnixMilli(), Bytes: info.Size(), reader: r}
		r = nil
		g.cache = map[string]*list.Element{}
		g.order.Init()
	}
	g.mu.Unlock()
	if err != nil {
		return errors.New("无法替换 IP 数据库，检查持久化目录权限")
	}
	if dir, err := os.Open(g.cfg.Directory); err == nil {
		_ = dir.Sync()
		dir.Close()
	}
	return nil
}
