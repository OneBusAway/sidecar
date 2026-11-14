package auth

import (
	"crypto/rand"
	"strings"
	"testing"
)

func TestHashVerifyRoundTrip(t *testing.T) {
	phc, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(phc, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("unexpected PHC prefix: %s", phc)
	}
	ok, err := VerifyPassword(phc, "correct horse battery")
	if err != nil || !ok {
		t.Fatalf("want match, got ok=%v err=%v", ok, err)
	}
	ok, err = VerifyPassword(phc, "wrong password xx")
	if err != nil || ok {
		t.Fatalf("want mismatch, got ok=%v err=%v", ok, err)
	}
}

func TestHashesAreSalted(t *testing.T) {
	a, _ := HashPassword("same password 12")
	b, _ := HashPassword("same password 12")
	if a == b {
		t.Fatal("two hashes of one password must differ (random salt)")
	}
}

// Foreign parameters: a hash written with different cost params must verify
// with THOSE params, so raising defaults later never breaks old rows.
func TestVerifyForeignParameters(t *testing.T) {
	// m=8 t=1 p=1 hash of "pw" is cheap to compute inline for the fixture.
	phc := hashWithParams("legacy password!", 8, 1, 1)
	ok, err := VerifyPassword(phc, "legacy password!")
	if err != nil || !ok {
		t.Fatalf("want match with stored params, got ok=%v err=%v", ok, err)
	}
}

func TestVerifyRejectsGarbage(t *testing.T) {
	for _, phc := range []string{"", "$argon2id$nope", "$argon2i$v=19$m=8,t=1,p=1$AAAA$AAAA", "plaintext"} {
		if _, err := VerifyPassword(phc, "x"); err == nil {
			t.Errorf("VerifyPassword(%q) should error", phc)
		}
	}
}

// The version field must be exactly "v=19". Sscanf-style parsing would read
// each of these as 19; everything else in the hash is valid, so an error here
// can only come from the version check.
func TestVerifyRejectsLooseVersion(t *testing.T) {
	for _, v := range []string{"v=19xxx", "v=+19", "v=019"} {
		phc := strings.Replace(DummyPHC, "$v=19$", "$"+v+"$", 1)
		if _, err := VerifyPassword(phc, "x"); err == nil {
			t.Errorf("VerifyPassword with %q should error", v)
		}
	}
}

// The parameters field must be exactly what encodePHC writes: no trailing
// garbage, signs or leading zeros in m, t or p. Everything else in the hash
// is valid, so an error here can only come from the parameters check.
func TestVerifyRejectsLooseParams(t *testing.T) {
	for _, params := range []string{
		"m=19456,t=2,p=1junk", "m=19456,t=2xx,p=1", "m=19456xx,t=2,p=1",
		"m=19456,t=2,p=+1", "m=19456,t=+2,p=1", "m=+19456,t=2,p=1",
		"m=19456,t=2,p=01", "m=19456,t=02,p=1", "m=019456,t=2,p=1",
	} {
		phc := strings.Replace(DummyPHC, "$m=19456,t=2,p=1$", "$"+params+"$", 1)
		if _, err := VerifyPassword(phc, "x"); err == nil {
			t.Errorf("VerifyPassword with %q should error", params)
		}
	}
}

func TestVerifyRejectsZeroParams(t *testing.T) {
	for _, params := range []string{"m=19456,t=0,p=1", "m=19456,t=2,p=0"} {
		phc := strings.Replace(DummyPHC, "$m=19456,t=2,p=1$", "$"+params+"$", 1)
		_, err := VerifyPassword(phc, "x")
		if err == nil || err.Error() != "auth: malformed hash parameters" {
			t.Errorf("VerifyPassword with %q: got %v, want malformed hash parameters", params, err)
		}
	}
}

