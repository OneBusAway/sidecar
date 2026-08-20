// Package securetoken mints the unguessable, URL-safe tokens that publicly
// address alarms and (later) Live Activities: 22 characters of raw URL-safe
// base64 encoding 128 random bits (spec §2.4). Possession of the token is
// the ownership proof for the resource, so the only requirements are
// crypto-strength randomness and URL safety.
package securetoken

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// New returns a fresh 22-character token.
func New() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("securetoken: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
