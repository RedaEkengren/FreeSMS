package workshop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/money"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Write-off reasons. A flag on a part could say "this is gone"; only a reason
// can say what a year of wrongly ordered parts cost, and that is a number a
// workshop owner can act on.
const (
	ReasonWrongPart     = "wrong_part_ordered"
	ReasonDamaged       = "damaged"
	ReasonOpened        = "opened_not_returnable"
	ReasonObsolete      = "obsolete"
	ReasonLost          = "lost"
	ReasonWarrantyScrap = "warranty_scrap"
)

var writeOffReasons = map[string]string{
	ReasonWrongPart:     "Wrongly ordered",
	ReasonDamaged:       "Damaged",
	ReasonOpened:        "Opened, cannot go back",
	ReasonObsolete:      "Obsolete",
	ReasonLost:          "Lost",
	ReasonWarrantyScrap: "Scrapped under warranty",
}

// WriteOffReasons lists the reasons, for a form.
func WriteOffReasons() map[string]string { return writeOffReasons }

// Part is a stocked item with its derived quantities.
type Part struct {
	ID       string
	Number   string
	Name     string
	Unit     string
	Location string

	CostMinor  *int64
	PriceMinor *int64

	// What it sells for on a job: its own price, or its cost with the markup
	// bands, or nil when it has neither. Filled where a part is offered.
	OfferedMinor *int64

	// Derived from the ledger, never stored.
	OnHand   float64
	Reserved float64
	Minimum  float64

	Codes []string
}

// Available is what can be promised to another job.
func (p Part) Available() float64 { return p.OnHand - p.Reserved }

// Short reports whether stock has fallen to or below the minimum.
func (p Part) Short() bool { return p.Minimum > 0 && p.Available() <= p.Minimum }

// Negative reports whether the ledger says there is less than nothing.
//
// Allowed on purpose. A negative figure is a symptom -- something was fitted
// that was never booked in -- and refusing to record it does not make the part
// reappear on the shelf, it just moves the discrepancy somewhere nobody looks.
func (p Part) Negative() bool { return p.OnHand < 0 }

// ReservedNote is the catalogue key for what is spoken for, and empty when
// nothing is. A key rather than a finished sentence, so the numbers are
// substituted by the page in the reader's language; empty rather than a
// condition in the template, so the page can join the facts it has without
// knowing which of them exist.
func (p Part) ReservedNote() string {
	if p.Reserved == 0 {
		return ""
	}
	return "%v reserved \u00b7 %v available"
}

// ShortfallNote is the catalogue key warning that putting this part on a job
// will take the shelf below nothing, and empty when it will not.
func (p Part) ShortfallNote() string {
	if p.Available() > 0 {
		return ""
	}
	return "none on the shelf; this will show as a shortfall"
}

// Cost and Price render the money.
func (p Part) Cost() string {
	if p.CostMinor == nil {
		return ""
	}
	return money.Format(*p.CostMinor)
}

func (p Part) Price() string {
	if p.PriceMinor == nil {
		return ""
	}
	return money.Format(*p.PriceMinor)
}

// Movement is one entry in the ledger.
type Movement struct {
	ID        string
	Kind      string
	Quantity  float64
	Reason    string
	MovedAt   time.Time
	MovedBy   string
	Note      string
	OrderNum  *int64
	CostMinor *int64
}

// KindLabel is what a movement was, as a catalogue key: a person reads
// "taken out to a job", not "consumed".
func (m Movement) KindLabel() string {
	switch m.Kind {
	case "received":
		return "arrived"
	case "reserved":
		return "put aside for a job"
	case "unreserved":
		return "no longer put aside"
	case "consumed":
		if m.OrderNum != nil {
			return "taken out to a job"
		}
		return "used"
	case "put_back":
		return "put back on the shelf"
	case "returned":
		return "went back to the supplier"
	case "written_off":
		return "written off"
	case "counted":
		return "stocktake difference"
	}
	return m.Kind
}

