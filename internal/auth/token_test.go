package auth

import "testing"

func TestNewTokenDistinctAndHashed(t *testing.T) {
	tok1, hash1, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	tok2, hash2, _ := NewToken()
	if tok1 == tok2 || hash1 == hash2 {
		t.Fatal("tokens must be unique")
	}
	if tok1 == hash1 {
		t.Fatal("hash must not equal the raw token")
	}
	if HashToken(tok1) != hash1 {
		t.Fatal("HashToken must reproduce the hash NewToken returned")
	}
	if len(hash1) != 64 {
		t.Fatalf("want 64 hex chars, got %d", len(hash1))
	}
}
