package regions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

// ErrResponseTooLarge is the sentinel wrapped into the error Fetch returns
// when the response body exceeds ClientOptions.MaxBytes, distinguishing that
// failure from an ordinary malformed-JSON or transport error.
var ErrResponseTooLarge = errors.New("regions: response too large")

// ClientOptions bounds a directory fetch. Every field here is a security
// control, not a tuning knob: this is an unauthenticated ingest path from
// infrastructure the operator does not control, fetched at boot and hourly,
// whose contents populate the serving path.
type ClientOptions struct {
	// Timeout bounds the whole request. Go's default http.Client has no
	// timeout at all, so a tarpitting host would otherwise hang the fetch
	// forever.
	Timeout time.Duration

	// MaxBytes caps the response body. The real document is ~100 KB; an
	// unbounded read hands a hostile or compromised host a free memory
	// amplifier.
	MaxBytes int64

	// MaxEntries caps the number of directory entries accepted in one
	// document.
	MaxEntries int

	// MaxFieldLen caps the length of every stored string field.
	MaxFieldLen int

	// HTTPClient, if set, is used instead of a client constructed from
	// Timeout. Tests supply this to point at an httptest server; production
	// callers normally leave it nil.
	HTTPClient *http.Client
}

// DefaultClientOptions are sized for the real directory (~100 KB, ~50
// entries).
//
// These are not tuning knobs, they are the defence. This is an
// unauthenticated ingest path from infrastructure the operator does not
// control, running at boot and hourly, whose contents populate the serving
// path.
func DefaultClientOptions() ClientOptions {
	return ClientOptions{
		Timeout:     30 * time.Second, // Go's default client has NO timeout
		MaxBytes:    5 << 20,          // an unbounded read is a memory amplifier
		MaxEntries:  10_000,
		MaxFieldLen: 512,
	}
}

// Client fetches and validates the regions directory document.
type Client struct {
	url  string
	opts ClientOptions
	http *http.Client
}

// NewClient builds a Client for the directory document at url. If
// opts.HTTPClient is nil, a client is constructed using opts.Timeout.
func NewClient(url string, opts ClientOptions) *Client {
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: opts.Timeout}
	}
	return &Client{url: url, opts: opts, http: httpClient}
}

// directoryResponse is the top-level shape of regions-v3.json:
//
//	{"version":3,"code":200,"text":"OK","data":{"list":[ ... ]}}
//
// The real document carries many more fields (bounds, analytics ids, contact
// info, ...) that this type does not name; encoding/json ignores unknown
// fields by default, so they are dropped harmlessly.
type directoryResponse struct {
	Data struct {
		List []directoryEntry `json:"list"`
	} `json:"data"`
}

// directoryEntry is one region as the directory document represents it. The
// document carries no timezone and no default agency id -- those are locally
// managed, which is why they are absent here too.
type directoryEntry struct {
	ID             int64            `json:"id"`
	RegionName     string           `json:"regionName"`
	OBABaseURL     string           `json:"obaBaseUrl"`
	SidecarBaseURL string           `json:"sidecarBaseUrl"`
	Language       string           `json:"language"`
	Active         bool             `json:"active"`
	Bounds         []directoryBound `json:"bounds"`
}

// directoryBound is one rectangle of a region's coverage. lat/lon is the
// rectangle's center, not a corner. Spans are pointers-free because a missing
// span legitimately means zero area, unlike a missing center.
type directoryBound struct {
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
	LatSpan float64 `json:"latSpan"`
	LonSpan float64 `json:"lonSpan"`
}

// Fetch downloads and parses the directory document, returning the entries
// that pass validation. An individual malformed entry is skipped rather than
// failing the whole fetch, so one bad row from upstream cannot block a
// refresh that would otherwise succeed. Fetch never mutates any store; that
// is Sync's job.
func (c *Client) Fetch(ctx context.Context) ([]Region, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, fmt.Errorf("regions: build request for %s: %w", c.url, err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("regions: fetch %s: %w", c.url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("regions: fetch %s: unexpected status %d", c.url, resp.StatusCode)
	}

	// Read at most MaxBytes+1: if the extra byte is present, the body
	// exceeded the cap and we reject it outright rather than parsing a
	// truncated document.
	limited := io.LimitReader(resp.Body, c.opts.MaxBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("regions: read response from %s: %w", c.url, err)
	}
	if int64(len(body)) > c.opts.MaxBytes {
		return nil, fmt.Errorf("regions: response from %s exceeds %d bytes: %w", c.url, c.opts.MaxBytes, ErrResponseTooLarge)
	}

	var doc directoryResponse
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("regions: decode response from %s: %w", c.url, err)
	}

	if len(doc.Data.List) > c.opts.MaxEntries {
		return nil, fmt.Errorf("regions: response from %s has %d entries, exceeds max %d", c.url, len(doc.Data.List), c.opts.MaxEntries)
	}

	seen := make(map[int64]bool, len(doc.Data.List))
	out := make([]Region, 0, len(doc.Data.List))
	for _, e := range doc.Data.List {
		reg, ok := c.validate(e, seen)
		if !ok {
			continue
		}
		out = append(out, reg)
	}
	return out, nil
}

