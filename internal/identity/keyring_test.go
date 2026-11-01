package identity_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/identity"
)

func keys(t *testing.T, ids ...string) string {
	t.Helper()
	var spec string
	for i, id := range ids {
		k, err := identity.NewKey(id)
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 {
			spec += ","
		}
		spec += k
	}
	return spec
}

// A value sealed for one record opens for that record, with its key, and
// for nothing else; the sealed bytes do not contain it.
func TestAValueOpensOnlyForItsRecordWithItsKey(t *testing.T) {
	k, err := identity.ParseKeys(keys(t, "k1"))
	if err != nil {
		t.Fatal(err)
	}
	value, aad := []byte("19800101-1234"), []byte("record-1")
	id, sealed, err := k.Seal(value, aad)
	if err != nil || id != "k1" {
		t.Fatalf("Seal: %s, %v", id, err)
	}
	if bytes.Contains(sealed, value) || bytes.Contains(sealed, []byte("1234")) {
		t.Error("the sealed bytes contain the value")
	}
	if got, err := k.Open(id, sealed, aad); err != nil || string(got) != string(value) {
		t.Errorf("Open: %q, %v", got, err)
	}
	if _, err := k.Open(id, sealed, []byte("record-2")); !errors.Is(err, identity.ErrTampered) {
		t.Errorf("opened for another record: %v", err)
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := k.Open(id, sealed, aad); !errors.Is(err, identity.ErrTampered) {
		t.Errorf("opened changed: %v", err)
	}
}

// Rotation: a new key first seals, the old one still opens what it sealed;
// without the old key, its values refuse with a clear error, and with no
// keys at all nothing is sealed or opened.
func TestKeysRotateAndAMissingKeyRefuses(t *testing.T) {
	old := keys(t, "old")
	before, _ := identity.ParseKeys(old)
	_, sealed, err := before.Seal([]byte("x"), nil)
	if err != nil {
		t.Fatal(err)
	}
	both, err := identity.ParseKeys(keys(t, "new") + "," + old)
	if err != nil || both.Current() != "new" {
		t.Fatalf("rotated keyring: %v, current %q", err, both.Current())
	}
	if got, err := both.Open("old", sealed, nil); err != nil || string(got) != "x" {
		t.Errorf("the old key during rotation: %q, %v", got, err)
	}
	after, _ := identity.ParseKeys(keys(t, "new"))
	if _, err := after.Open("old", sealed, nil); !errors.Is(err, identity.ErrKeyMissing) {
		t.Errorf("the old key dropped: %v", err)
	}
	none, err := identity.ParseKeys("")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := none.Seal([]byte("x"), nil); !errors.Is(err, identity.ErrNoKeys) {
		t.Errorf("sealed with no key: %v", err)
	}
	if _, err := none.Open("old", sealed, nil); !errors.Is(err, identity.ErrNoKeys) {
		t.Errorf("opened with no key: %v", err)
	}
}

// Keys come from configuration or a file beside it; a malformed key is
// refused at start, not when the first value is read.
func TestKeysAreReadFromConfigurationOrAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.keys")
	if err := os.WriteFile(path, []byte("# the shop's identity keys\n"+keys(t, "f1")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	k, err := identity.Load("", path)
	if err != nil || k.Current() != "f1" {
		t.Errorf("from the file: %v, current %q", err, k.Current())
	}
	for _, bad := range []string{"nocolon", "k1:notbase64!", "k1:c2hvcnQ=", keys(t, "a") + "," + keys(t, "a")} {
		if _, err := identity.ParseKeys(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
