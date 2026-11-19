package awsec2

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/pricing"

	"github.com/qoryai/hive/internal/adapterio"
	"github.com/qoryai/hive/internal/machine"
)

// cachePath is the path of the cache file of eu-west-1 of the handler h.
func cachePath(t *testing.T, h *Handler) string {
	t.Helper()
	dir, err := h.cacheDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "qory-machines-aws-ec2", "eu-west-1.json")
}

// reads returns the number of DescribeInstanceTypes, DescribeInstanceTypeOfferings and
// GetProducts calls of f.
func reads(f *fakeAPI) [3]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return [3]int{len(f.types), len(f.offerings), len(f.products)}
}

// makeOK runs make for a small spec and fails t unless it starts a machine.
func makeOK(t *testing.T, h *Handler, path string) {
	t.Helper()
	if status, out := serve(t, h, "make", path, makeRequest(2, 4096, 8, false, testData)); status != adapterio.ExitOK {
		t.Fatalf("make: exit status %d, output %q", status, out)
	}
}

// TestCacheHitAndExpiry pins the cache file: check writes it with the types, the
// zone's types and the prices, mode 0600 in a folder of mode 0700; a make within 24
// hours reads nothing of them from AWS; a make 24 hours later reads each again.
func TestCacheHitAndExpiry(t *testing.T) {
	f := world()
	h, log, _ := testHandler(f)
	path := writeSettings(t, validSettings)
	if status, out := serve(t, h, "check", path, `{"version": 1}`); status != adapterio.ExitOK {
		t.Fatalf("check: exit status %d, output %q", status, out)
	}
	file := cachePath(t, h)
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("cache file %v, error %v, want mode 0600", info, err)
	}
	if dir, err := os.Stat(filepath.Dir(file)); err != nil || dir.Mode().Perm() != 0o700 {
		t.Errorf("cache folder %v, error %v, want mode 0700", dir, err)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var doc cache
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Version != 1 || doc.Region != "eu-west-1" || !doc.TypesRead.Equal(testNow) || len(doc.Types) != len(catalog) ||
		len(doc.Zones[testZone].Types) != len(catalog) || !doc.PricesRead.Equal(testNow) || doc.Prices["t3a.medium"] != 0.0376 {
		t.Errorf("cache file %s", b)
	}

	h.now = func() time.Time { return testNow.Add(cacheRefresh - time.Second) }
	makeOK(t, h, path)
	if got := reads(f); got != [3]int{1, 1, 1} {
		t.Errorf("reads %v after a make within 24 hours, want [1 1 1]", got)
	}

	h.now = func() time.Time { return testNow.Add(cacheRefresh) }
	makeOK(t, h, path)
	if got := reads(f); got != [3]int{2, 2, 2} {
		t.Errorf("reads %v after a make 24 hours later, want [2 2 2]", got)
	}
	if log.Len() != 0 {
		t.Errorf("log lines %s", log)
	}
}

// TestCacheFailedRefresh pins that a failed read of a section that is 24 hours old
// uses the old section, with one log line per section, and that the file keeps every
// section and gets the time of the failed price lookup.
func TestCacheFailedRefresh(t *testing.T) {
	f := world()
	h, log, _ := testHandler(f)
	path := writeSettings(t, validSettings)
	makeOK(t, h, path)
	before, err := os.ReadFile(cachePath(t, h))
	if err != nil {
		t.Fatal(err)
	}

	f.itype = func(*ec2.DescribeInstanceTypesInput) (*ec2.DescribeInstanceTypesOutput, error) {
		return nil, apiError("RequestLimitExceeded")
	}
	f.offering = func(*ec2.DescribeInstanceTypeOfferingsInput) (*ec2.DescribeInstanceTypeOfferingsOutput, error) {
		return nil, apiError("RequestLimitExceeded")
	}
	f.product = func(*pricing.GetProductsInput) (*pricing.GetProductsOutput, error) {
		return nil, apiError("ThrottlingException")
	}
	h.now = func() time.Time { return testNow.Add(25 * time.Hour) }
	status, out := serve(t, h, "make", path, makeRequest(2, 4096, 8, false, testData))
	if status != adapterio.ExitOK || out != `{"id":"`+testID+`","machine_type":"t3a.medium"}`+"\n" {
		t.Fatalf("exit status %d, output %q", status, out)
	}
	for _, line := range []string{msgOldTypes, msgOldZone, msgOldPrices} {
		if strings.Count(log.String(), line) != 1 {
			t.Errorf("log\n%s\nwant one line: %s", log, line)
		}
	}
	if n := strings.Count(log.String(), "\n"); n != 3 {
		t.Errorf("%d log lines, want 3", n)
	}
	var old, doc cache
	if err := json.Unmarshal(before, &old); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(cachePath(t, h))
	if err != nil || json.Unmarshal(after, &doc) != nil {
		t.Fatalf("cache file %s, error %v", after, err)
	}
	if !doc.PricesTried.Equal(testNow.Add(25 * time.Hour)) {
		t.Errorf("prices_tried %v, want the time of the failed lookup", doc.PricesTried)
	}
	doc.PricesTried = time.Time{}
	if !reflect.DeepEqual(doc, old) {
		t.Errorf("the cache file changed beyond prices_tried:\n%s\nwas\n%s", after, before)
	}
}

