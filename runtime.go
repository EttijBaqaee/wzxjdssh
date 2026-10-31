package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

type RuntimeConfig struct {
	Geo           GeoConfig
	DatabasePath  string
	RetentionDays int
	DatabaseMaxMB int64
	WebEnabled    bool
	WebListen     string
	WebBasePath   string
	AdminUsername string
	AdminPassword string
	SecureCookie  bool
}

func runtimeConfig() (RuntimeConfig, error) {
	c := RuntimeConfig{DatabasePath: "/var/lib/wzxjdssh/records.sqlite", RetentionDays: 90, WebEnabled: true, WebListen: ":2223", WebBasePath: "", AdminUsername: "admin"}
	for name, dst := range map[string]*string{"WZXJDSSH_DATABASE_PATH": &c.DatabasePath, "WZXJDSSH_WEB_LISTEN": &c.WebListen, "WZXJDSSH_ADMIN_PASSWORD": &c.AdminPassword} {
		if v, ok := os.LookupEnv(name); ok {
			*dst = v
		}
	}
	if v := os.Getenv("WZXJDSSH_ADMIN_USERNAME"); v != "" {
		c.AdminUsername = v
	}
	if v := os.Getenv("WZXJDSSH_RETENTION_DAYS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 36500 {
			return c, errors.New("WZXJDSSH_RETENTION_DAYS must be 1..36500")
		}
		c.RetentionDays = n
	}
	if v := os.Getenv("WZXJDSSH_DATABASE_MAX_MB"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 || n > 1048576 {
			return c, errors.New("WZXJDSSH_DATABASE_MAX_MB must be 0..1048576 (0 disables the size limit)")
		}
		c.DatabaseMaxMB = n
	}
	for name, dst := range map[string]*bool{"WZXJDSSH_WEB_ENABLED": &c.WebEnabled, "WZXJDSSH_WEB_SECURE_COOKIE": &c.SecureCookie} {
		if v := os.Getenv(name); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				return c, fmt.Errorf("%s must be true or false", name)
			}
			*dst = b
		}
	}
	base := os.Getenv("WZXJDSSH_WEB_BASE_PATH")
	if base != "" && base != "/" {
		base = strings.TrimSuffix(base, "/")
		if !strings.HasPrefix(base, "/") || strings.Contains(base, "//") {
			return c, errors.New("WZXJDSSH_WEB_BASE_PATH must be an absolute URL path")
		}
		for _, part := range strings.Split(base[1:], "/") {
			if part == "" || part == "." || part == ".." {
				return c, errors.New("invalid Web UI base path")
			}
			for _, b := range []byte(part) {
				if !((b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '-' || b == '_') {
					return c, errors.New("Web UI path supports letters, digits, hyphen and underscore")
				}
			}
		}
		c.WebBasePath = base
	}
	if c.DatabasePath == "" {
		return c, errors.New("WZXJDSSH_DATABASE_PATH cannot be empty")
	}
	var geoErr error
	c.Geo, geoErr = geoConfig(c.DatabasePath, os.Getenv)
	if geoErr != nil {
		return c, geoErr
	}
	if c.WebEnabled {
		if len(c.AdminPassword) < 12 {
			return c, errors.New("set WZXJDSSH_ADMIN_PASSWORD to at least 12 bytes in Compose environment")
		}
		if c.AdminUsername == "" || len(c.AdminUsername) > 128 {
			return c, errors.New("invalid WZXJDSSH_ADMIN_USERNAME")
		}
		_, port, err := net.SplitHostPort(c.WebListen)
		if err != nil {
			return c, errors.New("invalid WZXJDSSH_WEB_LISTEN")
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return c, errors.New("invalid Web UI port")
		}
	}
	return c, nil
}
