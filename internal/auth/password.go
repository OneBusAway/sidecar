package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters per current OWASP guidance (spec section 4.1). Raising
// them later is safe: VerifyPassword always uses the parameters stored in
// the row's own PHC string.
const (
	argonMemoryKiB = 19456
	argonTime      = 2
	argonThreads   = 1
	saltLen        = 16
	keyLen         = 32
)

// Verification ceilings (SC-26). VerifyPassword honours the parameters a
// stored hash names, so these bound what a tampered or foreign row can make
// one login cost. They sit well above the defaults (about 3x memory, 5x
// passes) so the defaults can still be raised without a code change; the
// salt and key minimums are RFC 9106's.
const (
	maxArgonMemoryKiB = 64 * 1024
	maxArgonTime      = 10
	maxArgonThreads   = 8
	minSaltLen        = 8
	maxSaltLen        = 64
	minKeyLen         = 4
	maxKeyLen         = 64
	// maxPHCLen bounds the whole string before it is split or decoded.
	maxPHCLen = 256
)

// Password length bounds (spec section 4.1). Max exists because argon2 input
// length is attacker-controlled CPU cost.
const (
	MinPasswordLen   = 12
	MaxPasswordBytes = 512
)

// DummyPHC is a syntactically valid argon2id hash that matches no password.
// The login handler verifies against it when the username does not exist, so
// response timing does not reveal which usernames are real. The hash bytes
// are all zero, which no real key derivation produces.
const DummyPHC = "$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// ValidatePassword enforces the length bounds shared by every surface (CLI
// today, any future one).
func ValidatePassword(password string) error {
	if len(password) < MinPasswordLen {
		return fmt.Errorf("password must be at least %d characters", MinPasswordLen)
	}
	if len(password) > MaxPasswordBytes {
		return fmt.Errorf("password must be at most %d bytes", MaxPasswordBytes)
	}
	return nil
}

// HashPassword derives an argon2id hash and encodes it in PHC string format.
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generate salt: %w", err)
	}
	return encodePHC(password, salt, argonMemoryKiB, argonTime, argonThreads), nil
}

func encodePHC(password string, salt []byte, m uint32, t uint32, p uint8) string {
	key := argon2.IDKey([]byte(password), salt, t, m, p, keyLen)
	b64 := base64.RawStdEncoding.EncodeToString
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, m, t, p, b64(salt), b64(key))
}

// VerifyPassword reports whether password matches the PHC-encoded hash,
// using the parameters stored in the hash itself. A malformed hash is an
// error; a clean mismatch is (false, nil).
func VerifyPassword(phc, password string) (bool, error) {
	if len(phc) > maxPHCLen {
		return false, errors.New("auth: malformed password hash")
	}
	parts := strings.Split(phc, "$")
	// "" / "argon2id" / "v=19" / "m=..,t=..,p=.." / salt / hash
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false, errors.New("auth: malformed password hash")
	}
	if parts[2] != fmt.Sprintf("v=%d", argon2.Version) {
		return false, errors.New("auth: unsupported hash version")
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil ||
		parts[3] != fmt.Sprintf("m=%d,t=%d,p=%d", m, t, p) || t < 1 || p < 1 {
		return false, errors.New("auth: malformed hash parameters")
	}
	// The row names its own cost, and verification runs it unconditionally,
	// so a hash naming 4 GiB or a thousand passes is a memory/CPU bomb the
	// next login attempt detonates (SC-26). Refuse anything past the ceiling
	// before deriving a single byte.
	if m > maxArgonMemoryKiB || t > maxArgonTime || p > maxArgonThreads {
		return false, errors.New("auth: hash parameters exceed the verification ceiling")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < minSaltLen || len(salt) > maxSaltLen {
		return false, errors.New("auth: malformed hash salt")
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) < minKeyLen || len(want) > maxKeyLen {
		// An empty segment decodes to zero bytes; argon2 cannot derive a
		// zero-length key and would panic.
		return false, errors.New("auth: malformed hash value")
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