// An empty hash segment decodes to zero bytes without error. Deriving a
// zero-length key panics inside x/crypto, so it must be rejected as malformed.
func TestVerifyRejectsEmptyHash(t *testing.T) {
	phc := "$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$"
	_, err := VerifyPassword(phc, "x")
	if err == nil || err.Error() != "auth: malformed hash value" {
		t.Errorf("VerifyPassword with empty hash segment: got %v, want malformed hash value", err)
	}
}

func TestDummyPHCIsWellFormed(t *testing.T) {
	ok, err := VerifyPassword(DummyPHC, "anything at all")
	if err != nil {
		t.Fatalf("DummyPHC must parse cleanly: %v", err)
	}
	if ok {
		t.Fatal("DummyPHC must match no password")
	}
}

func TestValidatePassword(t *testing.T) {
	if err := ValidatePassword("elevenchars"); err == nil {
		t.Error("11 chars must fail")
	}
	if err := ValidatePassword("twelve chars"); err != nil {
		t.Errorf("12 chars must pass: %v", err)
	}
	if err := ValidatePassword(strings.Repeat("a", 513)); err == nil {
		t.Error("513 bytes must fail")
	}
}

// hashWithParams hashes password with explicit argon2id cost parameters, for
// exercising VerifyPassword against hashes written with non-default (e.g.
// older, cheaper) parameters. Test-only: production code always calls
// HashPassword, which uses the current defaults, so this lives here rather
// than in password.go.
func hashWithParams(password string, m uint32, t uint32, p uint8) string {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		panic(err) // test-only helper; crypto/rand failure is unrecoverable anyway
	}
	return encodePHC(password, salt, m, t, p)
}

// TestVerifyRejectsOversizedParameters pins SC-26: VerifyPassword runs
// whatever cost the stored PHC string names, so an unbounded m/t/p (or an
// enormous salt or key) in a row is a memory/CPU bomb the next login
// detonates. Each case must be refused before any key derivation -- the m
// case would try to allocate 4 GiB if it were not.
func TestVerifyRejectsOversizedParameters(t *testing.T) {
	salt := "AAAAAAAAAAAAAAAAAAAAAA"                     // 16 bytes
	key := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" // 32 bytes
	long := strings.Repeat("A", 4096)
	cases := map[string]string{
		"memory 4 GiB":   "$argon2id$v=19$m=4194304,t=2,p=1$" + salt + "$" + key,
		"memory > cap":   "$argon2id$v=19$m=65537,t=2,p=1$" + salt + "$" + key,
		"time 1000":      "$argon2id$v=19$m=19456,t=1000,p=1$" + salt + "$" + key,
		"threads 255":    "$argon2id$v=19$m=19456,t=2,p=255$" + salt + "$" + key,
		"salt 3 KiB":     "$argon2id$v=19$m=19456,t=2,p=1$" + long + "$" + key,
		"key 3 KiB":      "$argon2id$v=19$m=19456,t=2,p=1$" + salt + "$" + long,
		"short salt":     "$argon2id$v=19$m=19456,t=2,p=1$AAAA$" + key,
		"short key":      "$argon2id$v=19$m=19456,t=2,p=1$" + salt + "$AAAA",
		"whole string >": "$argon2id$v=19$m=19456,t=2,p=1$" + salt + "$" + key + long,
	}
	for name, phc := range cases {
		t.Run(name, func(t *testing.T) {
			if ok, err := VerifyPassword(phc, "whatever password"); err == nil || ok {
				t.Fatalf("VerifyPassword = %v, %v; want an error", ok, err)
			}
		})
	}

	// The bounds must leave room above today's defaults, so raising them
	// later (the reason the parameters live in the row) still verifies.
	raised := hashWithParams("raised password!", 2*argonMemoryKiB, 3, 2)
	if ok, err := VerifyPassword(raised, "raised password!"); err != nil || !ok {
		t.Fatalf("raised-cost hash: ok=%v err=%v; want a match", ok, err)
	}
}
