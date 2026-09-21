package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestHashAndVerify(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword() = %v", err)
	}
	if err := VerifyPassword(hash, "correct horse battery staple"); err != nil {
		t.Errorf("the right password did not verify: %v", err)
	}
	if err := VerifyPassword(hash, "Correct horse battery staple"); !errors.Is(err, ErrMismatch) {
		t.Errorf("a wrong password gave %v, want ErrMismatch", err)
	}
}

// The same password twice must not produce the same hash, or a stolen database
// shows at a glance which accounts share a password.
func TestHashIsSalted(t *testing.T) {
	a, _ := HashPassword("same password")
	b, _ := HashPassword("same password")
	if a == b {
		t.Error("two hashes of the same password are identical; the salt is not doing anything")
	}
}

func TestHashCarriesItsParameters(t *testing.T) {
	hash, _ := HashPassword("x")
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=1,p=") {
		t.Errorf("hash = %q, want the PHC form with its parameters", hash)
	}
}

// Raising the cost must not lock anyone out: an old hash keeps verifying with
// the parameters it was made with, and is flagged for rehashing on sign-in.
func TestOldParametersStillVerifyAndAreFlagged(t *testing.T) {
	// A hash made when the memory cost was a quarter of today's.
	const old = "$argon2id$v=19$m=16384,t=1,p=4$c29tZXNhbHRzb21lc2FsdA$" +
		"TzNCTHRuOFBXSEJkSVVIZG9MbHVHQ0RBc3djR0hqc2Q"
	if !NeedsRehash(old) {
		t.Error("a hash with a lower memory cost was not flagged for rehashing")
	}
	fresh, _ := HashPassword("x")
	if NeedsRehash(fresh) {
		t.Error("a hash made with the current parameters was flagged for rehashing")
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	for _, bad := range []string{
		"", "plaintext", "$2y$10$bcryptstyle", "$argon2i$v=19$m=65536,t=1,p=4$c2FsdA$aGFzaA",
	} {
		if err := VerifyPassword(bad, "x"); err == nil {
			t.Errorf("VerifyPassword(%q) = nil, want an error", bad)
		}
	}
}

func TestEmptyPasswordIsRefused(t *testing.T) {
	if _, err := HashPassword(""); err == nil {
		t.Error("an empty password was hashed")
	}
}
