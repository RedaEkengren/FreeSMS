package server

import (
	"bytes"
	"strings"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/i18n"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// The name an erased person was replaced with is one English sentence in the
// database. A Swedish reader must not be shown it, wherever on a page a name
// is printed -- and a form field must still carry what is stored, or saving
// the form would write the translation over the data.
func TestAnErasedNameIsShownInTheReadersLanguage(t *testing.T) {
	cats, err := i18n.Load("en", false)
	if err != nil {
		t.Fatal(err)
	}
	stored := "Erased at the person&#39;s request"
	page := `<div>#12 &middot; ` + stored + `</div><input value="` + stored + `">`

	sv := bytes.NewBufferString(page)
	translateErasedName(sv, cats.For("sv"))
	if !strings.Contains(sv.String(), "<div>#12 &middot; Raderad på personens begäran</div>") {
		t.Errorf("text not translated: %s", sv)
	}
	if !strings.Contains(sv.String(), `value="`+stored+`"`) {
		t.Errorf("form value was translated: %s", sv)
	}

	en := bytes.NewBufferString(page)
	translateErasedName(en, cats.For("en"))
	if en.String() != page {
		t.Errorf("English page changed: %s", en)
	}
	if workshop.ErasedName != "Erased at the person's request" {
		t.Errorf("the constant moved; the test's page must follow it")
	}
}
