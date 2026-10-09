package web

import (
	"regexp"
	"strings"
	"testing"
)

// Dark mode breaks one screen at a time: a colour written straight into a
// rule is right in light and wrong in dark, and nobody looks at that screen
// in dark until a technician does. Every colour is a theme variable, defined
// above the line that ends the themes, and nowhere below it.
func TestEveryColourIsATheme(t *testing.T) {
	css, err := Static.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	const end = "End of the themes."
	_, rules, ok := strings.Cut(string(css), end)
	if !ok {
		t.Fatalf("app.css has no %q line", end)
	}
	colour := regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b|\brgba?\(|\bhsla?\(`)
	for i, line := range strings.Split(rules, "\n") {
		if m := colour.FindString(line); m != "" {
			t.Errorf("a colour below the themes, %q: %s", m, strings.TrimSpace(line))
			_ = i
		}
	}
}
