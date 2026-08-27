// Package apikey is the domain for the two credentials a server-side
// consumer can hold against the admin API: a region key, scoped to exactly
// one region, and a service principal, whose only power is to mint, list,
// and revoke region keys.
//
// Nothing here performs I/O beyond crypto/rand and nothing reads the clock;
// storage lives behind Repository and every instant is injected.
package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Kind distinguishes the two credential families a raw key can name.
type Kind string

const (
	// KindRegion is a per-region key: obask_<regionID>_<secret>.
	KindRegion Kind = "region"
	// KindPrincipal is a deployment-wide provisioning credential: obasp_<secret>.
	KindPrincipal Kind = "principal"
)

const (
	// RegionPrefix is the first segment of a region key's plaintext.
	RegionPrefix = "obask"
	// PrincipalPrefix is the first segment of a service principal's plaintext.
	PrincipalPrefix = "obasp"
	// MaxRawLen bounds the credential itself -- the part of the header after
	// "Bearer " -- not the whole header value. A real key is 51 bytes at
	// most; the cap exists so an unauthenticated caller cannot make the
	// server SHA-256 a megabyte for free (spec section 4.2).
	MaxRawLen = 128
	// secretBytes is the entropy behind every key. 256 bits is what makes
	// salting and constant-time comparison unnecessary (spec section 2.6).
	secretBytes = 32
)

// regionSegment is the exact grammar of the region id in a region key's
// plaintext: no leading zeros, no sign, no whitespace (spec section 3.1).
// The same grammar governs the {regionId} path segment.
var regionSegment = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

// newSecret returns 43 characters of raw URL-safe base64 over 32 random bytes.
func newSecret() (string, error) {
	var b [secretBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("apikey: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// NewRegionKey mints a region key and its stored hash. The raw key is
// returned to the caller once and must never be logged or persisted.
func NewRegionKey(regionID int64) (raw, hash string, err error) {
	secret, err := newSecret()
	if err != nil {
		return "", "", err
	}
	raw = RegionPrefix + "_" + strconv.FormatInt(regionID, 10) + "_" + secret
	return raw, Hash(raw), nil
}

// NewPrincipalKey mints a service principal key and its stored hash. Same
// handling rules as NewRegionKey.
func NewPrincipalKey() (raw, hash string, err error) {
	secret, err := newSecret()
	if err != nil {
		return "", "", err
	}
	raw = PrincipalPrefix + "_" + secret
	return raw, Hash(raw), nil
}

// Hash is the stored form of a key: hex SHA-256, unsalted. The input is 256
// bits of crypto/rand output, so there is nothing to salt against and the
// lookup can be a plain index hit (spec section 2.6). This is deliberately a
// second, independent hash function from auth.HashToken rather than a call
// into that package: region keys and session tokens have separate
// lifecycles, and apikey must not depend on auth.
func Hash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// ParsePrefix classifies a raw key by its plaintext prefix, so the caller
// knows which table to look the hash up in without a second query.
//
// The parsing is pinned by the spec (section 3.1) and must not be
// "simplified": the base64url alphabet contains '_', so about half of all
// real secrets contain one. Only a Cut on the FIRST '_' -- twice for a
// region key -- is correct. A strings.Split or a cut on the last '_' passes
// every hand-written fixture and mangles half of production.
//
// The region id in the plaintext is a debugging aid only. The hash lookup
// decides; the caller must still reject a stored row whose region_id
// disagrees with what this returned.
func ParsePrefix(raw string) (kind Kind, regionID int64, ok bool) {
	prefix, rest, found := strings.Cut(raw, "_")
	if !found || rest == "" {
		return "", 0, false
	}
	switch prefix {
	case PrincipalPrefix:
		return KindPrincipal, 0, true
	case RegionPrefix:
		idPart, secret, split := strings.Cut(rest, "_")
		if !split || secret == "" || !regionSegment.MatchString(idPart) {
			return "", 0, false
		}
		id, err := strconv.ParseInt(idPart, 10, 64)
		if err != nil {
			// Only reachable for a digit string too long for int64; the
			// regex has already excluded every other shape.
			return "", 0, false
		}
		return KindRegion, id, true
	default:
		return "", 0, false
	}
}