// TestPriceRetry pins the wait after a failed price lookup: no lookup and no log line
// for 1 hour, then a lookup that, when it succeeds, clears the time of the failure.
func TestPriceRetry(t *testing.T) {
	f := world()
	product := f.product
	f.product = func(*pricing.GetProductsInput) (*pricing.GetProductsOutput, error) {
		return nil, apiError("ThrottlingException")
	}
	h, log, _ := testHandler(f)
	path := writeSettings(t, validSettings)
	makeOK(t, h, path)
	if n := strings.Count(log.String(), msgNoPrices); n != 1 || len(f.products) != 1 {
		t.Fatalf("%d GetProducts calls, log\n%s\nwant 1 call and one line: %s", len(f.products), log, msgNoPrices)
	}

	h.now = func() time.Time { return testNow.Add(priceRetry - time.Second) }
	makeOK(t, h, path)
	if len(f.products) != 1 || strings.Count(log.String(), "\n") != 1 {
		t.Errorf("%d GetProducts calls, log\n%s\nwant no lookup and no line within 1 hour", len(f.products), log)
	}

	f.product = product
	h.now = func() time.Time { return testNow.Add(priceRetry) }
	makeOK(t, h, path)
	b, err := os.ReadFile(cachePath(t, h))
	var doc cache
	if err != nil || json.Unmarshal(b, &doc) != nil {
		t.Fatalf("cache file %s, error %v", b, err)
	}
	if len(f.products) != 2 || !doc.PricesTried.IsZero() || !doc.PricesRead.Equal(testNow.Add(priceRetry)) || len(doc.Prices) == 0 {
		t.Errorf("%d GetProducts calls, cache file %s, want a second lookup and its prices", len(f.products), b)
	}
}

// TestPriceTimeout pins the time limit of a price lookup: each GetProducts call's
// context ends at most 20 seconds after the call.
func TestPriceTimeout(t *testing.T) {
	f := world()
	h, _, _ := testHandler(f)
	makeOK(t, h, writeSettings(t, validSettings))
	if len(f.productLimits) != 1 || f.productLimits[0] <= 0 || f.productLimits[0] > priceTimeout || priceTimeout != 20*time.Second {
		t.Errorf("time left %v, want a deadline within 20 s", f.productLimits)
	}
}

// missCache is a cache file of eu-west-1 whose types, read at typesRead, lack
// t3a.medium and x9.large, which the zone's types, read at testNow, contain.
// DescribeInstanceTypes of world returns t3a.medium and never x9.large.
func missCache(typesRead time.Time) cache {
	return cache{
		Version:    1,
		Region:     "eu-west-1",
		TypesRead:  typesRead,
		Types:      []machine.InstanceType{{Name: "t3.medium", VCPUs: 2, MemoryMiB: 4096, Archs: []string{machine.ArchAMD64}, OnDemand: true}},
		Zones:      map[string]zoneTypes{testZone: {Read: testNow, Types: []string{"t3.medium", "t3a.medium", "x9.large"}}},
		PricesRead: testNow,
		Prices:     map[string]float64{"t3.medium": 0.0416, "t3a.medium": 0.0376},
	}
}