// validate applies the per-entry rules and returns the sanitized Region. An
// entry that fails any rule -- including being a repeat of an id already
// seen earlier in this document -- is reported invalid (ok=false) rather
// than erroring the whole fetch. On success, e.ID is recorded in seen so a
// later duplicate in the same document is rejected.
func (c *Client) validate(e directoryEntry, seen map[int64]bool) (Region, bool) {
	if e.ID < 0 {
		return Region{}, false
	}
	if seen[e.ID] {
		return Region{}, false
	}

	// Strip ASCII control characters from the name: it is later printed to
	// an admin's terminal by `sidecar-admin region list`, where escape
	// sequences are an injection vector.
	name := StripControlChars(e.RegionName)
	if name == "" || e.OBABaseURL == "" {
		return Region{}, false
	}

	for _, s := range []string{name, e.OBABaseURL, e.SidecarBaseURL, e.Language} {
		if len(s) > c.opts.MaxFieldLen {
			return Region{}, false
		}
	}

	seen[e.ID] = true
	return Region{
		ID:             e.ID,
		Name:           name,
		OBABaseURL:     e.OBABaseURL,
		SidecarBaseURL: e.SidecarBaseURL,
		Language:       e.Language,
		Active:         e.Active,
		Centroid:       computeCentroid(e.Bounds),
	}, true
}

// StripControlChars removes ASCII control characters (0x00-0x1F and the
// 0x7F DEL) from s, leaving everything else -- including non-ASCII text --
// untouched. It guards every string that arrives from outside and later
// reaches an operator's terminal: directory names, and the name on a region
// API key, which a compromised service principal controls and which
// `sidecar-admin key list` prints to the terminal of the operator
// investigating that compromise.
func StripControlChars(s string) string {
	if !strings.ContainsFunc(s, isASCIIControl) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if isASCIIControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isASCIIControl(r rune) bool {
	return r < 0x20 || r == 0x7f
}

// computeCentroid reduces a region's bounds rectangles to a single point: the
// area-weighted mean of the rectangle centers.
//
// Area weighting is what makes the result invariant to how the bounds were
// split -- cutting one rectangle into four quadrants leaves the centroid
// exactly where it was. The two obvious alternatives both fail on real data:
// the union bounding box's center is dragged into the mountains by one small
// outlying rectangle, and the unweighted mean moves whenever an agency
// re-describes the same coverage with more rectangles.
//
// It returns nil rather than a zero value when there is nothing usable, since
// 0,0 is a real coordinate.
func computeCentroid(bounds []directoryBound) *LatLon {
	if len(bounds) == 0 {
		return nil
	}

	var sumLat, sumLon, sumWeight float64
	for _, b := range bounds {
		// A negative span is nonsense from upstream; clamp to zero weight
		// rather than letting it subtract area from its neighbours.
		w := math.Max(b.LatSpan, 0) * math.Max(b.LonSpan, 0)
		sumLat += b.Lat * w
		sumLon += b.Lon * w
		sumWeight += w
	}

	var lat, lon float64
	if sumWeight > 0 {
		lat, lon = sumLat/sumWeight, sumLon/sumWeight
	} else {
		// Every rectangle has zero area -- a region described as points.
		// The unweighted mean is the only meaningful answer available.
		for _, b := range bounds {
			lat += b.Lat
			lon += b.Lon
		}
		lat /= float64(len(bounds))
		lon /= float64(len(bounds))
	}

	if math.IsNaN(lat) || math.IsNaN(lon) ||
		lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return nil
	}
	return &LatLon{Lat: lat, Lon: lon}
}
