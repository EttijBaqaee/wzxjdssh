package main

import (
	"container/list"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oschwald/maxminddb-golang"
)

//go:embed geodata/cn-divisions.json
var divisionFiles embed.FS

// GeoIP is display-only enrichment. No lookup or download runs in an SSH worker.
type GeoConfig struct {
	Enabled    bool
	Directory  string
	AutoUpdate bool
	UpdateDays int
	CacheSize  int
	CacheTTL   time.Duration
	URLs       [3][]string
}

var geoNames = [3]string{"GeoLite2-City", "GeoLite2-ASN", "GeoCN"}

func geoConfig(databasePath string, lookup func(string) string) (GeoConfig, error) {
	c := GeoConfig{Enabled: true, Directory: filepath.Join(filepath.Dir(databasePath), "ipdb"), AutoUpdate: true, UpdateDays: 45, CacheSize: 300, CacheTTL: 5 * time.Minute}
	for key, dst := range map[string]*bool{"ENABLED": &c.Enabled, "AUTO_UPDATE": &c.AutoUpdate} {
		if v := lookup("WZXJDSSH_IPDB_" + key); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				return c, fmt.Errorf("WZXJDSSH_IPDB_%s must be true or false", key)
			}
			*dst = b
		}
	}
	if v := lookup("WZXJDSSH_IPDB_DIRECTORY"); v != "" {
		c.Directory = v
	}
	for _, o := range []struct {
		key      string
		dst      *int
		min, max int
	}{{"UPDATE_DAYS", &c.UpdateDays, 1, 365}, {"CACHE_SIZE", &c.CacheSize, 1, 10000}} {
		if v := lookup("WZXJDSSH_IPDB_" + o.key); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < o.min || n > o.max {
				return c, fmt.Errorf("WZXJDSSH_IPDB_%s must be %d..%d", o.key, o.min, o.max)
			}
			*o.dst = n
		}
	}
	if v := lookup("WZXJDSSH_IPDB_CACHE_TTL_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 86400 {
			return c, fmt.Errorf("WZXJDSSH_IPDB_CACHE_TTL_SECONDS must be 1..86400")
		}
		c.CacheTTL = time.Duration(n) * time.Second
	}
	for i, key := range []string{"CITY_URL", "ASN_URL", "GEOCN_URL"} {
		if v := lookup("WZXJDSSH_IPDB_" + key); v != "" {
			if err := validateGeoURL(v); err != nil {
				return c, fmt.Errorf("WZXJDSSH_IPDB_%s: %w", key, err)
			}
			c.URLs[i] = []string{v}
		} else {
			c.URLs[i] = []string{"https://raw.githubusercontent.com/PBH-BTN/GeoLite.mmdb/download/" + geoNames[i] + ".mmdb.gz", "https://github.com/PBH-BTN/GeoLite.mmdb/releases/latest/download/" + geoNames[i] + ".mmdb.gz"}
		}
	}
	return c, nil
}

