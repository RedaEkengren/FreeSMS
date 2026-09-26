package server

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/web"
)

// A template file that exists and is not in the page list renders as a 500
// with one line in the log and nothing on the screen. That is how the invoice
// document shipped broken for as long as it took to open it.
//
// layout is the frame every page is parsed with rather than a page of its own.
func TestEveryTemplateIsRegistered(t *testing.T) {
	pages, err := parseTemplates()
	if err != nil {
		t.Fatalf("parseTemplates: %v", err)
	}

	entries, err := fs.ReadDir(web.Templates, "templates")
	if err != nil {
		t.Fatalf("read templates: %v", err)
	}
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".html")
		if name == "layout" || !strings.HasSuffix(e.Name(), ".html") {
			continue
		}
		if _, ok := pages[name]; !ok {
			t.Errorf("templates/%s is not in the page list, so rendering it is a 500", e.Name())
		}
	}
}
