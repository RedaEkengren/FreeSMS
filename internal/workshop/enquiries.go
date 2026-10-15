package workshop

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/money"
)

// An enquiry is a customer asking what something would cost, before there is
// a car in the workshop. What is known is kept and the rest is left open: a
// registration nobody has yet, a payer nobody has named. The proposal made on
// it is a quote -- the same numbered, frozen version as on a job -- and the
// answer to it is the same append-only decision. Sending a proposal, or
// having it accepted, says nothing about the car being here, work being done,
// parts being taken out or anything being owed: only intake of a booking
// makes a job, and the job's lines are copied from what was accepted.

// ErrQuoteExpired is an answer to a proposal that stopped standing before it
// was given. The workshop puts a new version forward; nothing is guessed.
var ErrQuoteExpired = errors.New("workshop: that proposal is no longer valid; ask the workshop for a new one")

// NewEnquiry is what is known when somebody asks.
type NewEnquiry struct {
	Subject       string
	Registration  string
	Vehicle       string
	CustomerName  string
	CustomerPhone string
}

// Terms are what the workshop promises about a proposal.
type Terms struct {
	// How many days the price stands.
	ValidDays int
	// "fixed" is a price the workshop keeps; "estimate" says it may move,
	// and that moving it is another decision.
	PriceKind string
	// What a diagnosis may and may not find before the customer is asked
	// again. Free text, the workshop's own words.
	Limits string
}

// EnquiryQuote is one version of the proposal.
type EnquiryQuote struct {
	Quote
	ValidUntil time.Time
	PriceKind  string
	Limits     string
	Expired    bool
	// Replaced by a newer version.
	Superseded bool
}

// EnquiryAnswer is one answer to one version.
type EnquiryAnswer struct {
	Version           int
	Decision          string
	Via               string
	RecordedBy        string
	AgreedGrossMinor  int64
	DeclinedPositions []int
	Note              string
	DecidedAt         time.Time
}

// Partly is a yes to some of the lines.
func (a EnquiryAnswer) Partly() bool { return a.Decision == "approved" && len(a.DeclinedPositions) > 0 }

// Enquiry is the whole of one: what was asked, the draft, every version and
// every answer.
type Enquiry struct {
	ID            string
	Subject       string
	Registration  string
	Vehicle       string
	CustomerName  string
	CustomerPhone string
	CreatedAt     time.Time
	// The workshop's particulars, for a proposal laid out to print.
	ShopName, ShopPhone, ShopAddress string

	Draft    []QuoteLine // what would be put forward now
	Versions []EnquiryQuote
	Answers  []EnquiryAnswer
	// The booking made from it, if one is standing.
	BookingID string
}

// Latest is the newest version, or nil.
func (e Enquiry) Latest() *EnquiryQuote {
	if len(e.Versions) == 0 {
		return nil
	}
	return &e.Versions[0]
}

// Answer is the latest answer to the latest version, or nil.
func (e Enquiry) Answer() *EnquiryAnswer {
	l := e.Latest()
	if l == nil {
		return nil
	}
	for i := len(e.Answers) - 1; i >= 0; i-- {
		if e.Answers[i].Version == l.Version {
			return &e.Answers[i]
		}
	}
	return nil
}

// Status is where the enquiry stands, as a catalogue key. Derived from the
// versions, the answers and the booking, and stored nowhere.
func (e Enquiry) Status() string {
	var d *EnquiryAnswer
	l := e.Latest()
	if l != nil {
		d = e.Answer()
	}
	return enquiryStatus(l != nil, l != nil && l.Expired, d, e.BookingID != "")
}

func enquiryStatus(sent, expired bool, a *EnquiryAnswer, booked bool) string {
	switch {
	case booked:
		return "Booked"
	case !sent:
		return "Draft"
	case a != nil && a.Decision == "declined":
		return "Declined"
	case a != nil && a.Partly():
		return "Partly accepted"
	case a != nil:
		return "Accepted"
	case expired:
		return "Expired"
	}
	return "Sent"
}

