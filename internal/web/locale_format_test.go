package web

import (
	"regexp"
	"strings"
	"testing"
)

// What a person reads is written in their language: "1,5 h" and "mån 5 okt"
// on a Swedish page. Go's Format names months and weekdays in English and
// Sprintf writes a decimal point whatever the page is, so a template that
// uses either directly writes English into every language. Found on a
// Swedish job page reading "0.0 av 1.0 h", and on Swedish invoices dated
// "7 Oct 2026".
//
// The exceptions are machine values put back into a form, which must parse
// rather than read well: a date or datetime-local control's value.
func TestTemplatesWriteNumbersAndDatesInTheReadersLanguage(t *testing.T) {
	format := regexp.MustCompile(`\.Format "([^"]*)"`)
	sprintf := regexp.MustCompile(`printf "%[^"]*f"`)
	decimal := regexp.MustCompile(`(?:\.[A-Z]\w*)*\.(?:ClockedHours|EstimateHours|LeftHours|MedianHours|Hours|Markup)\b`)

	entries, _ := Templates.ReadDir("templates")
	var dates, numbers int
	for _, e := range entries {
		raw, _ := Templates.ReadFile("templates/" + e.Name())
		src := string(raw)
		for _, m := range format.FindAllStringSubmatch(src, -1) {
			if !strings.HasPrefix(m[1], "2006-01-02") {
				t.Errorf("%s formats a time as %q directly; use $.Date", e.Name(), m[1])
			}
		}
		for _, loc := range sprintf.FindAllStringIndex(src, -1) {
			if !strings.HasSuffix(src[:loc[0]], "$.Num (") {
				t.Errorf("%s writes %s without $.Num", e.Name(), src[loc[0]:loc[1]])
			}
		}
		for _, loc := range decimal.FindAllStringIndex(src, -1) {
			before := src[:loc[0]]
			if strings.HasSuffix(before, "$.Num ") || strings.HasSuffix(before, `value="{{`) {
				continue
			}
			t.Errorf("%s writes %s without $.Num", e.Name(), src[loc[0]:loc[1]])
		}
		dates += strings.Count(src, "$.Date ")
		numbers += strings.Count(src, "$.Num ")
	}
	if dates < 15 || numbers < 10 {
		t.Errorf("found %d dates and %d numbers through the locale; the patterns have stopped matching", dates, numbers)
	}
}
