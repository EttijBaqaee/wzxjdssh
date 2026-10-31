package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	"github.com/oschwald/maxminddb-golang"
)

func geoFixture(t *testing.T, dir, name string, records map[string]mmdbtype.Map) string {
	t.Helper()
	writer, err := mmdbwriter.New(mmdbwriter.Options{DatabaseType: name, IPVersion: 6, RecordSize: 24, IncludeReservedNetworks: true, Languages: []string{"en"}, Description: map[string]string{"en": "Synthetic test database"}})
	if err != nil {
		t.Fatal(err)
	}
	for cidr, record := range records {
		_, subnet, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatal(err)
		}
		if err = writer.Insert(subnet, record); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, name+".mmdb")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = writer.WriteTo(f); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}
func geoFixtureConfig(t *testing.T) GeoConfig {
	t.Helper()
	c, err := geoConfig(filepath.Join(t.TempDir(), "records.sqlite"), func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	c.AutoUpdate = false
	if err = os.MkdirAll(c.Directory, 0700); err != nil {
		t.Fatal(err)
	}
	return c
}
func TestGeoMergeIPv6AndBoundedCache(t *testing.T) {
	c := geoFixtureConfig(t)
	c.CacheSize = 2
	names := func(v string) mmdbtype.Map { return mmdbtype.Map{"names": mmdbtype.Map{"zh-CN": mmdbtype.String(v)}} }
	country := names("中国")
	country["iso_code"] = mmdbtype.String("CN")
	geoFixture(t, c.Directory, "GeoLite2-City", map[string]mmdbtype.Map{"203.0.113.0/24": {"country": country, "city": names("旧城市")}, "2001:db8::/32": {"country": country}})
	geoFixture(t, c.Directory, "GeoLite2-ASN", map[string]mmdbtype.Map{"203.0.113.0/24": {"autonomous_system_number": mmdbtype.Uint32(64500), "autonomous_system_organization": mmdbtype.String("Example Network")}})
	geoFixture(t, c.Directory, "GeoCN", map[string]mmdbtype.Map{"203.0.113.0/24": {"province": mmdbtype.String("样例省"), "city": mmdbtype.String("新城市"), "districts": mmdbtype.String("样例区"), "isp": mmdbtype.String("样例运营商"), "net": mmdbtype.String("宽带")}, "2001:db8::/32": {"division_code": mmdbtype.Uint32(110101), "isp": mmdbtype.String("IPv6 ISP"), "type": mmdbtype.String("IDC")}})
	g := NewGeoDB(c)
	defer g.Close()
	a := g.Lookup("::ffff:203.0.113.9")
	if a.Status != "found" || a.City != "新城市" || a.ASN != 64500 || a.Organization != "Example Network" || a.NetworkType != "宽带" || a.Subnet != "203.0.113.0/24" {
		t.Fatal(a)
	}
	b := g.Lookup("2001:db8::1")
	if b.Province != "北京市" || b.City == "" || b.NetworkType != "IDC" {
		t.Fatal(b)
	}
	if g.Lookup("127.0.0.1").Status != "private" || g.Lookup("invalid").Status != "invalid" {
		t.Fatal("address classification")
	}
	g.Lookup("203.0.113.10")
	if len(g.cache) != 2 {
		t.Fatal("cache cap", len(g.cache))
	}
	if _, ok := g.cache["203.0.113.9"]; ok {
		t.Fatal("LRU entry not evicted")
	}
}
func TestGeoExpiryMissingDatabaseAndConcurrentUpdate(t *testing.T) {
	c := geoFixtureConfig(t)
	g := NewGeoDB(c)
	defer g.Close()
	if g.Lookup("203.0.113.1").Status != "unavailable" || len(g.cache) != 0 {
		t.Fatal("cached initialization failure")
	}
	path := geoFixture(t, t.TempDir(), "GeoLite2-ASN", map[string]mmdbtype.Map{"203.0.113.0/24": {"autonomous_system_number": mmdbtype.Uint32(64501)}})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(data) }))
	defer httpServer.Close()
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				g.Lookup("203.0.113.1")
				g.Status()
			}
		}()
	}
	if err = g.download(context.Background(), 1, httpServer.URL); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if g.Lookup("203.0.113.1").ASN != 64501 {
		t.Fatal("new reader/cache unavailable")
	}
	g.mu.Lock()
	g.cache["203.0.113.1"].Value.(*geoCacheEntry).used = time.Now().Add(-10 * time.Minute)
	g.mu.Unlock()
	if g.Lookup("203.0.113.1").ASN != 64501 {
		t.Fatal("TTL reload")
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("not mmdb")) }))
	defer bad.Close()
	if g.download(context.Background(), 1, bad.URL) == nil || g.Lookup("203.0.113.1").ASN != 64501 {
		t.Fatal("invalid update replaced working database")
	}
	if matches, _ := filepath.Glob(filepath.Join(c.Directory, ".ipdb-*.tmp")); len(matches) != 0 {
		t.Fatal(matches)
	}
}
func TestGeoConfiguration(t *testing.T) {
	for _, bad := range []struct{ k, v string }{{"CACHE_SIZE", "0"}, {"CACHE_SIZE", "10001"}, {"CACHE_TTL_SECONDS", "0"}, {"UPDATE_DAYS", "366"}, {"ENABLED", "perhaps"}, {"CITY_URL", "http://example.test/city"}, {"ASN_URL", "https://user:secret@example.test/asn"}} {
		if _, err := geoConfig("/tmp/db.sqlite", func(key string) string {
			if key == "WZXJDSSH_IPDB_"+bad.k {
				return bad.v
			}
			return ""
		}); err == nil {
			t.Fatal(bad.k)
		}
	}
}

func TestGeoCNMissingOptionalDescriptionStillChecksContent(t *testing.T) {
	c := geoFixtureConfig(t)
	g := NewGeoDB(c)
	defer g.Close()
	tree, err := mmdbwriter.New(mmdbwriter.Options{DatabaseType: "GeoCN", IPVersion: 6, IncludeReservedNetworks: true})
	if err != nil {
		t.Fatal(err)
	}
	_, subnet, _ := net.ParseCIDR("203.0.113.0/24")
	if err = tree.Insert(subnet, mmdbtype.Map{"isp": mmdbtype.String("Synthetic ISP")}); err != nil {
		t.Fatal(err)
	}
	var payload bytes.Buffer
	if _, err = tree.WriteTo(&payload); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(payload.Bytes()) }))
	defer server.Close()
	if err = g.download(context.Background(), 2, server.URL); err != nil {
		t.Fatal(err)
	}
	if result := g.Lookup("203.0.113.1"); result.ISP != "Synthetic ISP" {
		t.Fatal(result)
	}
	path := filepath.Join(c.Directory, "GeoCN.mmdb")
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, payload.Bytes()) {
		t.Fatal("download was rewritten", err)
	}
	reader, err := maxminddb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if len(reader.Metadata.Description) != 0 {
		t.Fatal("description was fabricated on disk")
	}
	if verifyGeoReader(1, reader) == nil {
		t.Fatal("wrong database type accepted")
	}
	// A GeoCN metadata exception must not skip damaged search-tree validation.
	damaged := bytes.Clone(actual)
	damaged[0] = 255
	damaged[1] = 255
	damaged[2] = 254
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(damaged) }))
	defer broken.Close()
	if g.download(context.Background(), 2, broken.URL) == nil {
		t.Fatal("corrupted tree accepted")
	}
	if result := g.Lookup("203.0.113.1"); result.ISP != "Synthetic ISP" {
		t.Fatal("failed update lost old reader", result)
	}
}
