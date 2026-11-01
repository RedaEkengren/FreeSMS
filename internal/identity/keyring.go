// Package identity seals the values identity records hold -- a personal
// identity number, a document's number -- with a key the database never
// holds (#153). A dump of the database, a backup of it, or anybody reading
// its tables sees ciphertext; the key lives in the application's
// configuration or a file beside it, and is backed up apart from the data,
// if at all.
//
// AES-256-GCM, an authenticated cipher: a value changed in the database, or
// moved from one record to another, does not open, rather than opening to
// something else. Each value is sealed with the record it belongs to as
// associated data for exactly that reason.
//
// Keys have ids. The first key in the configuration seals; every key in it
// opens what was sealed with it. Rotating is adding a new key first, keeping
// the old ones until every value is sealed again with the new one, and then
// dropping them: no moment in it where a value cannot be read.
package identity

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// ErrNoKeys is a keyring with no key: nothing can be sealed or opened, and
// nothing ever falls back to plaintext.
var ErrNoKeys = errors.New("identity: no key is configured, so identity values can be neither recorded nor read")

// ErrKeyMissing is a value sealed with a key this keyring does not have:
// restored from a backup without its key, or a key dropped before every
// value was sealed again.
var ErrKeyMissing = errors.New("identity: the key this value was sealed with is not configured, so it cannot be read")

// ErrTampered is a value that does not open with its key: changed, or moved
// from the record it was sealed for.
var ErrTampered = errors.New("identity: the value does not open with its key; it has been changed or moved")

// Keyring is the keys identity values are sealed and opened with.
type Keyring struct {
	current string
	keys    map[string]cipher.AEAD
}

var keyID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,40}$`)

// ParseKeys reads keys written as "id:base64key,id:base64key", each key 32
// bytes. The first seals. An empty string is a keyring with no key.
func ParseKeys(spec string) (*Keyring, error) {
	k := &Keyring{keys: map[string]cipher.AEAD{}}
	for _, part := range strings.FieldsFunc(spec, func(r rune) bool { return r == ',' || r == '\n' }) {
		part = strings.TrimSpace(part)
		if part == "" || strings.HasPrefix(part, "#") {
			continue
		}
		id, b64, ok := strings.Cut(part, ":")
		if !ok || !keyID.MatchString(id) {
			return nil, fmt.Errorf("identity: a key is written id:base64, with an id of letters, digits, - and _")
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
		if err != nil || len(raw) != 32 {
			return nil, fmt.Errorf("identity: key %q is not 32 bytes of base64", id)
		}
		if _, dup := k.keys[id]; dup {
			return nil, fmt.Errorf("identity: key %q is given twice", id)
		}
		block, err := aes.NewCipher(raw)
		if err != nil {
			return nil, err
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		k.keys[id] = aead
		if k.current == "" {
			k.current = id
		}
	}
	return k, nil
}

// Load reads the keyring from spec, or, when spec is empty, from the file at
// path; both empty is a keyring with no key.
func Load(spec, path string) (*Keyring, error) {
	if spec == "" && path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("identity: read the key file: %w", err)
		}
		spec = string(b)
	}
	return ParseKeys(spec)
}

// Current is the id of the key that seals; "" when there is none.
func (k *Keyring) Current() string {
	if k == nil {
		return ""
	}
	return k.current
}

// Seal encrypts value for the record named by aad, with the current key.
// Returned: the key's id and the nonce followed by the ciphertext.
func (k *Keyring) Seal(value, aad []byte) (string, []byte, error) {
	if k == nil || k.current == "" {
		return "", nil, ErrNoKeys
	}
	aead := k.keys[k.current]
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", nil, fmt.Errorf("identity: nonce: %w", err)
	}
	return k.current, aead.Seal(nonce, nonce, value, aad), nil
}

// Open decrypts what Seal returned, for the record named by aad.
func (k *Keyring) Open(id string, sealed, aad []byte) ([]byte, error) {
	if k == nil || len(k.keys) == 0 {
		return nil, ErrNoKeys
	}
	aead, ok := k.keys[id]
	if !ok {
		return nil, ErrKeyMissing
	}
	if len(sealed) < aead.NonceSize() {
		return nil, ErrTampered
	}
	out, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], aad)
	if err != nil {
		return nil, ErrTampered
	}
	return out, nil
}

// NewKey is a fresh key written as ParseKeys reads it, for an operator
// making one.
func NewKey(id string) (string, error) {
	if !keyID.MatchString(id) {
		return "", fmt.Errorf("identity: a key id is letters, digits, - and _")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return id + ":" + base64.StdEncoding.EncodeToString(raw), nil
}
