// Package site generates the marketing page from the product.
//
// BraLeads settled the rule for its landing page: real data or no
// animation. Its scenes take their numbers from the product's own query
// code, and the link under each one runs that search. FreeSMS has no central
// database to read from -- every workshop runs its own -- so a scene here is
// bound to what is real instead:
//
//   - every word the product would show comes from the product: the
//     catalogue, the board's own choice of what a car is waiting for, a
//     state's own label. A scene cannot say what the screen does not;
//   - every scene names the browser test that performs its flow against the
//     real product, and the page links it. A scene whose test is missing is
//     not generated.
//
// Anything invented -- a plate, a customer, a technician -- is marked as an
// example on the page.
package site

import (
	"bytes"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"strings"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/i18n"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Source is the page as written, Output what is published. Both relative to
// the repository root.
const (
	Source = "site/src/index.html"
	Output = "site/index.html"
	Locale = "sv"
)

// Repository is where the page links its tests.
const Repository = "https://github.com/RedaEkengren/FreeSMS/blob/main/"

// Generate renders the page from the repository at root.
func Generate(root string) ([]byte, error) {
	// Strict: a word nobody translated comes out marked rather than in
	// English, and is refused below.
	cats, err := i18n.Load("en", true)
	if err != nil {
		return nil, err
	}
	p := cats.For(Locale)
	if p.Locale() != Locale {
		return nil, fmt.Errorf("site: no %s catalogue", Locale)
	}

	var missing []string
	funcs := template.FuncMap{
		// The product's words.
		"t": func(key string, args ...any) string { return p.T(key, args...) },
		"n": func(key string, count int, args ...any) string { return p.N(key, count, args...) },
		// What the board says a car is waiting for, decided by the board's
		// own code from a state and whoever has a clock running.
		"waiting": func(state, workingNow string) string {
			e := workshop.BoardEntry{State: state, WorkingNow: workingNow}
			return p.T(e.Waiting(), e.WaitingArg())
		},
		// A state as the job page labels it.
		"state": func(state string) (string, error) {
			label := workshop.State(state).Label()
			if label == state {
				return "", fmt.Errorf("site: %q is not a state", state)
			}
			return p.T(label), nil
		},
		// A role as the header names it.
		"role": func(role string) (string, error) {
			label := access.Role(role).Label()
			if label == role {
				return "", fmt.Errorf("site: %q is not a role", role)
			}
			return p.T(label), nil
		},
		// The browser test that performs a scene. No test, no scene.
		"test": func(file, title string) (string, error) {
			src, err := os.ReadFile(filepath.Join(root, file))
			if err != nil {
				return "", fmt.Errorf("site: the scene's test %s: %w", file, err)
			}
			if !strings.Contains(string(src), "test('"+title+"'") {
				return "", fmt.Errorf("site: %s has no test called %q", file, title)
			}
			return Repository + file, nil
		},
	}

	raw, err := os.ReadFile(filepath.Join(root, Source))
	if err != nil {
		return nil, err
	}
	tmpl, err := template.New("index").Funcs(funcs).Parse(string(raw))
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, nil); err != nil {
		return nil, err
	}
	// After the doctype, which has to come first.
	doctype, rest, _ := strings.Cut(out.String(), "\n")
	out.Reset()
	out.WriteString(doctype + "\n<!-- Generated from " + Source + " by go run ./cmd/site. Edit that, not this. -->\n" + rest)
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "!!") {
			missing = append(missing, strings.TrimSpace(line))
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("site: untranslated words on the page:\n%s", strings.Join(missing, "\n"))
	}
	return out.Bytes(), nil
}
