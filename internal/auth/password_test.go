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