// EnquiryRow is a line in the list of enquiries.
type EnquiryRow struct {
	ID            string
	Subject       string
	Registration  string
	CustomerName  string
	CreatedAt     time.Time
	Status        string
	LatestVersion int
}

func validKind(k string) bool {
	switch k {
	case "labour", "part", "sublet", "fee":
		return true
	}
	return false
}

// CreateEnquiry records an enquiry. Everything but what was asked is optional.
func CreateEnquiry(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, n NewEnquiry) (string, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return "", access.ErrForbidden
	}
	if strings.TrimSpace(n.Subject) == "" {
		return "", fmt.Errorf("%w: say what the customer is asking about", ErrInvalid)
	}
	var id string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO enquiries (shop_id, subject, registration, vehicle_text, customer_name, customer_phone, created_by)
			VALUES ($1, btrim($2), nullif(upper(btrim($3)), ''), nullif(btrim($4), ''), nullif(btrim($5), ''), nullif(btrim($6), ''), $7)
			RETURNING id`, scope.ShopID, n.Subject, n.Registration, n.Vehicle, n.CustomerName, n.CustomerPhone, scope.UserID).Scan(&id)
	})
	return id, err
}

// AddEnquiryLine puts a line on the draft.
func AddEnquiryLine(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, enquiryID string, l NewLine) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	switch {
	case !validKind(l.Kind):
		return fmt.Errorf("%w: %q is not a kind of line", ErrInvalid, l.Kind)
	case strings.TrimSpace(l.Description) == "":
		return fmt.Errorf("%w: say what the line is", ErrInvalid)
	case l.QuantityMilli <= 0:
		return fmt.Errorf("%w: the quantity is more than nothing", ErrInvalid)
	case l.UnitPriceMinor < 0:
		return fmt.Errorf("%w: a price is not negative", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockEnquiryTx(ctx, tx, enquiryID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO enquiry_lines (shop_id, enquiry_id, position, kind, description, quantity, unit_price_minor, vat_rate_bp)
			SELECT $1, $2, coalesce(max(position), 0) + 1, $3, btrim($4), ($5::bigint)::numeric / 1000, $6, $7
			FROM enquiry_lines WHERE enquiry_id = $2`,
			scope.ShopID, enquiryID, l.Kind, l.Description, l.QuantityMilli, l.UnitPriceMinor, l.VATRateBasis)
		if err != nil {
			return fmt.Errorf("add enquiry line: %w", err)
		}
		return nil
	})
}

// RemoveEnquiryLine takes a line off the draft. The line stays, removed: a
// version already put forward is not touched, and neither is the record.
func RemoveEnquiryLine(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, enquiryID string, position int) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockEnquiryTx(ctx, tx, enquiryID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE enquiry_lines SET removed_at = now()
			WHERE enquiry_id = $1 AND position = $2 AND removed_at IS NULL`, enquiryID, position)
		if err != nil {
			return fmt.Errorf("remove enquiry line: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

func lockEnquiryTx(ctx context.Context, tx pgx.Tx, id string) error {
	if !looksLikeUUID(id) {
		return ErrNotFound
	}
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT true FROM enquiries WHERE id = $1 FOR UPDATE`, id).Scan(&ok); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("lock enquiry: %w", err)
	}
	return nil
}

func draftLinesTx(ctx context.Context, tx pgx.Tx, id string) ([]QuoteLine, error) {
	rows, err := tx.Query(ctx, `
		SELECT position, kind, description, (quantity * 1000)::bigint, unit_price_minor, vat_rate_bp
		FROM enquiry_lines WHERE enquiry_id = $1 AND removed_at IS NULL ORDER BY position`, id)
	if err != nil {
		return nil, fmt.Errorf("read the draft: %w", err)
	}
	defer rows.Close()
	var out []QuoteLine
	for rows.Next() {
		var l QuoteLine
		if err := rows.Scan(&l.Position, &l.Kind, &l.Description, &l.QuantityMilli, &l.UnitPriceMinor, &l.VATRateBasis); err != nil {
			return nil, fmt.Errorf("scan a draft line: %w", err)
		}
		l.NetMinor = money.LineNet(l.QuantityMilli, l.UnitPriceMinor)
		l.VATMinor = money.VAT(l.NetMinor, l.VATRateBasis)
		out = append(out, l)
	}
	return out, rows.Err()
}

