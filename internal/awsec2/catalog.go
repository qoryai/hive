package awsec2

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/qoryai/hive/internal/machine"
)

// cacheRefresh is the age at which check and make read a section of the cache file
// again from AWS.
const cacheRefresh = 24 * time.Hour

// priceTimeout is the time limit of one price lookup, every page of it, so that a
// price lookup that gets no response leaves check and make time for EC2.
const priceTimeout = 20 * time.Second

// priceRetry is the wait after a failed price lookup before the next one, so that a
// price list endpoint that fails costs one lookup per hour, not one per command.
const priceRetry = time.Hour

// cacheVersion is the version of the cache file's layout. A file of another version
// is read as no cache.
const cacheVersion = 1

// cacheFolder is the adapter's folder in the folder of os.UserCacheDir.
const cacheFolder = "qory-machines-aws-ec2"

// maxCacheFile is the most bytes of a cache file that the adapter reads. A larger
// file is read as no cache.
const maxCacheFile = 8 << 20

// regionShape matches a region that the cache file's name may contain.
var regionShape = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

// cache is the cache file of one region: the region's instance types, the instance
// types of each zone, and the prices of the region's instance types, each with the
// time of its read from AWS.
type cache struct {
	Version int    `json:"version"`
	Region  string `json:"region"`
	// TypesRead is the time of the DescribeInstanceTypes read of Types.
	TypesRead time.Time              `json:"types_read"`
	Types     []machine.InstanceType `json:"types"`
	// Zones maps a zone, such as eu-west-1a, to the instance types it offers.
	Zones map[string]zoneTypes `json:"zones"`
	// PricesRead is the time of the GetProducts read of Prices.
	PricesRead time.Time `json:"prices_read"`
	// PricesTried is the time of the last failed price lookup, zero after a lookup
	// that succeeded.
	PricesTried time.Time `json:"prices_tried"`
	// Prices maps an instance type to its on-demand price in USD per hour.
	Prices map[string]float64 `json:"prices"`
}

// zoneTypes is the instance types of one zone, with the time of their
// DescribeInstanceTypeOfferings read.
type zoneTypes struct {
	Read  time.Time `json:"read"`
	Types []string  `json:"types"`
}

// The log lines of the cache. Each is one line, with the cache file's path or the
// error as attributes.
const (
	msgNoCacheDir  = "Set HOME or XDG_CACHE_HOME to a writable folder: the adapter has no folder for its cache file, so each check and make reads the instance types and prices from AWS"
	msgBadCache    = "No action needed: the adapter cannot read its cache file, so it reads the instance types and prices from AWS and writes the file again"
	msgCacheWrite  = "Give the adapter a writable home folder, or set XDG_CACHE_HOME to one: writing the cache file failed, so the next check or make reads the instance types and prices from AWS again"
	msgOldTypes    = "If this repeats, check the instance role and the network path to EC2: reading the instance types failed, so the adapter uses the cached list"
	msgOldZone     = "If this repeats, check the instance role and the network path to EC2: reading the zone's instance types failed, so the adapter uses the cached list"
	msgMissedTypes = "If this repeats, check the instance role and the network path to EC2: the zone offers instance types that the cached list lacks, and reading the list again failed"
	msgOldPrices   = "Give the instance role pricing:GetProducts, and allow HTTPS to api.pricing.us-east-1.amazonaws.com: the price lookup failed, so the adapter uses the cached prices and tries again in 1 hour"
	msgNoPrices    = "Give the instance role pricing:GetProducts, and allow HTTPS to api.pricing.us-east-1.amazonaws.com: the price lookup failed, so instance types of one size are tried by name until the next lookup in 1 hour"
)

