package sie

import (
	"bytes"
	"fmt"
	"time"
)

// Amounts are written with a full stop and two decimals, which is what the
// format specifies regardless of how Swedish writes numbers elsewhere.
func amount(minor int64) string {
	sign := ""
	if minor < 0 {
		sign, minor = "-", -minor
	}
	return fmt.Sprintf("%s%d.%02d", sign, minor/100, minor%100)
}

// Entry is one line of a verification: an account and a signed amount.
type Entry struct {
	Account int
	Minor   int64
}

// Verification is one document as the books see it.
type Verification struct {
	Series  string
	Number  string
	Date    time.Time
	Text    string
	Entries []Entry
}

// Balanced reports whether the entries sum to nothing, which every
// verification must.
func (v Verification) Balanced() bool {
	var sum int64
	for _, e := range v.Entries {
		sum += e.Minor
	}
	return sum == 0
}

// File is everything an export needs to know.
type File struct {
	Program       string
	ProgramVer    string
	Generated     time.Time
	CompanyName   string
	OrgNumber     string
	YearFrom      time.Time
	YearTo        time.Time
	Accounts      map[int]string
	Verifications []Verification
}

// ErrUnbalanced names a verification that does not sum to nothing.
type ErrUnbalanced struct{ Series, Number string }

func (e ErrUnbalanced) Error() string {
	return fmt.Sprintf("sie: verification %s %s does not balance", e.Series, e.Number)
}

// Write renders the file, in CP437.
//
// Every verification is checked for balance first. An accounting package will
// reject an unbalanced file, but it will do so after the accountant has tried
// to import it; failing here says which document and why.
func Write(f File) ([]byte, error) {
	for _, v := range f.Verifications {
		if !v.Balanced() {
			return nil, ErrUnbalanced{Series: v.Series, Number: v.Number}
		}
	}

	var b bytes.Buffer
	line := func(format string, args ...any) {
		b.Write(encode(fmt.Sprintf(format, args...)))
		// CRLF, because the format is old and the tools that read it are too.
		b.WriteString("\r\n")
	}

	line("#FLAGGA 0")
	line("#PROGRAM %s %s", quote(f.Program), quote(f.ProgramVer))
	// Declared, and true. A file that says PC8 and contains UTF-8 is worse
	// than one that says nothing.
	line("#FORMAT PC8")
	line("#SIETYP 4")
	line("#GEN %s", f.Generated.Format("20060102"))
	line("#FNAMN %s", quote(f.CompanyName))
	if f.OrgNumber != "" {
		line("#ORGNR %s", quote(f.OrgNumber))
	}
	line("#RAR 0 %s %s", f.YearFrom.Format("20060102"), f.YearTo.Format("20060102"))

	// Accounts in numerical order, so two exports of the same period are the
	// same file and a diff means something.
	for _, account := range sortedAccounts(f.Accounts) {
		line("#KONTO %d %s", account, quote(f.Accounts[account]))
	}

	for _, v := range f.Verifications {
		line("#VER %s %s %s %s", quote(v.Series), quote(v.Number),
			v.Date.Format("20060102"), quote(v.Text))
		line("{")
		for _, e := range v.Entries {
			line("#TRANS %d {} %s", e.Account, amount(e.Minor))
		}
		line("}")
	}
	return b.Bytes(), nil
}

func sortedAccounts(m map[int]string) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
