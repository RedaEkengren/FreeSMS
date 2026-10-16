package workshop

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/money"
)

// Getting ready for the first customer.
//
// The checklist is a reading of the workshop as it stands, not a wizard with
// state of its own. The address is on the page or it is not; the technician
// exists or does not. Only what cannot be read -- that the owner has looked at
// the invoice, who owns the backups, that "no final check" is a decision -- is
// something somebody said, and that is a row (setup_steps), so the list can be
// put down and picked up another day.
//
// Required is a matter of how the workshop works. A one-person shop does the
// work, the counter and the owner's part itself, and is not told to add staff
// or plan a rota; a provider for messages or number plate lookup is never
// required, and the manual way is said where the provider is not configured.
//
// Nothing here blocks an ordinary task. A task not yet done says what is
// missing and where to put it right; the job page, the invoice and the queue
// go on working. Nothing makes a test invoice: the invoice preview is drawn
// from the workshop's details with an example line and writes nothing.

// ReadinessEnv is what the server knows about its own configuration.
type ReadinessEnv struct {
	Email, SMS, Lookup bool
}

// ReadinessTask is one thing to do, or to decide, before the first customer.
type ReadinessTask struct {
	Key   string
	Group string
	// Catalogue keys.
	Title string
	Why   string
	Link  string
	// Required for how this workshop works; an optional task is never in the way
	// of being ready.
	Required bool
	// "done", "todo", "decided" (somebody said it is not wanted), "manual" (an
	// optional provider is not configured and the manual way is the way).
	State string
	// What was found, as catalogue keys with at most one argument.
	Detail []string
	// Whether the owner can say it is done or not wanted, with what label, and
	// whether a note is wanted with it.
	CanSay   bool
	SayLabel string
	CanSkip  bool
	SkipText string
	Note     string
	NeedNote bool
	By       string
	At       *time.Time
}

// Satisfied is whether the task stands in the way of nothing.
func (t ReadinessTask) Satisfied() bool {
	return t.State == "done" || t.State == "decided" || t.State == "manual" || !t.Required
}

// Readiness is where the workshop stands.
type Readiness struct {
	Solo    bool
	Tasks   []ReadinessTask
	Ready   bool
	Missing int
	Done    int
	Total   int
}

// ReadinessGroups are the groups, in order, as catalogue keys.
var ReadinessGroups = []string{"The business", "People and time", "Money", "Customers and data", "Before the first customer"}

var setupSteps = map[string]bool{
	"operating_model": true, "locale": true, "vat": true, "final_check": true, "accountant": true, "invoice": true,
	"backups": true, "support": true, "data": true, "messaging": true, "lookup": true,
}

// ReadinessFor reads where the workshop stands.
func ReadinessFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, env ReadinessEnv) (Readiness, error) {
	if !scope.Role.RunsTheShop() {
		return Readiness{}, access.ErrForbidden
	}
	var s Readiness
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		s, err = setupTx(ctx, tx, scope, env)
		return err
	})
	return s, err
}

type ackRow struct {
	state, note, by string
	at              time.Time
}