func sameLines(a, b []QuoteLine) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.Kind != y.Kind || x.Description != y.Description || x.QuantityMilli != y.QuantityMilli ||
			x.UnitPriceMinor != y.UnitPriceMinor || x.VATRateBasis != y.VATRateBasis {
			return false
		}
	}
	return true
}

func grossOf(lines []QuoteLine) money.Totals {
	ml := make([]money.Line, 0, len(lines))
	for _, l := range lines {
		ml = append(ml, money.Line{QuantityMilli: l.QuantityMilli, UnitPriceMinor: l.UnitPriceMinor,
			VATRateBasis: l.VATRateBasis, ChargedToCustomer: true})
	}
	return money.Compute(ml)
}

// PresentEnquiry freezes the draft as the next numbered version, with the
// terms it stands on, and returns it. The same draft and terms put forward
// again, while the last version is unanswered and valid, is that version: a
// double click does not number twice. A new version replaces the old for the
// customer's link; the old stays, with its answer if it had one.
func PresentEnquiry(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, enquiryID string, t Terms) (EnquiryQuote, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return EnquiryQuote{}, access.ErrForbidden
	}
	if t.ValidDays == 0 {
		t.ValidDays = 14
	}
	t.Limits = strings.TrimSpace(t.Limits)
	switch {
	case t.ValidDays < 1 || t.ValidDays > 90:
		return EnquiryQuote{}, fmt.Errorf("%w: a proposal stands for between 1 and 90 days", ErrInvalid)
	case t.PriceKind != "fixed" && t.PriceKind != "estimate":
		return EnquiryQuote{}, fmt.Errorf("%w: say whether the price is fixed or an estimate", ErrInvalid)
	}
	var out EnquiryQuote
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockEnquiryTx(ctx, tx, enquiryID); err != nil {
			return err
		}
		var booked bool
		if err := tx.QueryRow(ctx, `SELECT exists(SELECT 1 FROM bookings WHERE enquiry_id = $1 AND status IN ('booked', 'arrived'))`,
			enquiryID).Scan(&booked); err != nil {
			return fmt.Errorf("check booking: %w", err)
		}
		if booked {
			return fmt.Errorf("%w: this enquiry is already booked; more work goes on the job and is agreed there", ErrInvalid)
		}
		lines, err := draftLinesTx(ctx, tx, enquiryID)
		if err != nil {
			return err
		}
		if len(lines) == 0 {
			return fmt.Errorf("%w: there is nothing on the draft to put to the customer", ErrInvalid)
		}
		versions, answers, err := quotesOfEnquiryTx(ctx, tx, enquiryID)
		if err != nil {
			return err
		}
		if len(versions) > 0 {
			l := versions[0]
			answered := slices.ContainsFunc(answers, func(a EnquiryAnswer) bool { return a.Version == l.Version })
			quoted := make([]QuoteLine, len(l.Lines))
			copy(quoted, l.Lines)
			if !answered && !l.Expired && l.PriceKind == t.PriceKind && l.Limits == t.Limits && sameLines(quoted, lines) {
				out = l
				return nil
			}
		}
		version := 1
		if len(versions) > 0 {
			version = versions[0].Version + 1
		}
		totals := grossOf(lines)
		var id string
		if err := tx.QueryRow(ctx, `
			INSERT INTO quotes (shop_id, enquiry_id, version, net_minor, vat_minor, gross_minor, presented_by,
			                    valid_until, price_kind, limits)
			VALUES ($1, $2, $3, $4, $5, $6, $7, now() + make_interval(days => $8), $9, nullif($10, ''))
			RETURNING id`,
			scope.ShopID, enquiryID, version, totals.NetMinor, totals.VATMinor, totals.GrossMinor, scope.UserID,
			t.ValidDays, t.PriceKind, t.Limits).Scan(&id); err != nil {
			return fmt.Errorf("write the proposal: %w", err)
		}
		for i, l := range lines {
			if _, err := tx.Exec(ctx, `
				INSERT INTO quote_lines (shop_id, quote_id, position, kind, description, quantity,
				       unit_price_minor, vat_rate_bp, net_minor, vat_minor)
				VALUES ($1, $2, $3, $4, $5, ($6::bigint)::numeric / 1000, $7, $8, $9, $10)`,
				scope.ShopID, id, i+1, l.Kind, l.Description, l.QuantityMilli, l.UnitPriceMinor,
				l.VATRateBasis, l.NetMinor, l.VATMinor); err != nil {
				return fmt.Errorf("write a proposal line: %w", err)
			}
		}
		versions, _, err = quotesOfEnquiryTx(ctx, tx, enquiryID)
		if err != nil {
			return err
		}
		out = versions[0]
		return nil
	})
	return out, err
}

