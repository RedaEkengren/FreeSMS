package i18n

import (
	"encoding/json"
	"strings"
	"testing"
)

func load(t *testing.T, strict bool) *Catalogues {
	t.Helper()
	c, err := Load("en", strict)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

func TestSwedishShipped(t *testing.T) {
	c := load(t, false)
	if !c.Has("sv") {
		t.Fatal("there is no Swedish catalogue")
	}
	if got := c.For("sv").T("Sign in"); got != "Logga in" {
		t.Errorf("T(Sign in) = %q, want Logga in", got)
	}
}

// The key is the English text, so a string nobody has translated still says
// something sensible rather than an identifier or nothing at all.
func TestAnUntranslatedStringFallsBackToItsKey(t *testing.T) {
	c := load(t, false)
	const key = "Something nobody has translated yet"
	if got := c.For("sv").T(key); got != key {
		t.Errorf("T = %q, want the English key", got)
	}
}

// Loud in development, invisible to a workshop.
func TestStrictModeMakesAMissingTranslationObvious(t *testing.T) {
	strict := load(t, true)
	got := strict.For("sv").T("Something nobody has translated yet")
	if !strings.HasPrefix(got, "!!") {
		t.Errorf("T = %q, want a visible marker in strict mode", got)
	}

	// And the fallback language itself is never marked: English has no
	// catalogue entries because English is the source.
	if got := strict.For("en").T("Sign in"); got != "Sign in" {
		t.Errorf("English T = %q, want the key itself", got)
	}
}

// "Add an s" is not how plurals work, and pretending otherwise is how software
// ends up saying "1 timmar".
func TestPluralFormsAreChosen(t *testing.T) {
	sv := load(t, false).For("sv")
	if got := sv.N("Waiting for %d part", 1); got != "Väntar på 1 del" {
		t.Errorf("N(1) = %q", got)
	}
	if got := sv.N("Waiting for %d part", 3); got != "Väntar på 3 delar" {
		t.Errorf("N(3) = %q", got)
	}

	en := load(t, false).For("en")
	if got := en.N("Waiting for %d part", 2); !strings.Contains(got, "2") {
		t.Errorf("English N = %q, want the count in it", got)
	}
}

// A locale nobody shipped is not an error, it is English.
func TestAnUnknownLocaleFallsBack(t *testing.T) {
	c := load(t, false)
	p := c.For("kl")
	if p.Locale() != "en" {
		t.Errorf("locale = %q, want the fallback", p.Locale())
	}
}

func TestAvailableListsWhatShipped(t *testing.T) {
	got := load(t, false).Available()
	if len(got) < 2 {
		t.Fatalf("got %d catalogues, want at least English and Swedish", len(got))
	}
	var names []string
	for _, c := range got {
		names = append(names, c.Locale)
		if c.Name == "" {
			t.Errorf("catalogue %q has no name for a language picker", c.Locale)
		}
	}
	if names[0] != "en" {
		t.Errorf("catalogues are not in a stable order: %v", names)
	}
}

// Swedish text must survive: a catalogue that mangles its own characters is
// worse than no catalogue.
func TestSwedishCharactersSurvive(t *testing.T) {
	sv := load(t, false).For("sv")
	if got := sv.T("Password"); got != "Lösenord" {
		t.Errorf("T(Password) = %q, want Lösenord", got)
	}
	if got := sv.T("Nothing open."); !strings.Contains(got, "ö") {
		t.Errorf("T = %q", got)
	}
}

// Every message with a plural form has an English plural too.
//
// The keys are English singular, so English needs a catalogue entry only
// where a count is involved -- and the English catalogue was empty, so every
// counted phrase read "Waiting for 2 part" and "3 checkpoint" in English
// while Swedish had both forms.
func TestEveryPluralHasAnEnglishPlural(t *testing.T) {
	read := func(name string) map[string]map[string]string {
		raw, err := files.ReadFile("catalogues/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var c struct {
			Messages map[string]map[string]string `json:"messages"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		return c.Messages
	}
	en := read("en.json")
	for key, forms := range read("sv.json") {
		if forms["one"] == "" {
			continue
		}
		e := en[key]
		if e["other"] == "" || e["other"] == key {
			t.Errorf("%q is counted in Swedish and has no English plural", key)
		}
	}

	if got := load(t, false).For("en").N("Waiting for %d part", 2); got != "Waiting for 2 parts" {
		t.Errorf("English N(2) = %q", got)
	}
}
