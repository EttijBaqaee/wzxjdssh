package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

func (c *RuntimeConfig) configureVisitor(lookup func(string) string) error {
	c.TrustedProxies = nil
	c.VisitorIPURL = strings.TrimSpace(lookup("WZXJDSSH_VISITOR_IP_URL"))
	if c.VisitorIPURL == "" {
		c.VisitorIPURL = "/get-my-ip"
	}
	if _, err := visitorEndpointOrigin(c.VisitorIPURL); err != nil {
		return fmt.Errorf("WZXJDSSH_VISITOR_IP_URL: %w", err)
	}
	proxies := strings.TrimSpace(lookup("WZXJDSSH_TRUSTED_PROXIES"))
	if proxies == "" {
		proxies = "127.0.0.1/32,::1/128"
	}
	if proxies == "none" {
		return nil
	}
	parts := strings.Split(proxies, ",")
	if len(parts) > 64 {
		return errors.New("WZXJDSSH_TRUSTED_PROXIES supports at most 64 addresses or CIDRs")
	}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		prefix, err := netip.ParsePrefix(part)
		if err != nil {
			addr, parseErr := netip.ParseAddr(part)
			if parseErr != nil || addr.Zone() != "" {
				return errors.New("WZXJDSSH_TRUSTED_PROXIES must contain IP addresses or CIDRs, separated by commas")
			}
			addr = addr.Unmap()
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		if prefix.Addr().Is4In6() {
			if prefix.Bits() < 96 {
				return errors.New("IPv4-mapped trusted proxy CIDR must have at least 96 prefix bits")
			}
			prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
		}
		c.TrustedProxies = append(c.TrustedProxies, prefix.Masked())
	}
	return nil
}

// Only the configured origin is added to connect-src. The server never fetches
// the visitor endpoint: it must observe the browser's network path, not ours.
func visitorEndpointOrigin(raw string) (string, error) {
	if len(raw) > 2048 || strings.ContainsAny(raw, "\r\n\t \\") {
		return "", errors.New("use an absolute HTTP(S) URL or a same-origin path starting with /")
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return "", errors.New("invalid visitor IP endpoint")
	}
	if u.Scheme == "" && u.Host == "" && strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//") {
		return "", nil
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
		return "", errors.New("use an absolute HTTP(S) URL or a same-origin path starting with /")
	}
	for _, r := range u.Host {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune(".-_[]:", r)) {
			return "", errors.New("invalid visitor endpoint host")
		}
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", errors.New("invalid visitor endpoint port")
		}
	}
	return u.Scheme + "://" + u.Host, nil
}

func visitorAddress(raw string) (netip.Addr, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 128 {
		return netip.Addr{}, false
	}
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		if host, _, splitErr := net.SplitHostPort(raw); splitErr == nil {
			addr, err = netip.ParseAddr(host)
		} else if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
			addr, err = netip.ParseAddr(raw[1 : len(raw)-1])
		}
	}
	if err != nil || addr.Zone() != "" {
		return netip.Addr{}, false
	}
	addr = addr.Unmap()
	return addr, !addr.IsUnspecified() && !addr.IsMulticast()
}

func (w *Web) trustedVisitorProxy(addr netip.Addr) bool {
	for _, prefix := range w.cfg.TrustedProxies {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func (w *Web) requestVisitorIP(r *http.Request) (string, string) {
	peer, ok := visitorAddress(r.RemoteAddr)
	if !ok {
		return "", "unavailable"
	}
	if !w.trustedVisitorProxy(peer) {
		return peer.String(), "socket"
	}
	forwarded := strings.Join(r.Header.Values("X-Forwarded-For"), ",")
	if forwarded != "" {
		parts := strings.Split(forwarded, ",")
		if len(forwarded) > 4096 || len(parts) > 32 {
			return peer.String(), "socket"
		}
		chain := make([]netip.Addr, 0, len(parts))
		for _, part := range parts {
			addr, valid := visitorAddress(part)
			if !valid {
				return peer.String(), "socket"
			}
			chain = append(chain, addr)
		}
		// Walk from the nearest proxy; ignore any spoofed values to the left of
		// the first untrusted hop. Never blindly accept the first XFF value.
		for i := len(chain) - 1; i >= 0; i-- {
			if !w.trustedVisitorProxy(chain[i]) || i == 0 {
				return chain[i].String(), "forwarded"
			}
		}
	}
	if real, valid := visitorAddress(r.Header.Get("X-Real-IP")); valid {
		return real.String(), "real_ip"
	}
	return peer.String(), "socket"
}

func (w *Web) visitor(rw http.ResponseWriter, r *http.Request) {
	serverIP, source := w.requestVisitorIP(r)
	ip, verification, endpointIP := serverIP, "single", ""
	if values, supplied := r.URL.Query()["ip"]; supplied {
		if len(values) != 1 {
			sendJSON(rw, http.StatusBadRequest, map[string]string{"error": "无效 IP"})
			return
		}
		addr, valid := visitorAddress(values[0])
		if !valid {
			sendJSON(rw, http.StatusBadRequest, map[string]string{"error": "无效 IP"})
			return
		}
		endpointIP, ip = addr.String(), addr.String()
		verification = "mismatch"
		if serverIP == "" {
			verification = "single"
		} else if serverIP == endpointIP {
			verification = "match"
		}
	}
	geo := IPAttribution{Status: "invalid"}
	if ip != "" {
		geo = w.geo.Lookup(ip)
	}
	sendJSON(rw, http.StatusOK, map[string]any{
		"ip": ip, "geo": geo, "server_ip": serverIP, "server_source": source,
		"endpoint_ip": endpointIP, "endpoint_url": w.cfg.VisitorIPURL, "verification": verification,
	})
}