// ReasonLabel renders a write-off reason for a person.
func (m Movement) ReasonLabel() string {
	if label, ok := writeOffReasons[m.Reason]; ok {
		return label
	}
	return m.Reason
}

const partColumns = `
	p.id, p.number, p.name, p.unit, coalesce(p.location, ''),
	p.cost_minor, p.price_minor, p.minimum_quantity,
	coalesce((SELECT sum(m.quantity) FROM stock_movements m
	           WHERE m.part_id = p.id
	             AND m.kind IN ('received', 'consumed', 'put_back', 'returned', 'written_off', 'counted')), 0) AS on_hand,
	coalesce((SELECT sum(m.quantity) FROM stock_movements m
	           WHERE m.part_id = p.id AND m.kind IN ('reserved', 'unreserved')), 0) AS reserved`

func scanPart(row pgx.Row) (Part, error) {
	var p Part
	err := row.Scan(&p.ID, &p.Number, &p.Name, &p.Unit, &p.Location,
		&p.CostMinor, &p.PriceMinor, &p.Minimum, &p.OnHand, &p.Reserved)
	return p, err
}

// Parts lists the catalogue with what the ledger says is there.
func Parts(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]Part, error) {
	if !scope.Role.SeesParts() {
		return nil, access.ErrForbidden
	}
	var out []Part
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT`+partColumns+`
			FROM parts p WHERE p.active ORDER BY p.number`)
		if err != nil {
			return fmt.Errorf("list parts: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanPart(rows)
			if err != nil {
				return fmt.Errorf("scan part: %w", err)
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

// PartByCode finds a part by any code it answers to.
//
// One part is one thing under several supplier numbers, and the barcode
// belongs to the packaging. Codes map many to one onto a part.
func PartByCode(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, code string) (Part, error) {
	// A part carries its cost and price.
	if !scope.Role.SeesParts() {
		return Part{}, access.ErrForbidden
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return Part{}, ErrNotFound
	}
	var p Part
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		p, err = scanPart(tx.QueryRow(ctx, `SELECT`+partColumns+`
			FROM parts p
			WHERE p.active AND (p.number = $1
			   OR EXISTS (SELECT 1 FROM part_codes c WHERE c.part_id = p.id AND c.code = $1))
			LIMIT 1`, code))
		if err == pgx.ErrNoRows {
			return ErrNotFound
		}
		return err
	})
	return p, err
}

// Move records one entry in the ledger.
//
// Everything that changes stock goes through here, so there is one place where
// a quantity can be written and it always appends.
func Move(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, m Movement, partID string, workOrderID, lineID string) error {
	if !scope.Role.SeesParts() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(partID) {
		return ErrNotFound
	}
	if m.Quantity == 0 {
		return fmt.Errorf("%w: a movement of nothing is not a movement", ErrInvalid)
	}
	switch m.Kind {
	case "received", "reserved", "unreserved", "consumed", "returned", "written_off", "counted":
	default:
		return fmt.Errorf("%w: %q is not a kind of stock movement", ErrInvalid, m.Kind)
	}
	if (m.Kind == "written_off") != (m.Reason != "") {
		return fmt.Errorf("%w: a write-off needs a reason, and nothing else takes one", ErrInvalid)
	}
	if m.Reason != "" {
		if _, ok := writeOffReasons[m.Reason]; !ok {
			return fmt.Errorf("%w: %q is not a reason this system knows", ErrInvalid, m.Reason)
		}
	}

	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		return moveTx(ctx, tx, scope, m, partID, workOrderID, lineID)
	})
}

func moveTx(ctx context.Context, tx pgx.Tx, scope access.Scope, m Movement, partID, workOrderID, lineID string) error {
	// The cost at the moment it moved, taken from the part when the caller did
	// not say. A valuation that reprices history is one nobody can reconcile.
	//
	// The part's row is locked whatever the caller said, so movements on one
	// part happen one at a time. A stocktake reads the ledger and writes the
	// difference, and a consumption landing between the two would make the
	// difference wrong by exactly that consumption.
	var current *int64
	if err := tx.QueryRow(ctx, `SELECT cost_minor FROM parts WHERE id = $1 FOR UPDATE`, partID).Scan(&current); err != nil {
		if err == pgx.ErrNoRows {
			return ErrNotFound
		}
		return fmt.Errorf("read cost: %w", err)
	}
	cost := m.CostMinor
	if cost == nil {
		cost = current
	}

	_, err := tx.Exec(ctx, `
		INSERT INTO stock_movements
		  (shop_id, part_id, kind, quantity, reason, unit_cost_minor,
		   work_order_id, line_id, moved_by, note)
		VALUES ($1, $2, $3, $4, nullif($5,''), $6,
		        nullif($7,'')::uuid, nullif($8,'')::uuid, $9, nullif($10,''))`,
		scope.ShopID, partID, m.Kind, m.Quantity, m.Reason, cost,
		workOrderID, lineID, scope.UserID, strings.TrimSpace(m.Note))
	if err != nil {
		return fmt.Errorf("record movement: %w", err)
	}
	return nil
}

// MovementsFor lists a part's ledger, newest first.
func MovementsFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, partID string) ([]Movement, error) {
	if !scope.Role.SeesParts() {
		return nil, access.ErrForbidden
	}
	var out []Movement
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT m.id, m.kind, m.quantity, coalesce(m.reason, ''), m.moved_at,
			       coalesce(p.display_name, ''), coalesce(m.note, ''),
			       w.number, m.unit_cost_minor
			FROM stock_movements m
			LEFT JOIN users u   ON u.id = m.moved_by
			LEFT JOIN people p  ON p.id = u.person_id
			LEFT JOIN work_orders w ON w.id = m.work_order_id
			WHERE m.part_id = $1
			ORDER BY m.moved_at DESC
			LIMIT 100`, partID)
		if err != nil {
			return fmt.Errorf("list movements: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var m Movement
			if err := rows.Scan(&m.ID, &m.Kind, &m.Quantity, &m.Reason, &m.MovedAt,
				&m.MovedBy, &m.Note, &m.OrderNum, &m.CostMinor); err != nil {
				return fmt.Errorf("scan movement: %w", err)
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

// WriteOffCost is what a period of write-offs cost, by reason.
//
// The number the ledger exists for. A shop that discovers it writes off forty
// thousand a year in wrongly ordered parts has found a process problem, not an
// accounting entry.
type WriteOffCost struct {
	Reason    string
	Label     string
	Quantity  float64
	CostMinor int64
}

// Cost renders the money.
func (w WriteOffCost) Cost() string { return money.Format(w.CostMinor) }

// WriteOffs totals what was written off in a period, by reason.
func WriteOffs(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, from, to time.Time) ([]WriteOffCost, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	var out []WriteOffCost
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT reason, sum(-quantity),
			       coalesce(sum(-quantity * coalesce(unit_cost_minor, 0))::bigint, 0)
			FROM stock_movements
			WHERE kind = 'written_off' AND moved_at >= $1 AND moved_at < $2
			GROUP BY reason
			ORDER BY 3 DESC`, from, to)
		if err != nil {
			return fmt.Errorf("total write-offs: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var w WriteOffCost
			if err := rows.Scan(&w.Reason, &w.Quantity, &w.CostMinor); err != nil {
				return fmt.Errorf("scan write-off: %w", err)
			}
			w.Label = writeOffReasons[w.Reason]
			out = append(out, w)
		}
		return rows.Err()
	})
	return out, err
}

// ReserveForJob puts stock aside for a job.
//
// Reserved is neither sold nor free. Modelling it as "reduce the number" loses
// the fact that it happened, and then a part that comes back has to be
// explained rather than simply released.
func ReserveForJob(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID, partID string, quantity float64) error {
	// Stock is the parts desk's.
	if !scope.Role.SeesParts() {
		return access.ErrForbidden
	}
	if quantity <= 0 {
		return fmt.Errorf("%w: reserve a positive quantity", ErrInvalid)
	}
	// Positive: this is an amount set aside, not a reduction in what is on the
	// shelf. Recording it negative made available stock go *up* when something
	// was reserved, which a test caught.
	return Move(ctx, pool, scope, Movement{Kind: "reserved", Quantity: quantity}, partID, jobID, "")
}

// ConsumeForJob books a part out because it went on a car.
//
// It releases whatever was reserved for the same job first, so that fitting a
// part does not leave it both reserved and consumed -- which would show a
// workshop short of stock it has.
func ConsumeForJob(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID, partID, lineID string, quantity float64) error {
	if !scope.Role.SeesParts() {
		return access.ErrForbidden
	}
	if quantity <= 0 {
		return fmt.Errorf("%w: consume a positive quantity", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := releaseReservations(ctx, tx, scope, jobID, partID); err != nil {
			return err
		}
		return moveTx(ctx, tx, scope,
			Movement{Kind: "consumed", Quantity: -quantity}, partID, jobID, lineID)
	})
}

// releaseReservations undoes what is still reserved for a job.
func releaseReservations(ctx context.Context, tx pgx.Tx, scope access.Scope, jobID, partID string) error {
	q := `
		SELECT part_id, sum(quantity)
		FROM stock_movements
		WHERE work_order_id = $1 AND kind IN ('reserved', 'unreserved')`
	args := []any{jobID}
	if partID != "" {
		q += ` AND part_id = $2`
		args = append(args, partID)
	}
	q += ` GROUP BY part_id HAVING sum(quantity) <> 0`

	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("read reservations: %w", err)
	}
	type release struct {
		part     string
		quantity float64
	}
	var toRelease []release
	for rows.Next() {
		var r release
		if err := rows.Scan(&r.part, &r.quantity); err != nil {
			rows.Close()
			return fmt.Errorf("scan reservation: %w", err)
		}
		toRelease = append(toRelease, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, r := range toRelease {
		if err := moveTx(ctx, tx, scope,
			Movement{Kind: "unreserved", Quantity: -r.quantity,
				Note: "Released when the job stopped needing it"},
			r.part, jobID, ""); err != nil {
			return err
		}
	}
	return nil
}

// PriceFromCost applies the shop's markup bands.
//
// Bands rather than one percentage, because a five-krona clip and a
// five-thousand-krona turbo do not carry the same markup and every workshop
// knows it. The narrowest band whose ceiling the cost is under wins; the open
// top band catches everything above. No bands means no markup: the cost is
// the price, and the shop can see it has not set any rather than being
// quietly given one.
func PriceFromCost(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, costMinor int64) (int64, error) {
	// The markup is the shop's pricing.
	if !scope.Role.SeesParts() {
		return 0, access.ErrForbidden
	}
	var bands []PriceBand
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		bands, err = bandsTx(ctx, tx)
		return err
	})
	if err != nil {
		return 0, err
	}
	return priceWithBands(bands, costMinor), nil
}

// priceWithBands is the one place a cost becomes a price. The shelf on a job
// shows what it returns and the line added from it stores what it returns, so
// the two cannot disagree.
func priceWithBands(bands []PriceBand, costMinor int64) int64 {
	if costMinor <= 0 {
		return 0
	}
	// Bands come ordered by ceiling, open band last.
	for _, b := range bands {
		if b.UpToMinor == nil || costMinor < *b.UpToMinor {
			return money.WithMarkup(costMinor, b.MarkupBasis)
		}
	}
	return costMinor
}

// offeredPrice is what a part sells for: its own price if it has one, even
// zero, because setting it is a decision; otherwise its cost with the markup;
// otherwise nothing -- and nothing is not zero.
func offeredPrice(bands []PriceBand, price, cost *int64) *int64 {
	if price != nil {
		return price
	}
	if cost != nil {
		v := priceWithBands(bands, *cost)
		return &v
	}
	return nil
}

func bandsTx(ctx context.Context, tx pgx.Tx) ([]PriceBand, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, up_to_minor, markup_basis FROM price_bands ORDER BY up_to_minor NULLS LAST`)
	if err != nil {
		return nil, fmt.Errorf("read price bands: %w", err)
	}
	defer rows.Close()
	var out []PriceBand
	for rows.Next() {
		var b PriceBand
		if err := rows.Scan(&b.ID, &b.UpToMinor, &b.MarkupBasis); err != nil {
			return nil, fmt.Errorf("scan price band: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// PriceBand is one cost band and its markup.
type PriceBand struct {
	ID          string
	UpToMinor   *int64
	MarkupBasis int
}

// TopBand reports whether this is the band with no ceiling. The words and the
// amount are the page's to put together: "up to %s" is a catalogue key and the
// amount inside it is formatted for the reader, so neither can be built here.
func (b PriceBand) TopBand() bool { return b.UpToMinor == nil }

// UpTo renders the ceiling as a plain decimal, for anything that is not a
// rendered page.
func (b PriceBand) UpTo() string {
	if b.UpToMinor == nil {
		return ""
	}
	return money.Format(*b.UpToMinor)
}

// Markup renders the percentage.
func (b PriceBand) Markup() string { return fmt.Sprintf("%.1f%%", float64(b.MarkupBasis)/100) }

// PriceBands lists the shop's matrix.
func PriceBands(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]PriceBand, error) {
	if !scope.Role.SeesParts() {
		return nil, access.ErrForbidden
	}
	var out []PriceBand
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = bandsTx(ctx, tx)
		return err
	})
	return out, err
}

// SavePriceBand adds or replaces a band.
func SavePriceBand(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, upTo *int64, markupBasis int) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if markupBasis < 0 || markupBasis > 100000 {
		return fmt.Errorf("%w: a markup between 0 and 1000%% please", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO price_bands (shop_id, up_to_minor, markup_basis)
			VALUES ($1, $2, $3)
			ON CONFLICT (shop_id, up_to_minor) DO UPDATE SET markup_basis = excluded.markup_basis`,
			scope.ShopID, upTo, markupBasis)
		return err
	})
}

// PartsForJob is the short list of parts the job page offers, and the results
// of a search when somebody types one.
//
// The whole catalogue used to be rendered inline, one card with a quantity box
// and a button per part. Nine parts in a demonstration and two thousand in a
// workshop, which makes the job page thousands of lines with nothing to
// navigate by. The time library on the same page never had this problem
// because it is filtered to the vehicle; this is that idea applied to the
// shelf, where the filter cannot be the vehicle because a wiper blade fits
// everything.
//
// With no query, the order is what somebody is most likely to reach for:
// already on this order first, because a second brake pad set is the commonest
// second line; then by how recently the shop has moved the part at all, which
// puts the fast-moving shelf above the turbo nobody has touched since spring.
//
// With a query, the number is matched as a prefix and the name anywhere. The
// number comes first because that is what is on the shelf label and what a
// handheld scanner types.
func PartsForJob(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID, query string) ([]Part, error) {
	if !scope.Role.SeesParts() {
		return nil, access.ErrForbidden
	}

	// A cap rather than paging. Somebody looking at a job wants the part they
	// have in their hand, and a list long enough to scroll means the search
	// box is the answer, not the next page.
	// Six, not eight. The list is a shortcut, not a catalogue: past about
	// half a dozen the eye stops scanning and the search box is the faster
	// route anyway.
	const shortList, searchResults = 6, 25

	query = strings.TrimSpace(query)
	var out []Part
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		// The job has to exist in this shop before its shelf is listed.
		//
		// Without this the identifier is only used in an EXISTS that orders
		// the results, so another shop's job id -- or a typo -- returned this
		// shop's parts with a 200 and a row of buttons that post to an order
		// that is not there. Found by the route sweep, which is what it is for.
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM work_orders WHERE id = $1)`, jobID).Scan(&exists); err != nil {
			return fmt.Errorf("check job: %w", err)
		}
		if !exists {
			return ErrNotFound
		}

		var rows pgx.Rows
		var err error
		if query == "" {
			rows, err = tx.Query(ctx, `SELECT`+partColumns+`
				FROM parts p
				WHERE p.active
				ORDER BY
				    EXISTS (SELECT 1 FROM work_order_lines l
				             WHERE l.work_order_id = $1 AND l.part_id = p.id) DESC,
				    (SELECT max(m.moved_at) FROM stock_movements m
				      WHERE m.part_id = p.id) DESC NULLS LAST,
				    p.number
				LIMIT $2`, jobID, shortList)
		} else {
			rows, err = tx.Query(ctx, `SELECT`+partColumns+`
				FROM parts p
				WHERE p.active
				  AND (upper(p.number) LIKE upper($1) || '%' OR p.name ILIKE '%' || $1 || '%')
				ORDER BY
				    -- An exact number is a scanner, and a scanner is certain.
				    (upper(p.number) = upper($1)) DESC,
				    (upper(p.number) LIKE upper($1) || '%') DESC,
				    p.number
				LIMIT $2`, query, searchResults)
		}
		if err != nil {
			return fmt.Errorf("list parts for job: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanPart(rows)
			if err != nil {
				return fmt.Errorf("scan part: %w", err)
			}
			out = append(out, p)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		bands, err := bandsTx(ctx, tx)
		if err != nil {
			return err
		}
		for i := range out {
			out[i].OfferedMinor = offeredPrice(bands, out[i].PriceMinor, out[i].CostMinor)
		}
		return nil
	})
	return out, err
}

// partPriceTx is what a part sells for right now, read inside the
// transaction that puts it on a job.
func partPriceTx(ctx context.Context, tx pgx.Tx, partID string) (*int64, string, error) {
	var price, cost *int64
	var number string
	if err := tx.QueryRow(ctx, `SELECT price_minor, cost_minor, number FROM parts WHERE id = $1 AND active`,
		partID).Scan(&price, &cost, &number); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", ErrNotFound
		}
		return nil, "", fmt.Errorf("read part price: %w", err)
	}
	bands, err := bandsTx(ctx, tx)
	if err != nil {
		return nil, "", err
	}
	return offeredPrice(bands, price, cost), number, nil
}

// Stocktake records what is on the shelf, as a counted movement of the
// difference from the ledger.
//
// The form used to ask for the difference, and refused a negative number --
// so ten in the books and eight on the shelf could not be recorded except by
// calling the missing two a write-off or a return, which they were not. A
// count is a count: the person says what they see, and the arithmetic is
// done here, under the part's lock, so nothing booked in between makes it
// stale.
//
// A count that matches records nothing, and says so.
func Stocktake(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, partID string, countedMilli int64, note string) (differenceMilli int64, err error) {
	if !scope.Role.SeesParts() {
		return 0, access.ErrForbidden
	}
	if !looksLikeUUID(partID) {
		return 0, ErrNotFound
	}
	if countedMilli < 0 {
		return 0, fmt.Errorf("%w: a shelf cannot hold fewer than none", ErrInvalid)
	}
	err = database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM parts WHERE id = $1 FOR UPDATE`, partID); err != nil {
			return fmt.Errorf("lock part: %w", err)
		}
		// In thousandths, as numeric, so 0,1 of a litre is not a float.
		if err := tx.QueryRow(ctx, `
			SELECT ($2::numeric / 1000 - coalesce(sum(quantity), 0)) * 1000
			FROM stock_movements
			WHERE part_id = $1
			  AND kind IN ('received', 'consumed', 'put_back', 'returned', 'written_off', 'counted')`,
			partID, countedMilli).Scan(&differenceMilli); err != nil {
			return fmt.Errorf("read on hand: %w", err)
		}
		if differenceMilli == 0 {
			return nil
		}
		return moveTx(ctx, tx, scope, Movement{
			Kind: "counted", Quantity: float64(differenceMilli) / 1000, Note: note,
		}, partID, "", "")
	})
	return differenceMilli, err
}