// quotesOfEnquiryTx reads every version, newest first, with its lines, and
// every answer, oldest first.
func quotesOfEnquiryTx(ctx context.Context, tx pgx.Tx, enquiryID string) ([]EnquiryQuote, []EnquiryAnswer, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, version, net_minor, vat_minor, gross_minor, presented_at, valid_until, price_kind,
		       coalesce(limits, ''), valid_until < now()
		FROM quotes WHERE enquiry_id = $1 ORDER BY version DESC`, enquiryID)
	if err != nil {
		return nil, nil, fmt.Errorf("read the proposals: %w", err)
	}
	var versions []EnquiryQuote
	for rows.Next() {
		var q EnquiryQuote
		if err := rows.Scan(&q.ID, &q.Version, &q.NetMinor, &q.VATMinor, &q.GrossMinor, &q.PresentedAt,
			&q.ValidUntil, &q.PriceKind, &q.Limits, &q.Expired); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("scan a proposal: %w", err)
		}
		versions = append(versions, q)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	for i := range versions {
		versions[i].Superseded = i > 0
		lr, err := tx.Query(ctx, `
			SELECT position, kind, description, (quantity * 1000)::bigint, unit_price_minor, vat_rate_bp, net_minor, vat_minor
			FROM quote_lines WHERE quote_id = $1 ORDER BY position`, versions[i].ID)
		if err != nil {
			return nil, nil, fmt.Errorf("read a proposal's lines: %w", err)
		}
		for lr.Next() {
			var l QuoteLine
			if err := lr.Scan(&l.Position, &l.Kind, &l.Description, &l.QuantityMilli, &l.UnitPriceMinor,
				&l.VATRateBasis, &l.NetMinor, &l.VATMinor); err != nil {
				lr.Close()
				return nil, nil, fmt.Errorf("scan a proposal line: %w", err)
			}
			versions[i].Lines = append(versions[i].Lines, l)
		}
		lr.Close()
		if err := lr.Err(); err != nil {
			return nil, nil, err
		}
	}
	ar, err := tx.Query(ctx, `
		SELECT v.version, d.decision, d.via, coalesce(p.display_name, ''), coalesce(d.agreed_gross_minor, 0),
		       coalesce(d.declined_positions, '{}'), coalesce(d.note, ''), d.decided_at
		FROM quote_decisions d
		JOIN quotes v ON v.id = d.quote_id
		LEFT JOIN users u ON u.id = d.recorded_by
		LEFT JOIN people p ON p.id = u.person_id
		WHERE d.enquiry_id = $1 ORDER BY d.decided_at, d.id`, enquiryID)
	if err != nil {
		return nil, nil, fmt.Errorf("read the answers: %w", err)
	}
	defer ar.Close()
	var answers []EnquiryAnswer
	for ar.Next() {
		var a EnquiryAnswer
		var declined []int32
		if err := ar.Scan(&a.Version, &a.Decision, &a.Via, &a.RecordedBy, &a.AgreedGrossMinor, &declined, &a.Note, &a.DecidedAt); err != nil {
			return nil, nil, fmt.Errorf("scan an answer: %w", err)
		}
		for _, d := range declined {
			a.DeclinedPositions = append(a.DeclinedPositions, int(d))
		}
		answers = append(answers, a)
	}
	return versions, answers, ar.Err()
}

// EnquiryByID reads one enquiry whole.
func EnquiryByID(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string) (Enquiry, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return Enquiry{}, access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return Enquiry{}, ErrNotFound
	}
	var e Enquiry
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT enquiries.id, subject, coalesce(registration, ''), coalesce(vehicle_text, ''),
			       coalesce(customer_name, ''), coalesce(customer_phone, ''), enquiries.created_at,
			       coalesce((SELECT b.id::text FROM bookings b WHERE b.enquiry_id = enquiries.id
			                  AND b.status IN ('booked', 'arrived') ORDER BY b.created_at DESC LIMIT 1), ''),
			       s.name, coalesce(s.phone, ''), concat_ws(', ', s.address_line1, s.postal_code || ' ' || s.city)
			FROM enquiries JOIN shops s ON s.id = enquiries.shop_id WHERE enquiries.id = $1`, id).
			Scan(&e.ID, &e.Subject, &e.Registration, &e.Vehicle, &e.CustomerName, &e.CustomerPhone, &e.CreatedAt, &e.BookingID,
				&e.ShopName, &e.ShopPhone, &e.ShopAddress)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read enquiry: %w", err)
		}
		if e.Draft, err = draftLinesTx(ctx, tx, id); err != nil {
			return err
		}
		e.Versions, e.Answers, err = quotesOfEnquiryTx(ctx, tx, id)
		return err
	})
	return e, err
}