// TestCacheMiss pins that a zone that offers a type that the cached types lack makes
// catalog read the region's types again, once, when the types were read before the
// zone's types, and use the new type. A type that DescribeInstanceTypes does not
// return causes no second read.
func TestCacheMiss(t *testing.T) {
	f := world()
	h, _, _ := testHandler(f)
	if err := writeCache(cachePath(t, h), missCache(testNow.Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	path := writeSettings(t, validSettings)
	for range 2 {
		status, out := serve(t, h, "make", path, makeRequest(2, 4096, 8, false, testData))
		if status != adapterio.ExitOK || out != `{"id":"`+testID+`","machine_type":"t3a.medium"}`+"\n" {
			t.Fatalf("exit status %d, output %q", status, out)
		}
	}
	if got := reads(f); got != [3]int{1, 0, 0} {
		t.Errorf("reads %v after two makes, want [1 0 0]: one read of the types, none of the zone or the prices", got)
	}
}

// TestCacheMissSameRead pins that a missing type causes no read of the region's types
// when they were read at the same time as the zone's types or later.
func TestCacheMissSameRead(t *testing.T) {
	f := world()
	h, _, _ := testHandler(f)
	if err := writeCache(cachePath(t, h), missCache(testNow)); err != nil {
		t.Fatal(err)
	}
	status, out := serve(t, h, "make", writeSettings(t, validSettings), makeRequest(2, 4096, 8, false, testData))
	if status != adapterio.ExitOK || out != `{"id":"`+testID+`","machine_type":"t3.medium"}`+"\n" {
		t.Fatalf("exit status %d, output %q", status, out)
	}
	if got := reads(f); got != [3]int{0, 0, 0} {
		t.Errorf("reads %v, want none", got)
	}
}

// TestCacheNoFile pins the cases with no usable cache file, each with one log line and
// a make that reads everything from AWS: a file that is not valid, a file of another
// version, no cache folder, and a cache folder that cannot be written.
func TestCacheNoFile(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, h *Handler)
		line  string
	}{
		{"not JSON", func(t *testing.T, h *Handler) {
			writeFile(t, cachePath(t, h), "{not json")
		}, msgBadCache},
		{"another version", func(t *testing.T, h *Handler) {
			writeFile(t, cachePath(t, h), `{"version": 2, "region": "eu-west-1"}`)
		}, msgBadCache},
		{"another region", func(t *testing.T, h *Handler) {
			writeFile(t, cachePath(t, h), `{"version": 1, "region": "eu-central-1"}`)
		}, msgBadCache},
		{"larger than 8 MiB", func(t *testing.T, h *Handler) {
			writeFile(t, cachePath(t, h), `{"version": 1, "region": "eu-west-1"}`+strings.Repeat(" ", maxCacheFile))
		}, msgBadCache},
		{"no cache folder", func(_ *testing.T, h *Handler) {
			h.cacheDir = func() (string, error) { return "", errors.New("neither $XDG_CACHE_HOME nor $HOME are defined") }
		}, msgNoCacheDir},
		{"a cache folder that cannot be written", func(t *testing.T, h *Handler) {
			if os.Geteuid() == 0 {
				t.Skip("root writes to a folder of mode 0500")
			}
			dir := filepath.Dir(filepath.Dir(cachePath(t, h)))
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(dir, 0o700) })
		}, msgCacheWrite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := world()
			h, log, _ := testHandler(f)
			tc.setup(t, h)
			makeOK(t, h, writeSettings(t, validSettings))
			if got := reads(f); got != [3]int{1, 1, 1} {
				t.Errorf("reads %v, want [1 1 1]", got)
			}
			if strings.Count(log.String(), "\n") != 1 || !strings.Contains(log.String(), tc.line) {
				t.Errorf("log\n%s\nwant one line: %s", log, tc.line)
			}
		})
	}
}

// TestCacheRewrite pins that a file that is not valid is written again, valid, after
// the reads from AWS.
func TestCacheRewrite(t *testing.T) {
	f := world()
	h, _, _ := testHandler(f)
	writeFile(t, cachePath(t, h), "{not json")
	makeOK(t, h, writeSettings(t, validSettings))
	b, err := os.ReadFile(cachePath(t, h))
	var doc cache
	if err != nil || json.Unmarshal(b, &doc) != nil || doc.Version != 1 {
		t.Errorf("cache file %s, error %v", b, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(cachePath(t, h)))
	if len(entries) != 1 {
		t.Errorf("%d files in the cache folder, want 1", len(entries))
	}
}

// TestStale pins the age at which a section is read again.
func TestStale(t *testing.T) {
	for _, tc := range []struct {
		read  time.Time
		stale bool
	}{
		{time.Time{}, true},
		{testNow, false},
		{testNow.Add(-cacheRefresh + time.Second), false},
		{testNow.Add(-cacheRefresh), true},
		{testNow.Add(time.Minute), true},
	} {
		if got := stale(tc.read, testNow); got != tc.stale {
			t.Errorf("read %v: stale %v, want %v", tc.read, got, tc.stale)
		}
	}
}

// TestOrder pins the order of make: vCPUs, memory, price, a type without a price
// after the priced ones, then name.
func TestOrder(t *testing.T) {
	ts := []machine.InstanceType{
		{Name: "d", VCPUs: 4, MemoryMiB: 8192},
		{Name: "c", VCPUs: 2, MemoryMiB: 8192},
		{Name: "b2", VCPUs: 2, MemoryMiB: 4096},
		{Name: "b1", VCPUs: 2, MemoryMiB: 4096},
		{Name: "a", VCPUs: 2, MemoryMiB: 4096},
		{Name: "z", VCPUs: 2, MemoryMiB: 4096},
	}
	order(ts, map[string]float64{"z": 0.02, "a": 0.03, "c": 0.01, "d": 0.001})
	var got []string
	for _, x := range ts {
		got = append(got, x.Name)
	}
	if want := []string{"z", "a", "b1", "b2", "c", "d"}; !reflect.DeepEqual(got, want) {
		t.Errorf("order %v, want %v", got, want)
	}
}

// writeFile writes text to path, with the folders it needs.
func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}
