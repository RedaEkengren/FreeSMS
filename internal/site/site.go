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
	"time"
	// The scenes are in Stockholm whether or not the machine generating
	// them has a zone database.
	_ "time/tzdata"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/i18n"
	"github.com/RedaEkengren/FreeSMS/internal/money"
	"github.com/RedaEkengren/FreeSMS/internal/sie"
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

var stockholm = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		panic(err)
	}
	return loc
}()

// sceneInvoice is an invoice as a scene draws it: the money already in the
// reader's format, and the verification as the SIE file carries it.
type sceneInvoice struct {
	Reference       string
	Description     string
	Quantity, Unit  string
	Net, VAT, Gross string
	Verification    []string
}

func invoiceFor(kind, description string, quantityMilli int, unitMinor int64, customer string, number int, issued string) (sceneInvoice, error) {
	day, err := time.ParseInLocation("2006-01-02", issued, stockholm)
	if err != nil {
		return sceneInvoice{}, err
	}
	const rate = 2500
	line := money.Line{QuantityMilli: int64(quantityMilli), UnitPriceMinor: unitMinor, VATRateBasis: rate, ChargedToCustomer: true}
	totals := money.Compute([]money.Line{line})
	purpose, ok := workshop.SalesPurpose(kind)
	if !ok {
		return sceneInvoice{}, fmt.Errorf("site: %q is not a kind of line", kind)
	}

	accounts, names := workshop.DefaultAccounts()
	v, err := workshop.InvoiceVerification(accounts, workshop.VoucherDocument{
		Series: "A", Number: int64(number), Issued: day, Customer: customer,
		NetMinor: totals.NetMinor, VATMinor: totals.VATMinor, GrossMinor: totals.GrossMinor,
		NetByPurpose: map[string]int64{purpose: totals.NetMinor},
		VATByRate:    map[int]int64{rate: totals.VATMinor},
	})
	if err != nil {
		return sceneInvoice{}, err
	}
	file, err := sie.Write(sie.File{
		Program: "FreeSMS", ProgramVer: "1", Generated: day, CompanyName: "Exempel",
		YearFrom: day, YearTo: day, Accounts: names, Verifications: []sie.Verification{v},
	})
	if err != nil {
		return sceneInvoice{}, err
	}
	// The verification, as it is in the file. Only ASCII is shown: the file
	// is CP437, and a name with å in it would need decoding to be honest.
	var ver []string
	inside := false
	for _, l := range strings.Split(string(file), "\r\n") {
		if strings.HasPrefix(l, "#VER") {
			inside = true
		}
		if inside {
			for _, r := range l {
				if r > 127 {
					return sceneInvoice{}, fmt.Errorf("site: the scene's verification is not ASCII: %q", l)
				}
			}
			ver = append(ver, l)
		}
		if inside && l == "}" {
			break
		}
	}

	show := money.DisplayFor(Locale, "SEK")
	return sceneInvoice{
		Reference:    fmt.Sprintf("A-%d", number),
		Description:  description,
		Quantity:     fmt.Sprintf("%g", float64(quantityMilli)/1000),
		Unit:         show.Amount(unitMinor),
		Net:          show.Amount(totals.NetMinor),
		VAT:          show.Amount(totals.VATMinor),
		Gross:        show.Amount(totals.GrossMinor),
		Verification: ver,
	}, nil
}

// sceneTotals is the bottom of an invoice as a scene shows it.
type sceneTotals struct {
	Net, VAT, Gross string
	// Paid in cash, rounded at the counter, and the rounding kept apart.
	Cash, Rounding string
}

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
		// A day in the shop's calendar, "2006-01-02".
		"day": func(v string) (time.Time, error) { return time.ParseInLocation("2006-01-02", v, stockholm) },
		// An amount as the product prints it.
		"money": func(minor int64) string { return money.DisplayFor(Locale, "SEK").Amount(minor) },
		// Numbers and dates as the reader's language writes them.
		"num":  p.Number,
		"date": p.Date,
		// A job's progress at a moment, worked out by the product's own
		// Progress: the verdict, the hours, the bar. Times are the shop's
		// wall clock, "2006-01-02 15:04" in Stockholm.
		"progress": func(estimate, clocked int, running bool, asOf, promised string) (workshop.Progress, error) {
			now, err := time.ParseInLocation("2006-01-02 15:04", asOf, stockholm)
			if err != nil {
				return workshop.Progress{}, err
			}
			at, err := time.ParseInLocation("2006-01-02 15:04", promised, stockholm)
			if err != nil {
				return workshop.Progress{}, err
			}
			return workshop.Progress{EstimateMinutes: estimate, ClockedMinutes: clocked, Running: running, AsOf: now, PromisedAt: &at}, nil
		},
		// What a scene's caption claims, checked against what the product
		// says. If the product stops agreeing, the scene is not generated.
		"expect": func(got, want any) (string, error) {
			if fmt.Sprint(got) != fmt.Sprint(want) {
				return "", fmt.Errorf("site: the scene says %v, the product says %v", want, got)
			}
			return "", nil
		},
		// An invoice of one line, totalled by the product's money code and
		// booked by its accounting code, as the scene shows it.
		"invoice": func(kind, description string, quantityMilli int, unitMinor int64, customer string, number int, issued string) (sceneInvoice, error) {
			return invoiceFor(kind, description, quantityMilli, unitMinor, customer, number, issued)
		},
		// Whether a role is given a customer's personal data, as the
		// product decides it where the data is read.
		"sees": func(role string) bool { return access.Role(role).SeesCustomerPersonalData() },
		// The languages that shipped, named in themselves.
		"languages": workshop.Languages,
		// An invoice's bottom lines, from (quantity in thousandths, unit price
		// in öre) pairs at 25 per cent, totalled and cash-rounded by the
		// product's money code.
		"totals": func(pairs ...int64) (sceneTotals, error) {
			if len(pairs)%2 != 0 {
				return sceneTotals{}, fmt.Errorf("site: totals takes quantity and price pairs")
			}
			var lines []money.Line
			for i := 0; i < len(pairs); i += 2 {
				lines = append(lines, money.Line{QuantityMilli: pairs[i], UnitPriceMinor: pairs[i+1],
					VATRateBasis: 2500, ChargedToCustomer: true})
			}
			t := money.Compute(lines)
			show := money.DisplayFor(Locale, "SEK")
			cash := money.CashRound(t.GrossMinor)
			return sceneTotals{Net: show.Amount(t.NetMinor), VAT: show.Amount(t.VATMinor),
				Gross: show.Amount(t.GrossMinor), Cash: show.Amount(cash),
				Rounding: show.Amount(t.GrossMinor - cash)}, nil
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
