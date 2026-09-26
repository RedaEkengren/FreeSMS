package web

import (
	"strings"
	"testing"
)

// blocks splits the stylesheet into its top level and each @media block, so a
// rule can be asked which of them it lives in.
//
// A brace counter rather than a parser: the file is two hundred and fifty
// lines and hand-written, and a dependency to read it would cost more than it
// is worth.
func blocks(t *testing.T) (top string, media map[string]string) {
	t.Helper()
	raw, err := Static.ReadFile("static/app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	css := string(raw)

	media = map[string]string{}
	var topB strings.Builder
	for i := 0; i < len(css); {
		at := strings.Index(css[i:], "@media")
		if at < 0 {
			topB.WriteString(css[i:])
			break
		}
		at += i
		topB.WriteString(css[i:at])

		open := strings.Index(css[at:], "{")
		if open < 0 {
			t.Fatalf("@media at %d has no body", at)
		}
		query := strings.TrimSpace(css[at+len("@media") : at+open])
		depth, j := 0, at+open
		for ; j < len(css); j++ {
			switch css[j] {
			case '{':
				depth++
			case '}':
				depth--
			}
			if depth == 0 {
				break
			}
		}
		media[query] = css[at+open : j]
		i = j + 1
	}
	return topB.String(), media
}

// The phone is the default and the desk is the exception. Every rule that
// widens, splits or pins something has to live inside the width query, or a
// technician in a bay gets a sticky panel eating a fifth of the screen and a
// four-column table clipped by overflow-x: hidden.
//
// This is a contract about where a rule sits, not about how it looks. The
// looking is done in a browser and cannot be done here, which is worth saying
// rather than pretending a test covers it.
func TestDesktopRulesStayInsideTheBreakpoint(t *testing.T) {
	top, media := blocks(t)

	const wide = "(min-width: 60rem)"
	desk, ok := media[wide]
	if !ok {
		t.Fatalf("no %s block; the desk layout has nowhere to live", wide)
	}

	for _, rule := range []struct{ fragment, why string }{
		{".split { display: grid", "two columns on a phone is one column too many"},
		{".split > .side { position: sticky", "a sticky rail under a sticky header eats a phone screen"},
		{".picks { display: grid", "the two pick lists side by side need width"},
		{".board { display: grid", "the board is a stack on a phone"},
		{"main { max-width: 88rem", "88rem is the desk width, not the phone's 16px gutter"},
	} {
		if !strings.Contains(desk, rule.fragment) {
			t.Errorf("%q is not in the %s block", rule.fragment, wide)
		}
		if strings.Contains(top, rule.fragment) {
			t.Errorf("%q applies at every width; %s", rule.fragment, rule.why)
		}
	}
}

// The line table collapses below the breakpoint. Four columns of figures do
// not fit on a phone, and the body forbids a sideways page, so without this
// the numbers are simply cut off.
func TestTheLineTableCollapsesOnAPhone(t *testing.T) {
	_, media := blocks(t)

	const narrow = "(max-width: 59.999rem)"
	phone, ok := media[narrow]
	if !ok {
		t.Fatalf("no %s block; the line table never collapses", narrow)
	}
	for _, want := range []string{
		"table.lines-table thead { display: none; }",
		"data-label",
	} {
		if !strings.Contains(phone, want) {
			t.Errorf("the phone block is missing %q", want)
		}
	}
}

// Red means one thing. If it becomes the accent, the left border on a late car
// stops being a signal and becomes decoration.
func TestRedIsReservedForUrgency(t *testing.T) {
	raw, err := Static.ReadFile("static/app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	css := string(raw)

	for _, want := range []string{
		".card.late { border-left: 4px solid var(--danger); }",
		".tag.late { color: var(--danger); border-color: var(--danger); }",
		".tag.warranty { color: var(--attention); border-color: var(--attention); }",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("missing %q: the two urgencies have been collapsed back into one", want)
		}
	}
	// The accent is what buttons and links are painted with. It must not be
	// the danger colour under another name.
	for _, line := range strings.Split(css, "\n") {
		l := strings.TrimSpace(line)
		if !strings.HasPrefix(l, "--accent:") {
			continue
		}
		if strings.Contains(css, "--danger: "+strings.TrimSuffix(strings.TrimPrefix(l, "--accent: "), ";")) {
			t.Errorf("the accent and the danger colour are the same value: %q", l)
		}
	}
}
