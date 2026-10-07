// Package i18n renders text in the reader's language.
//
// Translations are data, not code: a JSON file per language that somebody who
// does not program can edit, and that a translator can be handed without a Go
// toolchain. English is the source language and the fallback.
package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed catalogues/*.json
var files embed.FS

// Message is one translatable string.
//
// Plural forms are a map because "add an s" is not how plurals work in most
// languages, and pretending otherwise is how software ends up saying
// "1 timmar".
type Message struct {
	One   string `json:"one,omitempty"`
	Other string `json:"other,omitempty"`
}

// Catalogue is one language.
type Catalogue struct {
	Locale   string
	Name     string
	Messages map[string]Message

	// How the language writes a number and a date. A decimal comma is not a
	// translation of a word, and neither are the names of the months, so
	// they are here rather than among the messages. Empty means English:
	// a point, and Go's own names.
	Decimal  string   `json:"decimal,omitempty"`
	Months   []string `json:"months,omitempty"`   // short, January first
	Weekdays []string `json:"weekdays,omitempty"` // short, Sunday first, as time.Weekday counts
}

// Catalogues holds every language that shipped, and answers lookups.
type Catalogues struct {
	mu       sync.RWMutex
	byLocale map[string]*Catalogue
	fallback string
	strict   bool
}

// Load reads the embedded catalogues.
//
// strict turns a missing translation into a visible marker rather than a
// silent fall back to English. On in development, off in production: a missing
// string should be obvious to whoever is working on it and invisible to a
// workshop.
func Load(fallback string, strict bool) (*Catalogues, error) {
	entries, err := files.ReadDir("catalogues")
	if err != nil {
		return nil, fmt.Errorf("i18n: read catalogues: %w", err)
	}

	c := &Catalogues{byLocale: map[string]*Catalogue{}, fallback: fallback, strict: strict}
	for _, e := range entries {
		body, err := files.ReadFile("catalogues/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("i18n: read %s: %w", e.Name(), err)
		}
		var cat Catalogue
		if err := json.Unmarshal(body, &cat); err != nil {
			return nil, fmt.Errorf("i18n: %s is not valid JSON: %w", e.Name(), err)
		}
		if cat.Locale == "" {
			return nil, fmt.Errorf("i18n: %s does not say which locale it is", e.Name())
		}
		if n := len(cat.Months); n != 0 && n != 12 {
			return nil, fmt.Errorf("i18n: %s names %d months", e.Name(), n)
		}
		if n := len(cat.Weekdays); n != 0 && n != 7 {
			return nil, fmt.Errorf("i18n: %s names %d weekdays", e.Name(), n)
		}
		c.byLocale[cat.Locale] = &cat
	}
	if _, ok := c.byLocale[fallback]; !ok {
		return nil, fmt.Errorf("i18n: the fallback locale %q has no catalogue", fallback)
	}
	return c, nil
}

// Available lists the locales that shipped, for a language picker.
func (c *Catalogues) Available() []Catalogue {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]Catalogue, 0, len(c.byLocale))
	for _, cat := range c.byLocale {
		out = append(out, *cat)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Locale < out[j].Locale })
	return out
}

// Has reports whether a locale shipped.
func (c *Catalogues) Has(locale string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.byLocale[locale]
	return ok
}

// Printer renders messages in one language.
type Printer struct {
	c      *Catalogues
	locale string
}

// For returns a printer for a locale, falling back when it did not ship.
func (c *Catalogues) For(locale string) *Printer {
	if !c.Has(locale) {
		locale = c.fallback
	}
	return &Printer{c: c, locale: locale}
}

// Locale is the language actually being rendered.
func (p *Printer) Locale() string { return p.locale }

// T renders a message, substituting %s and friends.
//
// The key is the English text. That reads better at the call site than an
// identifier, survives a catalogue going missing, and means a string nobody
// has translated yet still says something sensible.
func (p *Printer) T(key string, args ...any) string {
	return p.format(p.lookup(key, false), key, args...)
}

// N renders a message with a count, choosing the plural form.
func (p *Printer) N(key string, count int, args ...any) string {
	text := p.lookup(key, count == 1)
	all := append([]any{count}, args...)
	return p.format(text, key, all...)
}

// Form is a message's text for a count with nothing substituted, for a page
// to hand to a script that fills in the count itself -- the offline queue's
// banner counts work the server never saw. The choice of form is the one N
// makes, so the two cannot disagree.
func (p *Printer) Form(key string, count int) string {
	return p.lookup(key, count == 1)
}

// Number writes a decimal the domain formatted -- "1.5", "45.0%" -- the way
// the reader's language does. The domain keeps a point so that a value put
// back into a form parses; only what a person reads changes.
func (p *Printer) Number(s string) string {
	if cat := p.catalogue(); cat != nil && cat.Decimal != "" {
		return strings.ReplaceAll(s, ".", cat.Decimal)
	}
	return s
}

// Date formats t with a Go layout, naming months and weekdays in the
// reader's language. Go's own names are English whatever the page is, which
// put "7 Oct 2026" on a Swedish invoice.
//
// A layout with the long names ("January", "Monday") is left in English
// rather than half translated into "okt uary": nothing uses them, and a
// catalogue carries short ones.
func (p *Printer) Date(t time.Time, layout string) string {
	cat := p.catalogue()
	if cat == nil || (len(cat.Months) == 0 && len(cat.Weekdays) == 0) ||
		strings.Contains(layout, "January") || strings.Contains(layout, "Monday") {
		return t.Format(layout)
	}
	// Go's layout tokens become marks Format leaves alone, and the marks
	// become the names.
	const month, weekday = "\x00M\x00", "\x00W\x00"
	if len(cat.Months) == 12 {
		layout = strings.ReplaceAll(layout, "Jan", month)
	}
	if len(cat.Weekdays) == 7 {
		layout = strings.ReplaceAll(layout, "Mon", weekday)
	}
	out := t.Format(layout)
	if len(cat.Months) == 12 {
		out = strings.ReplaceAll(out, month, cat.Months[t.Month()-1])
	}
	if len(cat.Weekdays) == 7 {
		out = strings.ReplaceAll(out, weekday, cat.Weekdays[t.Weekday()])
	}
	return out
}

func (p *Printer) catalogue() *Catalogue {
	p.c.mu.RLock()
	defer p.c.mu.RUnlock()
	return p.c.byLocale[p.locale]
}

func (p *Printer) lookup(key string, singular bool) string {
	p.c.mu.RLock()
	defer p.c.mu.RUnlock()

	for _, locale := range []string{p.locale, p.c.fallback} {
		cat, ok := p.c.byLocale[locale]
		if !ok {
			continue
		}
		if m, ok := cat.Messages[key]; ok {
			if singular && m.One != "" {
				return m.One
			}
			if m.Other != "" {
				return m.Other
			}
		}
		// Never mark the fallback language. Its keys are the text, so an
		// "untranslated" English string is simply English -- marking it would
		// make every screen unreadable in development for no reason.
		if locale == p.locale && p.locale != p.c.fallback && p.c.strict {
			// Loud in development, so a string nobody translated is obvious to
			// whoever is looking at the screen rather than to a customer.
			return "!!" + key + "!!"
		}
	}
	// Never an empty string: the key is English, so the worst case still reads.
	return key
}

func (p *Printer) format(text, key string, args ...any) string {
	if len(args) == 0 {
		return text
	}
	if !strings.Contains(text, "%") {
		return text
	}
	return fmt.Sprintf(text, args...)
}
