package workshop

import "testing"

// A value written into a file a spreadsheet opens never runs: anything
// starting as a formula can is led by an apostrophe -- a minus included,
// unless the whole value is a number (review r7, #1).
func TestASpreadsheetCellNeverRuns(t *testing.T) {
	for in, want := range map[string]string{
		"Rita":                 "Rita",
		"=1+1":                 "'=1+1",
		"+46 70":               "'+46 70",
		"@SUM(A1)":             "'@SUM(A1)",
		"\tx":                  "'\tx",
		"\rx":                  "'\rx",
		"-1+cmd|' /C calc'!A0": "'-1+cmd|' /C calc'!A0",
		"-cmd":                 "'-cmd",
		"-500":                 "-500",
		"-12.50":               "-12.50",
		"-0,5":                 "-0,5",
		"2031-05-10 09:00":     "2031-05-10 09:00",
		"":                     "",
	} {
		if got := SpreadsheetCell(in); got != want {
			t.Errorf("%q written as %q, want %q", in, got, want)
		}
	}
}