// Enquiries lists them, newest first.
func Enquiries(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]EnquiryRow, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	var out []EnquiryRow
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT e.id, e.subject, coalesce(e.registration, ''), coalesce(e.customer_name, ''), e.created_at,
			       coalesce(q.version, 0), q.id IS NOT NULL, coalesce(q.valid_until < now(), false),
			       coalesce(d.decision, ''), coalesce(cardinality(d.declined_positions), 0),
			       exists(SELECT 1 FROM bookings b WHERE b.enquiry_id = e.id AND b.status IN ('booked', 'arrived'))
			FROM enquiries e
			LEFT JOIN LATERAL (SELECT * FROM quotes WHERE enquiry_id = e.id ORDER BY version DESC LIMIT 1) q ON true
			LEFT JOIN LATERAL (SELECT * FROM quote_decisions WHERE quote_id = q.id ORDER BY decided_at DESC, id DESC LIMIT 1) d ON true
			ORDER BY e.created_at DESC LIMIT 200`)
		if err != nil {
			return fmt.Errorf("list enquiries: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var r EnquiryRow
			var sent, expired, booked bool
			var decision string
			var declined int
			if err := rows.Scan(&r.ID, &r.Subject, &r.Registration, &r.CustomerName, &r.CreatedAt,
				&r.LatestVersion, &sent, &expired, &decision, &declined, &booked); err != nil {
				return fmt.Errorf("scan an enquiry: %w", err)
			}
			var a *EnquiryAnswer
			if decision != "" {
				a = &EnquiryAnswer{Decision: decision}
				if declined > 0 {
					a.DeclinedPositions = make([]int, declined)
				}
			}
			r.Status = enquiryStatus(sent, expired, a, booked)
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// EnquiryAnswerInput is what the desk writes down when the customer answered
// by telephone or at the counter.
type EnquiryAnswerInput struct {
	Decision          string // "approved" or "declined"
	Via               string // "phone" or "counter"
	AgreedGrossMinor  int64  // for a yes: what the lines kept come to, including VAT
	DeclinedPositions []int  // for a partial yes: the lines they turned down
	Note              string
}

// acceptedLines is the version's lines without the ones turned down, and
// refuses positions that are not on it or a yes to nothing.
func acceptedLines(lines []QuoteLine, declined []int) ([]QuoteLine, error) {
	var kept []QuoteLine
	for _, d := range declined {
		if !slices.ContainsFunc(lines, func(l QuoteLine) bool { return l.Position == d }) {
			return nil, fmt.Errorf("%w: line %d is not on that proposal", ErrInvalid, d)
		}
	}
	for _, l := range lines {
		if !slices.Contains(declined, l.Position) {
			kept = append(kept, l)
		}
	}
	if len(kept) == 0 {
		return nil, fmt.Errorf("%w: a yes to none of the lines is a no", ErrInvalid)
	}
	return kept, nil
}

func recordEnquiryDecisionTx(ctx context.Context, tx pgx.Tx, shopID, enquiryID string, q EnquiryQuote,
	decision, via string, by, linkID *string, agreed *int64, declined []int, note string) error {
	if len(declined) == 0 {
		declined = nil
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO quote_decisions (shop_id, enquiry_id, quote_id, decision, via, recorded_by, job_link_id,
		       agreed_gross_minor, declined_positions, note)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, nullif($10, ''))`,
		shopID, enquiryID, q.ID, decision, via, by, linkID, agreed, declined, note)
	if err != nil {
		return fmt.Errorf("record the answer: %w", err)
	}
	return nil
}

