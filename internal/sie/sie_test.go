package sie

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func day(y, m, d int) time.Time { return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC) }

func sample() File {
	return File{
		Program: "FreeSMS", ProgramVer: "1.0",
		Generated:   day(2026, 9, 23),
		CompanyName: "Öbergs Verkstad",
		OrgNumber:   "556677-8899",
		YearFrom:    day(2026, 1, 1), YearTo: day(2026, 12, 31),
		Accounts: map[int]string{3010: "Försäljning", 2611: "Utgående moms 25%", 1510: "Kundfordringar"},
		Verifications: []Verification{{
			Series: "A", Number: "1", Date: day(2026, 9, 21), Text: "Faktura A-1",
			Entries: []Entry{
				{Account: 1510, Minor: 373313},
				{Account: 3010, Minor: -298650},
				{Account: 2611, Minor: -74663},
			},
		}},
	}
}

// The single most common way this export goes out broken.
func TestTheFileIsCP437AndNotUTF8(t *testing.T) {
	out, err := Write(sample())
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Ö is one byte, 153, not the two bytes UTF-8 would give.
	if !bytes.Contains(out, []byte{153, 'b', 'e', 'r', 'g'}) {
		t.Error("Öbergs is not encoded as CP437")
	}
	if bytes.Contains(out, []byte("Ö")) {
		t.Error("the file contains UTF-8; an accountant would see mangled names")
	}
	// å in Försäljning: ä is 132.
	if !bytes.Contains(out, []byte{'F', 'o' + 0, 'r'}) && !bytes.Contains(out, []byte{132}) {
		t.Error("Swedish characters in account names were not encoded")
	}
	if !bytes.Contains(out, []byte("#FORMAT PC8")) {
		t.Error("the file does not declare its encoding")
	}
}

// A character with no place in the code page must be obviously wrong rather
// than quietly something else.
func TestUnmappableCharactersBecomeQuestionMarks(t *testing.T) {
	f := sample()
	f.CompanyName = "工場"
	out, err := Write(f)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !bytes.Contains(out, []byte(`"??"`)) {
		t.Error("an unmappable name did not become question marks")
	}
}

// An accounting package rejects an unbalanced file after the accountant has
// tried to import it. This says which document and why, first.
func TestAnUnbalancedVerificationIsRefused(t *testing.T) {
	f := sample()
	f.Verifications[0].Entries[0].Minor = 1
	var unbalanced ErrUnbalanced
	if _, err := Write(f); !errors.As(err, &unbalanced) {
		t.Fatalf("Write = %v, want ErrUnbalanced", err)
	}
	if unbalanced.Number != "1" {
		t.Errorf("the error names verification %q", unbalanced.Number)
	}
}

func TestTheShapeIsWhatAnImporterExpects(t *testing.T) {
	out, _ := Write(sample())
	text := string(out)

	for _, want := range []string{
		"#FLAGGA 0", "#SIETYP 4", `#FNAMN "`, `#ORGNR "556677-8899"`,
		"#RAR 0 20260101 20261231", `#VER "A" "1" 20260921`,
		"#TRANS 1510 {} 3733.13", "#TRANS 3010 {} -2986.50",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the file is missing %q", want)
		}
	}
	if !strings.Contains(text, "\r\n") {
		t.Error("lines are not CRLF terminated")
	}
	// Accounts in numerical order, so two exports of a period are the same
	// file and a diff means something.
	i1510, i2611, i3010 := strings.Index(text, "#KONTO 1510"), strings.Index(text, "#KONTO 2611"), strings.Index(text, "#KONTO 3010")
	if !(i1510 < i2611 && i2611 < i3010) {
		t.Error("accounts are not in numerical order")
	}
}

// Amounts use a full stop and two decimals whatever Swedish does elsewhere.
func TestAmountFormatting(t *testing.T) {
	for _, c := range []struct {
		minor int64
		want  string
	}{{0, "0.00"}, {5, "0.05"}, {373313, "3733.13"}, {-74663, "-746.63"}, {-5, "-0.05"}} {
		if got := amount(c.minor); got != c.want {
			t.Errorf("amount(%d) = %q, want %q", c.minor, got, c.want)
		}
	}
}

func TestQuotesAreEscaped(t *testing.T) {
	f := sample()
	f.CompanyName = `The "Good" Garage`
	out, _ := Write(f)
	if !bytes.Contains(out, []byte(`\"Good\"`)) {
		t.Error("quotes inside a field were not escaped")
	}
}
