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
	parts := strings.Split(phc, "$")
	// "" / "argon2id" / "v=19" / "m=..,t=..,p=.." / salt / hash
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false, errors.New("auth: malformed password hash")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, errors.New("auth: unsupported hash version")
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false, errors.New("auth: malformed hash parameters")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, errors.New("auth: malformed hash salt")
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, errors.New("auth: malformed hash value")
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