type IPAttribution struct {
	Status       string `json:"status"`
	CountryISO   string `json:"country_iso,omitempty"`
	Country      string `json:"country,omitempty"`
	Province     string `json:"province,omitempty"`
	City         string `json:"city,omitempty"`
	District     string `json:"district,omitempty"`
	ISP          string `json:"isp,omitempty"`
	NetworkType  string `json:"network_type,omitempty"`
	ASN          uint   `json:"asn,omitempty"`
	Organization string `json:"organization,omitempty"`
	Subnet       string `json:"subnet,omitempty"`
	Source       string `json:"source,omitempty"`
}
type geoCacheEntry struct {
	key   string
	value IPAttribution
	used  time.Time
}
type geoDatabase struct {
	Name      string `json:"name"`
	Ready     bool   `json:"ready"`
	BuildAt   int64  `json:"build_at,omitempty"`
	UpdatedAt int64  `json:"updated_at,omitempty"`
	Bytes     int64  `json:"bytes"`
	Error     string `json:"error,omitempty"`
	reader    *maxminddb.Reader
}
type GeoDB struct {
	cfg       GeoConfig
	mu        sync.Mutex // Covers reader lifetime, cache contents and status snapshots.
	databases [3]geoDatabase
	cache     map[string]*list.Element
	order     *list.List
	divisions map[string]string
	client    *http.Client
	requests  chan struct{}
	updating  atomic.Bool
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

func NewGeoDB(c GeoConfig) *GeoDB {
	ctx, cancel := context.WithCancel(context.Background())
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSHandshakeTimeout = 30 * time.Second
	transport.ResponseHeaderTimeout = time.Minute
	g := &GeoDB{cfg: c, cache: map[string]*list.Element{}, order: list.New(), divisions: map[string]string{}, client: &http.Client{Timeout: 20 * time.Minute, Transport: transport}, requests: make(chan struct{}, 1), ctx: ctx, cancel: cancel}
	raw, _ := divisionFiles.ReadFile("geodata/cn-divisions.json")
	_ = json.Unmarshal(raw, &g.divisions)
	for i, name := range geoNames {
		g.databases[i].Name = name
		if c.Enabled {
			if err := g.load(i, filepath.Join(c.Directory, name+".mmdb")); err != nil {
				g.databases[i].Error = "未找到可用数据库"
			}
		}
	}
	if c.Enabled {
		g.wg.Add(1)
		go g.maintain()
	}
	return g
}
func (g *GeoDB) Close() {
	g.cancel()
	g.wg.Wait()
	g.client.CloseIdleConnections()
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := range g.databases {
		if r := g.databases[i].reader; r != nil {
			r.Close()
			g.databases[i].reader = nil
		}
	}
	g.cache = map[string]*list.Element{}
	g.order.Init()
}

func (g *GeoDB) Lookup(raw string) IPAttribution {
	if g == nil || !g.cfg.Enabled {
		return IPAttribution{Status: "disabled"}
	}
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return IPAttribution{Status: "invalid"}
	}
	addr = addr.WithZone("").Unmap()
	if !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return IPAttribution{Status: "private"}
	}
	key := addr.String()
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if e := g.cache[key]; e != nil {
		c := e.Value.(*geoCacheEntry)
		if now.Sub(c.used) < g.cfg.CacheTTL {
			c.used = now
			g.order.MoveToFront(e)
			return c.value
		}
		g.order.Remove(e)
		delete(g.cache, key)
	}
	out := g.lookupLocked(net.IP(addr.AsSlice()))
	// Unavailable readers are not negative-cached. Loading a database also clears
	// all old entries, including partial results and genuine not-found results.
	if out.Status != "unavailable" {
		e := g.order.PushFront(&geoCacheEntry{key, out, now})
		g.cache[key] = e
		for g.order.Len() > g.cfg.CacheSize {
			last := g.order.Back()
			delete(g.cache, last.Value.(*geoCacheEntry).key)
			g.order.Remove(last)
		}
	}
	return out
}

type geoNamesRecord struct {
	Names map[string]string `maxminddb:"names"`
	ISO   string            `maxminddb:"iso_code"`
}
type geoCityRecord struct {
	Country      geoNamesRecord   `maxminddb:"country"`
	City         geoNamesRecord   `maxminddb:"city"`
	Subdivisions []geoNamesRecord `maxminddb:"subdivisions"`
}
type geoASNRecord struct {
	Number       uint   `maxminddb:"autonomous_system_number"`
	Organization string `maxminddb:"autonomous_system_organization"`
}
type geoCNRecord struct {
	ISP      string `maxminddb:"isp"`
	Type     string `maxminddb:"type"`
	Net      string `maxminddb:"net"`
	Division any    `maxminddb:"division_code"`
	Province string `maxminddb:"province"`
	City     string `maxminddb:"city"`
	District string `maxminddb:"districts"`
}

func geoName(names map[string]string) string {
	if n := names["zh-CN"]; n != "" {
		return n
	}
	return names["en"]
}

