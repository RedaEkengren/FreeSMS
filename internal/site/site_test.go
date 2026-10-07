package site

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The published page is what the generator makes of the product as it is
// now. Change a word in the catalogue, or what the board says a car is
// waiting for, and this fails until the page is generated again -- so the
// scene cannot go on saying what the screen no longer does.
func TestThePageIsGeneratedFromTheProductAsItIs(t *testing.T) {
	got, err := Generate("../..")
	if err != nil {
		t.Fatal(err)
	}
	committed, err := os.ReadFile(filepath.Join("../..", Output))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, committed) {
		t.Fatalf("%s is not what %s generates from the product today; run go run ./cmd/site", Output, Source)
	}
}

// A root holding only a page, to see what the generator refuses.
func pageOnly(t *testing.T, page string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, filepath.Dir(Source)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, Source), []byte(page), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// No test that performs the scene, no scene.
func TestASceneWithoutItsTestIsNotGenerated(t *testing.T) {
	_, err := Generate(pageOnly(t, "<!doctype html>\n"+`<a href="{{test "e2e/tests/99-gone.spec.ts" "a flow nobody runs"}}">`))
	if err == nil || !strings.Contains(err.Error(), "99-gone") {
		t.Errorf("generated a scene whose test does not exist (err %v)", err)
	}
}

// A word the product does not have in Swedish is not put on a Swedish page
// in English.
func TestAnUntranslatedWordIsRefused(t *testing.T) {
	_, err := Generate(pageOnly(t, "<!doctype html>\n"+`{{t "A sentence no screen has ever said"}}`))
	if err == nil || !strings.Contains(err.Error(), "untranslated") {
		t.Errorf("an untranslated word reached the page (err %v)", err)
	}
}

// A state or a role that does not exist is a typo, not a label.
func TestAStateOrRoleThatDoesNotExistIsRefused(t *testing.T) {
	for _, page := range []string{`{{state "in_progres"}}`, `{{role "mechanic"}}`} {
		if _, err := Generate(pageOnly(t, "<!doctype html>\n"+page)); err == nil {
			t.Errorf("%s generated", page)
		}
	}
}
