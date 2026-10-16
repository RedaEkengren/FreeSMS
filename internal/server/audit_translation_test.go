package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/i18n"
)

// Every sentence a person can be shown has a Swedish translation.
//
// "Adding a string without a catalogue key is not acceptable in new work" was a
// rule kept by remembering. This keeps it by reading the source: the template
// strings that go through T and N, the sentences a handler gives an error page,
// the printer's own calls, and the messages the workshop's refusals carry. A
// string that is the shop's own words -- a part's name, a checklist label, a
// technician's note -- never goes through any of these, which is how it stays
// untranslated on purpose.

var templateString = regexp.MustCompile(`(?:\.|\$\.)(?:T|N)\s+"((?:[^"\\]|\\.)*)"`)

func unquote(t *testing.T, lit string) string {
	t.Helper()
	s, err := strconv.Unquote(lit)
	if err != nil {
		t.Fatalf("unquote %s: %v", lit, err)
	}
	return s
}

// sourceStrings are the sentences in the Go source that reach a reader.
func sourceStrings(t *testing.T) map[string]string {
	t.Helper()
	found := map[string]string{}
	add := func(s, where string) {
		if strings.TrimSpace(s) != "" {
			found[s] = where
		}
	}

	// Handlers: what is given to the error page, to the helpers that wrap it,
	// and to the printer.
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				// A page's title: pageData{Title: "..."}.
				// A message put on the page as data.Error = "..." reaches a reader
				// as it is written, so it is a sentence too. The scan screen's
				// "Nothing answers to that code." was one nobody had translated,
				// found while drawing a scene of it.
				if as, ok := n.(*ast.AssignStmt); ok && len(as.Lhs) == 1 && len(as.Rhs) == 1 {
					if sel, ok := as.Lhs[0].(*ast.SelectorExpr); ok && sel.Sel.Name == "Error" {
						if lit, ok := as.Rhs[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							add(unquote(t, lit.Value), filepath.Base(name))
						}
					}
				}
				if kv, ok := n.(*ast.KeyValueExpr); ok {
					if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Error" {
						if lit, ok := kv.Value.(*ast.BasicLit); ok && lit.Kind == token.STRING {
							add(unquote(t, lit.Value), filepath.Base(name))
						}
					}
					if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Title" {
						if lit, ok := kv.Value.(*ast.BasicLit); ok && lit.Kind == token.STRING {
							add(unquote(t, lit.Value), filepath.Base(name))
						}
					}
					return true
				}
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var fn string
				switch f := call.Fun.(type) {
				case *ast.Ident:
					fn = f.Name
				case *ast.SelectorExpr:
					fn = f.Sel.Name
				}
				var strs []*ast.BasicLit
				for _, a := range call.Args {
					if lit, ok := a.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						strs = append(strs, lit)
					}
				}
				switch fn {
				case "renderError":
					// title and message are the last two arguments.
					for _, lit := range strs {
						add(unquote(t, lit.Value), filepath.Base(name))
					}
				case "bad", "refuse":
					for _, lit := range strs {
						add(unquote(t, lit.Value), filepath.Base(name))
					}
				case "T":
					// p.T("..."), d.T("...") -- the first argument is the key.
					if len(call.Args) > 0 {
						if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							add(unquote(t, lit.Value), filepath.Base(name))
						}
					}
				}
				return true
			})
		}
	}

	// The workshop's refusals: "workshop: invalid: <sentence>".
	refusal := regexp.MustCompile(`fmt\.Errorf\("%w: ((?:[^"\\]|\\.)*)"`)
	for _, dir := range []string{"../workshop", "../auth"} {
		files, _ := filepath.Glob(dir + "/*.go")
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range refusal.FindAllStringSubmatch(string(b), -1) {
				s, err := strconv.Unquote(`"` + m[1] + `"`)
				if err != nil {
					continue
				}
				add(s, filepath.Base(f))
			}
		}
	}
	return found
}

func templateStrings(t *testing.T) map[string]string {
	t.Helper()
	found := map[string]string{}
	files, _ := filepath.Glob("../web/templates/*.html")
	if len(files) < 20 {
		t.Fatalf("found %d templates; the path has stopped working", len(files))
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range templateString.FindAllStringSubmatch(string(b), -1) {
			s, err := strconv.Unquote(`"` + m[1] + `"`)
			if err != nil {
				s = m[1]
			}
			found[s] = filepath.Base(f)
		}
	}
	return found
}

// Not shown to a reader as written: a pattern with nothing but verbs in it, a
// word passed through for formatting, or something the printer is handed that
// is built from the database.
func notASentence(s string) bool {
	letters := 0
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			letters++
		}
	}
	return letters < 3 || strings.Trim(s, "%sdqfv. ") == ""
}

func TestEverySentenceAReaderCanBeShownHasASwedishTranslation(t *testing.T) {
	cats, err := i18n.Load("en", false)
	if err != nil {
		t.Fatal(err)
	}
	sv := cats.Messages("sv")
	var missing []string
	check := func(group string, strs map[string]string) {
		for s, where := range strs {
			if notASentence(s) {
				continue
			}
			if _, ok := sv[s]; !ok {
				missing = append(missing, group+" "+where+": "+s)
			}
		}
	}
	check("template", templateStrings(t))
	check("handler", sourceStrings(t))
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d sentences have no Swedish translation; add them to internal/i18n/catalogues/sv.json:\n%s",
			len(missing), strings.Join(missing, "\n"))
	}
}