// catalog returns the instance types that zone offers, as DescribeInstanceTypes
// returns them, and the on-demand prices of the region's instance types in USD per
// hour. It reads them through the cache file of the region, at
// <os.UserCacheDir>/qory-machines-aws-ec2/<region>.json, which has three sections:
// the region's instance types, the instance types of each zone, and the prices.
//
//   - A section that is missing or 24 hours old, [cacheRefresh], is read again from
//     AWS. When that read fails, catalog uses the old section and writes one log line.
//     With no old section, a failed read of the instance types or of the zone's types
//     fails catalog, and a failed price lookup leaves the prices empty, with one log
//     line. The price lookup has a time limit of [priceTimeout]. A failed price
//     lookup writes its time to the file, and catalog tries no price lookup until
//     [priceRetry] after it.
//   - When the zone offers an instance type that the cached types lack, and the
//     region's types were read before the zone's types, catalog reads the region's
//     instance types again, once. A type that DescribeInstanceTypes does not return
//     causes no second read until the zone's types are read again.
//   - After a read from AWS, catalog writes the file again: to a new file in the same
//     folder, renamed over the old one, mode 0600 in a folder of mode 0700. When the
//     folder or the file cannot be written, catalog writes one log line and goes on.
//   - When os.UserCacheDir returns an error, catalog reads everything from AWS, with
//     one log line. A file that is not valid, of another version or region, or larger
//     than 8 MiB counts as no file, with one log line.
//
// A type that the zone offers and DescribeInstanceTypes does not return is left out.
func (h *Handler) catalog(ctx context.Context, c *machine.Client, zone string) ([]machine.InstanceType, map[string]float64, error) {
	path, doc := h.loadCache(c.RegionCode())
	now := h.now().UTC()
	changed, typesRead := false, false
	if stale(doc.TypesRead, now) || len(doc.Types) == 0 {
		ts, err := c.AllInstanceTypes(ctx)
		switch {
		case err == nil:
			doc.Types, doc.TypesRead, changed, typesRead = ts, now, true, true
		case len(doc.Types) == 0:
			return nil, nil, err
		default:
			h.log.Warn(msgOldTypes, "read", doc.TypesRead, "error", err)
		}
	}
	z, ok := doc.Zones[zone]
	if !ok || stale(z.Read, now) {
		names, err := c.ZoneTypes(ctx, zone)
		switch {
		case err == nil:
			z = zoneTypes{Read: now, Types: names}
			doc.Zones[zone], changed = z, true
		case !ok:
			return nil, nil, err
		default:
			h.log.Warn(msgOldZone, "zone", zone, "read", z.Read, "error", err)
		}
	}
	if !typesRead && doc.TypesRead.Before(z.Read) && missing(doc.Types, z.Types) {
		ts, err := c.AllInstanceTypes(ctx)
		if err == nil {
			doc.Types, doc.TypesRead, changed = ts, now, true
		} else {
			h.log.Warn(msgMissedTypes, "error", err)
		}
	}
	if stale(doc.PricesRead, now) && !tried(doc.PricesTried, now) {
		pctx, cancel := context.WithTimeout(ctx, priceTimeout)
		prices, err := c.Prices(pctx)
		cancel()
		changed = true
		switch {
		case err == nil:
			doc.Prices, doc.PricesRead, doc.PricesTried = prices, now, time.Time{}
		case len(doc.Prices) > 0:
			doc.PricesTried = now
			h.log.Warn(msgOldPrices, "read", doc.PricesRead, "error", err)
		default:
			doc.PricesTried = now
			h.log.Warn(msgNoPrices, "error", err)
		}
	}
	if changed {
		h.saveCache(path, doc)
	}
	byName := make(map[string]machine.InstanceType, len(doc.Types))
	for _, t := range doc.Types {
		byName[t.Name] = t
	}
	var out []machine.InstanceType
	for _, name := range z.Types {
		if t, ok := byName[name]; ok {
			out = append(out, t)
		}
	}
	return out, doc.Prices, nil
}

// stale reports whether a section read at read is to be read again at now: it was
// never read, it is [cacheRefresh] old, or its time is after now.
func stale(read, now time.Time) bool {
	return read.IsZero() || now.Sub(read) >= cacheRefresh || read.After(now)
}

// tried reports whether a price lookup that failed at failed is less than
// [priceRetry] before now. A time after now counts as not tried.
func tried(failed, now time.Time) bool {
	return !failed.IsZero() && !failed.After(now) && now.Sub(failed) < priceRetry
}

// missing reports whether names contains a type that ts lacks.
func missing(ts []machine.InstanceType, names []string) bool {
	have := make(map[string]bool, len(ts))
	for _, t := range ts {
		have[t.Name] = true
	}
	for _, n := range names {
		if !have[n] {
			return true
		}
	}
	return false
}

// loadCache returns the path of the cache file of region and its content, or "" and
// an empty cache when the adapter has no cache folder, and an empty cache when the
// file is missing or not valid.
func (h *Handler) loadCache(region string) (string, cache) {
	empty := cache{Version: cacheVersion, Region: region, Zones: map[string]zoneTypes{}}
	if !regionShape.MatchString(region) {
		return "", empty
	}
	dir, err := h.cacheDir()
	if err != nil {
		h.log.Warn(msgNoCacheDir, "error", err)
		return "", empty
	}
	path := filepath.Join(dir, cacheFolder, region+".json")
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return path, empty
	}
	var doc cache
	if err == nil {
		var b []byte
		b, err = io.ReadAll(io.LimitReader(f, maxCacheFile+1))
		f.Close()
		switch {
		case err != nil:
		case len(b) > maxCacheFile:
			err = errors.New("the file is larger than 8 MiB")
		default:
			err = json.Unmarshal(b, &doc)
		}
	}
	if err == nil && (doc.Version != cacheVersion || doc.Region != region) {
		err = errors.New("the file is of another version or region")
	}
	if err != nil {
		h.log.Warn(msgBadCache, "path", path, "error", err)
		return path, empty
	}
	if doc.Zones == nil {
		doc.Zones = map[string]zoneTypes{}
	}
	return path, doc
}

// saveCache writes doc to the cache file at path, through a new file in the same
// folder that it renames over the old one. It writes one log line when that fails,
// and nothing when path is "".
func (h *Handler) saveCache(path string, doc cache) {
	if path == "" {
		return
	}
	if err := writeCache(path, doc); err != nil {
		h.log.Warn(msgCacheWrite, "path", path, "error", err)
	}
}

// writeCache writes doc as JSON to path, mode 0600, in a folder of mode 0700 that it
// makes when it is missing.
func writeCache(path string, doc cache) error {
	b, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".write-*.json")
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}
