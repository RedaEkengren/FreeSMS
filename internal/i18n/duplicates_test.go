package i18n

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"testing"
)

// A key written twice in a catalogue is not an error to encoding/json: the
// second silently wins. "Sales" was added once as a role's label and so
// turned the figures page's heading "Försäljning" into "Säljare". Every
// object in every catalogue has each key once.
func TestNoCatalogueHasAKeyTwice(t *testing.T) {
	names, err := fs.Glob(files, "catalogues/*.json")
	if err != nil || len(names) == 0 {
		t.Fatalf("no catalogues: %v", err)
	}
	for _, name := range names {
		raw, err := files.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		var walk func(path string)
		walk = func(path string) {
			tok, err := dec.Token()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			switch tok {
			case json.Delim('{'):
				seen := map[string]bool{}
				for dec.More() {
					k, err := dec.Token()
					if err != nil {
						t.Fatalf("%s: %v", name, err)
					}
					key := k.(string)
					if seen[key] {
						t.Errorf("%s: %q is in %s twice; the second wins without a word", name, key, path)
					}
					seen[key] = true
					walk(path + "/" + key)
				}
				dec.Token()
			case json.Delim('['):
				for dec.More() {
					walk(path + "[]")
				}
				dec.Token()
			}
		}
		walk("")
	}
}