// RecordEnquiryAnswer writes down a yes, a partial yes or a no the customer
// gave to the front desk. Only the newest version can be answered, and only
// while it stands; the amount is typed and has to be what the lines kept come
// to.
func RecordEnquiryAnswer(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, enquiryID, quoteID string, a EnquiryAnswerInput) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if a.Decision != "approved" && a.Decision != "declined" {
		return fmt.Errorf("%w: %q is not approved or declined", ErrInvalid, a.Decision)
	}
	if a.Via != "phone" && a.Via != "counter" {
		return fmt.Errorf("%w: say whether it was by telephone or at the counter", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockEnquiryTx(ctx, tx, enquiryID); err != nil {
			return err
		}
		versions, _, err := quotesOfEnquiryTx(ctx, tx, enquiryID)
		if err != nil {
			return err
		}
		if len(versions) == 0 || versions[0].ID != quoteID {
			return ErrQuoteSuperseded
		}
		latest := versions[0]
		if latest.Expired {
			return ErrQuoteExpired
		}
		var agreed *int64
		declined := a.DeclinedPositions
		if a.Decision == "approved" {
			kept, err := acceptedLines(latest.Lines, declined)
			if err != nil {
				return err
			}
			if want := grossOf(kept).GrossMinor; a.AgreedGrossMinor != want {
				return fmt.Errorf("%w: what is agreed comes to %s including VAT, not %s",
					ErrInvalid, money.Format(want), money.Format(a.AgreedGrossMinor))
			}
			agreed = &a.AgreedGrossMinor
		} else {
			declined = nil
		}
		by := scope.UserID
		return recordEnquiryDecisionTx(ctx, tx, scope.ShopID, enquiryID, latest, a.Decision, a.Via, &by, nil,
			agreed, declined, strings.TrimSpace(a.Note))
	})
}

