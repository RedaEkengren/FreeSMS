// Package auth hashes passwords and manages sessions.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters.
//
// argon2id rather than bcrypt: it is memory-hard, so an attacker with a graphics
// card does not get the enormous advantage over a defender with a server that
// bcrypt hands them. 64 MiB and one pass is the configuration RFC 9106 gives
// for the memory-constrained case, and it is a reasonable ask of a machine in
// a workshop back office.
//
// These are not constants the way a magic number is. Changing them is a
// deliberate act: existing hashes carry their own parameters, so raising the
// cost later rehashes on next sign-in rather than locking anyone out.
const (
	argonTime    = 1
	argonMemory  = 64 * 1024 // KiB
	argonKeyLen  = 32
	argonSaltLen = 16
)

// ErrMismatch is returned when a password does not match its hash. It carries
// no detail on purpose; "wrong password" and "no such user" must be
// indistinguishable to whoever is asking.
var ErrMismatch = errors.New("auth: password does not match")

func threads() uint8 {
	n := runtime.NumCPU()
	if n > 4 {
		n = 4
	}
	if n < 1 {
		n = 1
	}
	return uint8(n)
}

// HashPassword returns an encoded argon2id hash in the standard PHC string
// format, which carries the parameters and the salt alongside the digest. That
// is what makes the cost upgradeable without a migration.
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", errors.New("auth: password is empty")
	}
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: read salt: %w", err)
	}
	p := threads()
	sum := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, p, argonKeyLen)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, p,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(sum)), nil
}

// VerifyPassword reports whether password matches encoded.
//
// The comparison is constant time. A byte-by-byte comparison that returns
// early leaks, through timing, how much of a guess was correct.
func VerifyPassword(encoded, password string) error {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return fmt.Errorf("auth: not an argon2id hash")
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return fmt.Errorf("auth: unsupported argon2 version %q", parts[2])
	}

	// The parameters come from the stored hash, not from the constants above,
	// so hashes written under an older cost keep verifying after the cost is
	// raised.
	var memory, time uint32
	var parallelism uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &parallelism); err != nil {
		return fmt.Errorf("auth: unreadable argon2 parameters %q", parts[3])
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return fmt.Errorf("auth: unreadable salt")
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return fmt.Errorf("auth: unreadable digest")
	}

	got := argon2.IDKey([]byte(password), salt, time, memory, parallelism, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrMismatch
	}
	return nil
}

// NeedsRehash reports whether a stored hash was made with weaker parameters
// than the ones in use now. Call it after a successful sign-in: that is the
// only moment the plaintext is available to rehash with.
func NeedsRehash(encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 {
		return true
	}
	var memory, time uint32
	var parallelism uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &parallelism); err != nil {
		return true
	}
	return memory < argonMemory || time < argonTime
}