func (g *GeoDB) lookupLocked(ip net.IP) IPAttribution {
	out := IPAttribution{Status: "not_found"}
	sources := []string{}
	ready := false
	if r := g.databases[0].reader; r != nil {
		ready = true
		var c geoCityRecord
		if err := r.Lookup(ip, &c); err == nil && (c.Country.ISO != "" || len(c.City.Names) > 0) {
			out.CountryISO = c.Country.ISO
			out.Country = geoName(c.Country.Names)
			out.City = geoName(c.City.Names)
			if len(c.Subdivisions) > 0 {
				out.Province = geoName(c.Subdivisions[0].Names)
			}
			sources = append(sources, "GeoLite2-City")
		}
	}
	if r := g.databases[1].reader; r != nil {
		ready = true
		var a geoASNRecord
		if network, ok, err := r.LookupNetwork(ip, &a); err == nil && ok {
			out.ASN = a.Number
			out.Organization = a.Organization
			out.ISP = a.Organization
			if network != nil {
				out.Subnet = network.String()
			}
			sources = append(sources, "GeoLite2-ASN")
		}
	}
	if r := g.databases[2].reader; r != nil {
		ready = true
		// An absent City database must not disable otherwise usable GeoCN data.
		if out.CountryISO == "" || out.CountryISO == "CN" || out.CountryISO == "HK" || out.CountryISO == "MO" || out.CountryISO == "TW" {
			var c geoCNRecord
			if err := r.Lookup(ip, &c); err == nil && (c.ISP != "" || c.Province != "" || c.Division != nil) {
				province, city, district := c.Province, c.City, c.District
				if c.Division != nil {
					code := fmt.Sprint(c.Division)
					names := []string{}
					for n := 1; n <= len(code); n++ {
						if name := g.divisions[code[:n]]; name != "" {
							names = append(names, name)
						}
					}
					if len(names) > 0 {
						province = names[0]
					}
					if len(names) > 1 {
						city = names[1]
					}
					if len(names) > 2 {
						district = strings.Join(names[2:], " ")
					}
				}
				if province != "" {
					out.Province = province
				}
				if city != "" {
					out.City = city
				}
				if district != "" {
					out.District = district
				}
				if c.ISP != "" {
					out.ISP = c.ISP
				}
				if c.Type != "" {
					out.NetworkType = c.Type
				} else if c.Net != "" {
					out.NetworkType = c.Net
				}
				sources = append(sources, "GeoCN")
			}
		}
	}
	if !ready {
		out.Status = "unavailable"
	} else if len(sources) > 0 {
		out.Status = "found"
	}
	out.Source = strings.Join(sources, " + ")
	return out
}

func (g *GeoDB) Status() map[string]any {
	if g == nil {
		return map[string]any{"enabled": false, "updating": false, "databases": []geoDatabase{}}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return map[string]any{"enabled": g.cfg.Enabled, "auto_update": g.cfg.AutoUpdate, "update_days": g.cfg.UpdateDays, "updating": g.updating.Load(), "cache_entries": len(g.cache), "cache_max": g.cfg.CacheSize, "cache_ttl_seconds": int(g.cfg.CacheTTL / time.Second), "databases": g.databases, "attribution": "GeoLite2: MaxMind; GeoCN: ljxi/GeoCN; divisions: AreaCity-JsSpider-StatsGov", "historical_lookup": "current_local_databases"}
}

func (g *GeoDB) load(index int, path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular MMDB file")
	}
	r, err := maxminddb.Open(path)
	if err != nil {
		return err
	}
	if err = validateGeoReader(index, r); err != nil {
		r.Close()
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if old := g.databases[index].reader; old != nil {
		old.Close()
	}
	g.databases[index] = geoDatabase{Name: geoNames[index], Ready: true, BuildAt: int64(r.Metadata.BuildEpoch) * 1000, UpdatedAt: info.ModTime().UnixMilli(), Bytes: info.Size(), reader: r}
	g.cache = map[string]*list.Element{}
	g.order.Init()
	return nil
}

func validateGeoReader(index int, r *maxminddb.Reader) error {
	kind := strings.ToLower(r.Metadata.DatabaseType)
	expected := []string{"city", "asn", "geocn"}[index]
	if !strings.Contains(kind, expected) {
		return fmt.Errorf("wrong MMDB database type")
	}
	return nil
}

// GeoCN omits the optional description metadata. Reader.Verify is deliberately
// stricter than the MMDB specification here. Normalize only this descriptive
// field on the unpublished reader, retain all tree/data checks, then restore it.
// The downloaded file is never rewritten and other database types stay strict.
func verifyGeoReader(index int, r *maxminddb.Reader) error {
	if err := validateGeoReader(index, r); err != nil {
		return err
	}
	description := r.Metadata.Description
	if index == 2 && len(description) == 0 {
		r.Metadata.Description = map[string]string{"en": "GeoCN"}
		defer func() { r.Metadata.Description = description }()
	}
	return r.Verify()
}
