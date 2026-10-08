package site

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The published page is what the generator makes of the product as it is
// now. Change a word in the catalogue, or what the board says a car is
// waiting for, and this fails until the page is generated again -- so the
// scene cannot go on saying what the screen no longer does.
func TestThePageIsGeneratedFromTheProductAsItIs(t *testing.T) {
	for _, p := range Pages {
		got, err := Generate("../..", p)
		if err != nil {
			t.Fatalf("%s: %v", p.Source, err)
		}
		committed, err := os.ReadFile(filepath.Join("../..", p.Output))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, committed) {
			t.Errorf("%s is not what %s generates from the product today; run go run ./cmd/site", p.Output, p.Source)
		}
	}
}

// Every scene is on every page, linked to the same test: a language that
// quietly drops a scene, or keeps one whose flow changed, is a page saying
// less, or something else.
func TestEveryLanguageShowsTheSameScenes(t *testing.T) {
	tests := regexp.MustCompile(`\{\{test "([^"]+)" "([^"]+)"\}\}`)
	var first []string
	for i, p := range Pages {
		src, err := os.ReadFile(filepath.Join("../..", p.Source))
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, m := range tests.FindAllStringSubmatch(string(src), -1) {
			got = append(got, m[1]+" "+m[2])
		}
		if i == 0 {
			first = got
		} else if strings.Join(got, "\n") != strings.Join(first, "\n") {
			t.Errorf("%s shows the scenes\n%s\n%s shows\n%s", Pages[0].Source, strings.Join(first, "\n"), p.Source, strings.Join(got, "\n"))
		}
	}
}

// A root holding only a page, to see what the generator refuses.
func pageOnly(t *testing.T, page string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, filepath.Dir(Pages[0].Source)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, Pages[0].Source), []byte(page), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// No test that performs the scene, no scene.
func TestASceneWithoutItsTestIsNotGenerated(t *testing.T) {
	_, err := Generate(pageOnly(t, "<!doctype html>\n"+`<a href="{{test "e2e/tests/99-gone.spec.ts" "a flow nobody runs"}}">`), Pages[0])
	if err == nil || !strings.Contains(err.Error(), "99-gone") {
		t.Errorf("generated a scene whose test does not exist (err %v)", err)
	}
}

// A word the product does not have in Swedish is not put on a Swedish page
// in English.
func TestAnUntranslatedWordIsRefused(t *testing.T) {
	_, err := Generate(pageOnly(t, "<!doctype html>\n"+`{{t "A sentence no screen has ever said"}}`), Pages[0])
	if err == nil || !strings.Contains(err.Error(), "untranslated") {
		t.Errorf("an untranslated word reached the page (err %v)", err)
	}
}

// A state or a role that does not exist is a typo, not a label.
func TestAStateOrRoleThatDoesNotExistIsRefused(t *testing.T) {
	for _, page := range []string{`{{state "in_progres"}}`, `{{role "mechanic"}}`} {
		if _, err := Generate(pageOnly(t, "<!doctype html>\n"+page), Pages[0]); err == nil {
			t.Errorf("%s generated", page)
		}
	}
}

// Without JavaScript a scene shows its still frame: one caption, one
// chapter -- not every moment of it stacked on the same spot.
func TestWithoutJavaScriptASceneIsOneMoment(t *testing.T) {
	caption := regexp.MustCompile(`<div class="an-cap[^"]*"`)
	for _, p := range Pages {
		page, err := Generate("../..", p)
		if err != nil {
			t.Fatal(err)
		}
		scenes := strings.Split(string(page), `<div class="an-wrap"`)[1:]
		if len(scenes) == 0 {
			t.Fatalf("%s has no scenes", p.Output)
		}
		for i, scene := range scenes {
			shown := 0
			for _, c := range caption.FindAllString(scene, -1) {
				if !strings.Contains(c, "an-off") {
					shown++
				}
			}
			if shown != 1 {
				t.Errorf("%s, scene %d: %d captions on screen without JavaScript, want 1", p.Output, i+1, shown)
			}
		}
	}
}