func setupTx(ctx context.Context, tx pgx.Tx, scope access.Scope, env ReadinessEnv) (Readiness, error) {
	var d struct {
		name, line1, postal, city, org, vat, pay, locale, tz string
		terms                                                int
		lifts                                                *int
	}
	if err := tx.QueryRow(ctx, `
		SELECT name, coalesce(address_line1, ''), coalesce(postal_code, ''), coalesce(city, ''), coalesce(org_number, ''),
		       coalesce(vat_number, ''), coalesce(payment_reference, ''), locale, timezone, payment_terms_days, lifts
		FROM shops WHERE id = $1`, scope.ShopID).Scan(&d.name, &d.line1, &d.postal, &d.city, &d.org, &d.vat, &d.pay, &d.locale, &d.tz, &d.terms, &d.lifts); err != nil {
		return Readiness{}, fmt.Errorf("read the shop: %w", err)
	}
	count := func(q string, args ...any) (int, error) {
		var n int
		if err := tx.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			return 0, fmt.Errorf("count: %w", err)
		}
		return n, nil
	}
	var technicians, others, rotas, labour, bands, templates, parts, imports, customAccounts, finalTemplate int
	var err error
	for _, c := range []struct {
		into *int
		q    string
	}{
		{&technicians, `SELECT count(*) FROM users WHERE role = 'technician' AND active`},
		{&others, `SELECT count(*) FROM users WHERE role IN ('service_advisor', 'parts') AND active`},
		{&rotas, `SELECT count(DISTINCT user_id) FROM rotas`},
		{&labour, `SELECT count(*) FROM labour_times`},
		{&bands, `SELECT count(*) FROM price_bands`},
		{&templates, `SELECT count(*) FROM inspection_templates WHERE active`},
		{&parts, `SELECT count(*) FROM parts WHERE active`},
		{&imports, `SELECT count(*) FROM import_batches WHERE status = 'applied'`},
		{&customAccounts, `SELECT count(*) FROM ledger_accounts`},
		{&finalTemplate, `SELECT count(*) FROM shops WHERE id = $1 AND final_check_template_id IS NOT NULL`},
	} {
		if strings.Contains(c.q, "$1") {
			*c.into, err = count(c.q, scope.ShopID)
		} else {
			*c.into, err = count(c.q)
		}
		if err != nil {
			return Readiness{}, err
		}
	}

	acks := map[string]ackRow{}
	rows, err := tx.Query(ctx, `
		SELECT s.step, s.state, coalesce(s.note, ''), coalesce(p.display_name, ''), s.at
		FROM setup_steps s LEFT JOIN users u ON u.id = s.by_user LEFT JOIN people p ON p.id = u.person_id`)
	if err != nil {
		return Readiness{}, fmt.Errorf("read the steps: %w", err)
	}
	for rows.Next() {
		var step string
		var a ackRow
		if err := rows.Scan(&step, &a.state, &a.note, &a.by, &a.at); err != nil {
			rows.Close()
			return Readiness{}, fmt.Errorf("scan step: %w", err)
		}
		acks[step] = a
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Readiness{}, err
	}

	solo := acks["operating_model"].note == "solo"
	var tasks []ReadinessTask
	add := func(t ReadinessTask) {
		if a, ok := acks[t.Key]; ok && t.State == "" {
			t.Note, t.By = a.note, a.by
			at := a.at
			t.At = &at
			t.State = "done"
			if a.state == "skipped" {
				t.State = "decided"
			}
		}
		if t.State == "" {
			t.State = "todo"
		}
		tasks = append(tasks, t)
	}
	missing := func(pairs ...string) []string {
		var out []string
		for i := 0; i+1 < len(pairs); i += 2 {
			if strings.TrimSpace(pairs[i]) == "" {
				out = append(out, pairs[i+1])
			}
		}
		return out
	}

	// The business.
	lack := missing(d.line1, "address", d.postal, "postal code", d.city, "city", d.org, "company number")
	seller := ReadinessTask{Key: "seller", Group: "The business", Title: "Your company details", Required: true, Link: "/shop",
		Why: "They are printed at the top of every invoice. An invoice without a company number and an address is not one a customer or the tax office can use."}
	if len(lack) == 0 {
		seller.State = "done"
		seller.Detail = []string{d.name}
	} else {
		seller.State, seller.Detail = "todo", lack
	}
	add(seller)

	pay := ReadinessTask{Key: "payment", Group: "The business", Title: "Where customers pay", Required: true, Link: "/shop",
		Why: "The bankgiro, plusgiro or account an invoice tells the customer to pay to, and how many days they have."}
	if strings.TrimSpace(d.pay) != "" {
		pay.State, pay.Detail = "done", []string{d.pay}
	} else {
		pay.State, pay.Detail = "todo", []string{"payment details"}
	}
	add(pay)

	vat := ReadinessTask{Key: "vat", Group: "The business", Title: "VAT registration", Required: true, Link: "/shop",
		Why:     "A VAT number is printed on an invoice. A workshop that is not registered says so instead, and the invoice leaves it out.",
		CanSkip: true, SkipText: "We are not registered for VAT", NeedNote: false}
	if strings.TrimSpace(d.vat) != "" {
		vat.State, vat.Detail = "done", []string{d.vat}
	}
	add(vat)

	add(ReadinessTask{Key: "locale", Group: "The business", Title: "Language and time zone", Required: true, Link: "/shop",
		Why:    "The language every page and the words on an invoice come in until somebody picks their own, and the clock the day starts and ends on.",
		Detail: []string{"language|" + d.locale, "time zone|" + d.tz}, CanSay: true, SayLabel: "These are right"})

	// People and time.
	staff := ReadinessTask{Key: "staff", Group: "People and time", Title: "Staff", Required: !solo, Link: "/staff",
		Why: "A technician sees the cars and the work and never a customer's personal details. A one-person workshop does it all as the owner."}
	switch {
	case technicians > 0:
		staff.State, staff.Detail = "done", []string{fmt.Sprintf("technicians|%d", technicians), fmt.Sprintf("counter and parts desk|%d", others)}
	case solo:
		staff.State, staff.Detail = "decided", []string{"a one-person workshop"}
	default:
		staff.State, staff.Detail = "todo", []string{"no technician yet"}
	}
	add(staff)

	cap := ReadinessTask{Key: "capacity", Group: "People and time", Title: "Opening hours and lifts", Required: false, Link: "/rota",
		Why: "Working hours per person and the number of lifts let the planning view show what fits. Without them everything still works; the calendar just cannot warn about a clash."}
	if rotas > 0 || d.lifts != nil {
		cap.State = "done"
		if d.lifts != nil {
			cap.Detail = append(cap.Detail, fmt.Sprintf("lifts|%d", *d.lifts))
		}
		if rotas > 0 {
			cap.Detail = append(cap.Detail, fmt.Sprintf("people with working hours|%d", rotas))
		}
	}
	add(cap)

	// Money.
	add(ReadinessTask{Key: "accounts", Group: "Money", Title: "Accounts for the accountant", Required: false, Link: "/accounting",
		Why:   "Sales, VAT and receivables are booked to the standard Swedish accounts unless you change them. Your accountant can change them in the import.",
		State: "done", Detail: []string{fmt.Sprintf("accounts of your own|%d", customAccounts)}})
	rates := ReadinessTask{Key: "rates", Group: "Money", Title: "Labour times and price bands", Required: false, Link: "/labour",
		Why: "A time guide gives a job its labour hours, and price bands turn a part's cost into a price. Without them every price is typed."}
	if labour > 0 || bands > 0 {
		rates.State, rates.Detail = "done", []string{fmt.Sprintf("labour times|%d", labour), fmt.Sprintf("price bands|%d", bands)}
	}
	add(rates)
	add(ReadinessTask{Key: "accountant", Group: "Money", Title: "The accountant can read the export", Required: true, Link: "/accounting",
		Why:    "FreeSMS writes a SIE 4B file. Hand your accountant a real one and have them tell you it imported before the first month ends; this cannot be checked from here.",
		CanSay: true, SayLabel: "My accountant has read an export", NeedNote: true})
	add(ReadinessTask{Key: "invoice", Group: "Money", Title: "Look at the invoice", Required: true, Link: "/start/invoice",
		Why:    "See what a customer will receive, drawn from your details with an example line. Nothing is stored and no invoice number is used.",
		CanSay: true, SayLabel: "The invoice looks right"})

	// Customers and data.
	data := ReadinessTask{Key: "data", Group: "Customers and data", Title: "Your customers, cars and stock", Required: true, Link: "/import",
		Why:     "Bring what you already have, or start clean. Either is fine, and it is a choice, not an oversight.",
		CanSkip: true, SkipText: "Start clean"}
	if imports > 0 {
		data.State, data.Detail = "done", []string{fmt.Sprintf("files imported|%d", imports)}
	}
	add(data)
	cat := ReadinessTask{Key: "catalogue", Group: "Customers and data", Title: "Checklists and parts", Required: false, Link: "/checklists",
		Why: "Inspection checklists a technician works through, and the parts you stock. A workshop can begin without either."}
	if templates > 0 || parts > 0 {
		cat.State, cat.Detail = "done", []string{fmt.Sprintf("checklists|%d", templates), fmt.Sprintf("parts|%d", parts)}
	}
	add(cat)

	// Before the first customer.
	fc := ReadinessTask{Key: "final_check", Group: "Before the first customer", Title: "Final check before a car is ready", Required: true, Link: "/shop",
		Why:     "Whether a car can be called ready without somebody checking the work. Choose a checklist, or decide that you do not use one.",
		CanSkip: true, SkipText: "We do not use a final check"}
	if finalTemplate > 0 {
		fc.State, fc.Detail = "done", []string{"a checklist signs the work off"}
	}
	add(fc)

	msg := ReadinessTask{Key: "messaging", Group: "Before the first customer", Title: "Texts and email to customers", Required: false, Link: "/start",
		Why: "If a provider is set up, FreeSMS can send the link to a job, a booking confirmation and an invoice. Without one you send them yourself and write down that you did."}
	switch {
	case env.Email || env.SMS:
		msg.State = "done"
		if env.Email {
			msg.Detail = append(msg.Detail, "email is set up")
		}
		if env.SMS {
			msg.Detail = append(msg.Detail, "text messages are set up")
		}
	default:
		msg.State, msg.Detail = "manual", []string{"No provider: send it from your own phone or email and write down that you did."}
	}
	add(msg)
	look := ReadinessTask{Key: "lookup", Group: "Before the first customer", Title: "Looking up a registration number", Required: false, Link: "/start",
		Why: "A vehicle register agreement fills in make, model and year. It is the workshop's to hold, so nothing is included."}
	if env.Lookup {
		look.State, look.Detail = "done", []string{"a lookup service is set up"}
	} else {
		look.State, look.Detail = "manual", []string{"No lookup: type the make and model when a car is taken in."}
	}
	add(look)
	add(ReadinessTask{Key: "backups", Group: "Before the first customer", Title: "Who owns the backups", Required: true, Link: "/start/help/owner",
		Why:    "A workshop cannot lose a day of invoices. Somebody has to own the backups and have tried getting them back. Write down who, and where the runbook is.",
		CanSay: true, SayLabel: "This person owns the backups", NeedNote: true})
	add(ReadinessTask{Key: "support", Group: "Before the first customer", Title: "Who to ring when it breaks", Required: true, Link: "/start/help/owner",
		Why:    "A name and a number, written down before the day you need them.",
		CanSay: true, SayLabel: "This is who we ring", NeedNote: true})

	set := Readiness{Solo: solo, Tasks: tasks}
	for _, t := range tasks {
		if t.Required {
			set.Total++
			if t.Satisfied() {
				set.Done++
			} else {
				set.Missing++
			}
		}
	}
	set.Ready = set.Missing == 0
	return set, nil
}