// CreateEnquiryLink makes the customer's link to the proposal and returns its
// token, once. A new link closes the earlier ones.
func CreateEnquiryLink(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, enquiryID string) (string, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return "", access.ErrForbidden
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockEnquiryTx(ctx, tx, enquiryID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE job_links SET revoked_at = now() WHERE enquiry_id = $1 AND revoked_at IS NULL`, enquiryID); err != nil {
			return fmt.Errorf("close earlier links: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO job_links (shop_id, enquiry_id, token_sha256, created_by, expires_at)
			VALUES ($1, $2, $3, $4, now() + $5::interval)`,
			scope.ShopID, enquiryID, sum[:], scope.UserID, fmt.Sprintf("%d seconds", int(jobLinkLifetime.Seconds()))); err != nil {
			return fmt.Errorf("create link: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

// Offer is what the customer's link shows of an enquiry.
type Offer struct {
	ShopName, ShopPhone, ShopAddress string
	Subject, Registration, Vehicle   string
	Token                            string
	// The newest version, nil if none has been put forward.
	Quote *EnquiryQuote
	// Their latest answer to it.
	Answer *EnquiryAnswer
}

func offerLinkTx(ctx context.Context, tx pgx.Tx, token string) (enquiryID, linkID string, err error) {
	if token == "" {
		return "", "", ErrShareNotUsable
	}
	sum := sha256.Sum256([]byte(token))
	err = tx.QueryRow(ctx, `
		SELECT enquiry_id, id FROM job_links
		WHERE token_sha256 = $1 AND enquiry_id IS NOT NULL AND revoked_at IS NULL AND expires_at > now()`, sum[:]).
		Scan(&enquiryID, &linkID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrShareNotUsable
	}
	if err != nil {
		return "", "", fmt.Errorf("read link: %w", err)
	}
	return enquiryID, linkID, nil
}

// OfferFor answers a customer's link to a proposal. Wrong, closed and expired
// links are one answer.
func OfferFor(ctx context.Context, pool *pgxpool.Pool, shopID, token string) (Offer, error) {
	o := Offer{Token: token}
	err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		id, _, err := offerLinkTx(ctx, tx, token)
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			SELECT s.name, coalesce(s.phone, ''), concat_ws(', ', s.address_line1, s.postal_code || ' ' || s.city),
			       e.subject, coalesce(e.registration, ''), coalesce(e.vehicle_text, '')
			FROM enquiries e JOIN shops s ON s.id = e.shop_id WHERE e.id = $1`, id).
			Scan(&o.ShopName, &o.ShopPhone, &o.ShopAddress, &o.Subject, &o.Registration, &o.Vehicle); err != nil {
			return fmt.Errorf("read the offer: %w", err)
		}
		versions, answers, err := quotesOfEnquiryTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if len(versions) > 0 {
			o.Quote = &versions[0]
			for i := len(answers) - 1; i >= 0; i-- {
				if answers[i].Version == versions[0].Version {
					a := answers[i]
					o.Answer = &a
					break
				}
			}
		}
		return nil
	})
	return o, err
}

// OfferAnswer is the customer's own yes, partial yes or no on their link, to
// the version they were shown. A version that has been replaced is refused,
// and so is one that has stopped standing; neither is taken for another.
func OfferAnswer(ctx context.Context, pool *pgxpool.Pool, shopID, token, quoteID, decision string, declined []int) error {
	if decision != "approved" && decision != "declined" {
		return fmt.Errorf("%w: %q is not approved or declined", ErrInvalid, decision)
	}
	return database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		id, linkID, err := offerLinkTx(ctx, tx, token)
		if err != nil {
			return err
		}
		if err := lockEnquiryTx(ctx, tx, id); err != nil {
			return err
		}
		versions, answers, err := quotesOfEnquiryTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if len(versions) == 0 || versions[0].ID != quoteID {
			return ErrQuoteSuperseded
		}
		latest := versions[0]
		slices.Sort(declined)
		declined = slices.Compact(declined)
		// Pressing the same button twice is the same answer.
		for i := len(answers) - 1; i >= 0; i-- {
			if a := answers[i]; a.Version == latest.Version {
				if a.Decision == decision && (decision == "declined" || slices.Equal(a.DeclinedPositions, declined) ||
					(len(a.DeclinedPositions) == 0 && len(declined) == 0)) {
					return nil
				}
				break
			}
		}
		if latest.Expired {
			return ErrQuoteExpired
		}
		var agreed *int64
		if decision == "approved" {
			kept, err := acceptedLines(latest.Lines, declined)
			if err != nil {
				return err
			}
			g := grossOf(kept).GrossMinor
			agreed = &g
		} else {
			declined = nil
		}
		return recordEnquiryDecisionTx(ctx, tx, shopID, id, latest, decision, "link", nil, &linkID, agreed, declined, "")
	})
}

// BookEnquiry books the work the customer accepted, without anything having
// to be typed again, and returns the booking. The booking carries the
// accepted version; it does not make the car present, and nothing is priced
// again. A second call while a booking stands is that booking.
func BookEnquiry(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, enquiryID string, nb NewBooking) (string, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return "", access.ErrForbidden
	}
	var id string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockEnquiryTx(ctx, tx, enquiryID); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `
			SELECT id FROM bookings WHERE enquiry_id = $1 AND status IN ('booked', 'arrived')
			ORDER BY created_at DESC LIMIT 1`, enquiryID).Scan(&id)
		if err == nil {
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check booking: %w", err)
		}
		versions, answers, err := quotesOfEnquiryTx(ctx, tx, enquiryID)
		if err != nil {
			return err
		}
		if len(versions) == 0 {
			return fmt.Errorf("%w: nothing has been put to the customer", ErrInvalid)
		}
		var accepted *EnquiryAnswer
		for i := len(answers) - 1; i >= 0; i-- {
			if answers[i].Version == versions[0].Version {
				accepted = &answers[i]
				break
			}
		}
		if accepted == nil || accepted.Decision != "approved" {
			return fmt.Errorf("%w: the customer has not accepted the latest proposal", ErrInvalid)
		}
		var subject, reg, vehicle, name, phone string
		if err := tx.QueryRow(ctx, `
			SELECT subject, coalesce(registration, ''), coalesce(vehicle_text, ''), coalesce(customer_name, ''), coalesce(customer_phone, '')
			FROM enquiries WHERE id = $1`, enquiryID).Scan(&subject, &reg, &vehicle, &name, &phone); err != nil {
			return fmt.Errorf("read enquiry: %w", err)
		}
		if strings.TrimSpace(nb.What) == "" {
			nb.What = subject
		}
		if strings.TrimSpace(nb.Registration) == "" {
			nb.Registration = reg
		}
		if strings.TrimSpace(nb.CustomerName) == "" {
			nb.CustomerName = name
		}
		if strings.TrimSpace(nb.CustomerPhone) == "" {
			nb.CustomerPhone = phone
		}
		if err := checkBooking(&nb); err != nil {
			return err
		}
		id, err = createBookingTx(ctx, tx, scope, nb, enquiryID, versions[0].ID)
		return err
	})
	return id, err
}

// carryAcceptedLinesTx copies what the customer accepted onto the job a
// booking became, as agreed lines with the price they were shown. Nothing is
// priced again, and each line points at the answer that agreed to it, which
// points at the version and the enquiry.
func carryAcceptedLinesTx(ctx context.Context, tx pgx.Tx, shopID, jobID, bookingID string) error {
	var quoteID *string
	if err := tx.QueryRow(ctx, `SELECT quote_id FROM bookings WHERE id = $1`, bookingID).Scan(&quoteID); err != nil {
		return fmt.Errorf("read booking: %w", err)
	}
	if quoteID == nil {
		return nil
	}
	var decisionID, decision string
	var decidedAt time.Time
	err := tx.QueryRow(ctx, `
		SELECT id, decision, decided_at FROM quote_decisions WHERE quote_id = $1
		ORDER BY decided_at DESC, id DESC LIMIT 1`, *quoteID).Scan(&decisionID, &decision, &decidedAt)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && decision != "approved") {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the answer: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO work_order_lines (shop_id, work_order_id, position, kind, description, quantity,
		       unit_price_minor, vat_rate_bp, cost_bearer, approved_at, quote_decision_id)
		SELECT $1, $2, row_number() OVER (ORDER BY l.position), l.kind, l.description, l.quantity,
		       l.unit_price_minor, l.vat_rate_bp, 'customer', $4, d.id
		FROM quote_lines l JOIN quote_decisions d ON d.id = $3
		WHERE l.quote_id = $5 AND NOT (l.position = ANY (coalesce(d.declined_positions, '{}')))
		ORDER BY l.position`, shopID, jobID, decisionID, decidedAt, *quoteID)
	if err != nil {
		return fmt.Errorf("carry the accepted lines: %w", err)
	}
	return nil
}