// ReadinessStatus says something has been looked at, decided, or taken back.
//
// state is "done", "skipped", or "reset" to take it back. A note is required
// where the task is about a person or an agreement, since "done" without whom
// proves nothing; the operating model is "solo" or "team".
func ReadinessStatus(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, step, state, note string) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	note = strings.TrimSpace(note)
	switch {
	case !setupSteps[step]:
		return fmt.Errorf("%w: that is not a step", ErrInvalid)
	case state != "done" && state != "skipped" && state != "reset":
		return fmt.Errorf("%w: a step is done, skipped, or taken back", ErrInvalid)
	case len([]rune(note)) > 300:
		return fmt.Errorf("%w: the note is limited to 300 characters", ErrInvalid)
	case step == "operating_model" && state != "reset" && note != "solo" && note != "team":
		return fmt.Errorf("%w: a workshop is solo or has a team", ErrInvalid)
	case state != "reset" && note == "" && (step == "accountant" || step == "backups" || step == "support"):
		return fmt.Errorf("%w: write down who", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if state == "reset" {
			_, err := tx.Exec(ctx, `DELETE FROM setup_steps WHERE step = $1`, step)
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO setup_steps (shop_id, step, state, note, by_user) VALUES ($1, $2, $3, nullif($4, ''), $5)
			ON CONFLICT (shop_id, step) DO UPDATE SET state = EXCLUDED.state, note = EXCLUDED.note, by_user = EXCLUDED.by_user, at = clock_timestamp()`,
			scope.ShopID, step, state, note, scope.UserID)
		if err != nil {
			return fmt.Errorf("record the step: %w", err)
		}
		return nil
	})
}

// InvoiceExample is an invoice drawn from the workshop's own details with one
// example line, for the owner to look at before the first real one. It is
// built in memory: no number is taken, nothing is written, and it carries a
// series that no real invoice can have.
func InvoiceExample(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) (Document, []string, error) {
	if !scope.Role.RunsTheShop() {
		return Document{}, nil, access.ErrForbidden
	}
	var seller Seller
	var currency string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if seller, err = readSeller(ctx, tx, scope.ShopID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT trim(currency) FROM shops WHERE id = $1`, scope.ShopID).Scan(&currency)
	})
	if err != nil {
		return Document{}, nil, err
	}
	var lacks []string
	for _, f := range [][2]string{{seller.Address, "address"}, {seller.OrgNumber, "company number"}, {seller.PaymentReference, "payment details"}} {
		if strings.TrimSpace(f[0]) == "" {
			lacks = append(lacks, f[1])
		}
	}
	d := Document{Seller: seller, Currency: currency}
	d.Series, d.Number = "EXAMPLE", 0
	d.CustomerName = "An Example Customer"
	d.CustomerAddress = "Exempelgatan 1, 411 00 Göteborg"
	d.Registration = "ABC 123"
	d.IssuedAt = time.Now()
	d.Lines = []DocumentLine{
		{Position: 1, Kind: "labour", Description: "Labour, one hour", QuantityMilli: 1000, UnitPriceMinor: 80000, VATRateBasis: 2500, NetMinor: 80000, VATMinor: 20000},
		{Position: 2, Kind: "part", Description: "An example part", QuantityMilli: 2000, UnitPriceMinor: 10000, VATRateBasis: 2500, NetMinor: 20000, VATMinor: 5000},
	}
	d.NetMinor, d.VATMinor, d.GrossMinor = 100000, 25000, 125000
	d.Bands = []money.VATBand{{RateBasisPoints: 2500, NetMinor: 100000, VATMinor: 25000}}
	return d, lacks, nil
}

// Fact is one thing found: a catalogue key and, usually, a value.
type Fact struct{ Key, Arg string }

// Facts splits what was found into key and value.
func (t ReadinessTask) Facts() []Fact {
	var out []Fact
	for _, d := range t.Detail {
		k, a, _ := strings.Cut(d, "|")
		out = append(out, Fact{k, a})
	}
	return out
}

// SayText is what the button that says it is done says.
func (t ReadinessTask) SayText() string {
	if t.SayLabel != "" {
		return t.SayLabel
	}
	return "Done"
}

// SellerGaps are what a document carries no seller details for: the things a
// Swedish invoice needs from its seller, left blank when it was issued. Said
// to the counter, never filled in afterwards -- an issued document is
// immutable, and a wrong one is credited and made again. A document issued
// before the seller was snapshotted has none and is not flagged.
func (d Document) SellerGaps() []string {
	if !d.Seller.Known() || d.IsCreditNote() {
		return nil
	}
	var out []string
	for _, f := range [][2]string{{d.Seller.Address, "address"}, {d.Seller.OrgNumber, "company number"}, {d.Seller.PaymentReference, "payment details"}} {
		if strings.TrimSpace(f[0]) == "" {
			out = append(out, f[1])
		}
	}
	return out
}
