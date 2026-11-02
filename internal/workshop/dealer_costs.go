package workshop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/money"
)

// Costs per stock unit (#164): every krona spent on a stock car, kept
// against its unit and apart from the tax purchase price. Nothing here adds
// a cost to that price: the margin scheme reads the purchase (#161) and
// nothing else, and the costs make only the business margin true (#182).
// Entries are never changed; a correction reverses one.

// The sources a cost comes from.
const (
	CostFromWorkOrder       = "work_order"
	CostFromOutsourced      = "outsourced"
	CostFromSupplierInvoice = "supplier_invoice"
	CostFromManual          = "manual"
)

// StockCost is one cost entry, as whoever may see what a car cost reads it.
type StockCost struct {
	ID              string
	StockUnitID     string
	Kind            string
	NetMinor        int64
	InputVATMinor   int64
	VATRateBasis    int
	SupplierPartyID string
	ExternalRef     string
	IncurredOn      time.Time
	Source          string
	WorkOrderID     string
	PostSale        bool
	Currency        string
	OriginalMinor   int64
	RateMicro       int64
	RateSource      string
	Note            string
	ReversesID      string
	// Reversed by a later entry.
	Reversed   bool
	RecordedAt time.Time
	RecordedBy string
}

// KindLabel is the kind of cost in words, a catalogue key.
func (c StockCost) KindLabel() string { return CostKindLabel(c.Kind) }

// StockCosts is a unit's costs and what they come to: before the sale, and
// after it, net of VAT.
type StockCosts struct {
	Entries       []StockCost
	BeforeSale    int64
	AfterSale     int64
	InputVATMinor int64
}

// TotalMinor is every cost, before and after the sale.
func (c StockCosts) TotalMinor() int64 { return c.BeforeSale + c.AfterSale }

// NewStockCost is a cost entered by hand or from a supplier's invoice.
// InputVATMinor nil works the VAT out at the rate; a supplier's credit note
// is a cost below nothing.
type NewStockCost struct {
	Kind            string
	NetMinor        int64
	InputVATMinor   *int64
	VATRateBasis    int
	SupplierPartyID string
	ExternalRef     string
	IncurredOn      time.Time
	// From a supplier's invoice rather than a person's own entry.
	FromInvoice bool
	// Another currency: its code, the net amount in it, kronor for one unit
	// in millionths and where the rate came from; NetMinor is then worked
	// out and not given.
	Currency      string
	OriginalMinor int64
	RateMicro     int64
	RateSource    string
	Note          string
}

var (
	errCostKind     = fmt.Errorf("%w: that is not a kind of cost", ErrInvalid)
	errCostAmount   = fmt.Errorf("%w: a cost is up to a hundred million kronor either way, and its VAT within it", ErrInvalid)
	errCostRate     = fmt.Errorf("%w: a VAT rate is between 0 and 100 per cent", ErrInvalid)
	errCostDate     = fmt.Errorf("%w: say the day it was spent, and not a day still to come", ErrInvalid)
	errCostText     = fmt.Errorf("%w: a reference is at most 100 characters and a note 500", ErrInvalid)
	errCostCurrency = fmt.Errorf("%w: in another currency, say which, how much in it, the rate and where the rate came from", ErrInvalid)
	errCostReversed = fmt.Errorf("%w: that cost has already been reversed", ErrInvalid)
	errCostReversal = fmt.Errorf("%w: a reversal is not reversed; enter the cost again instead", ErrInvalid)
	errJobOpen      = fmt.Errorf("%w: that job is still open; its cost is posted once it is done", ErrInvalid)
	errJobNotStock  = fmt.Errorf("%w: that job was not opened on a car in stock", ErrInvalid)
	errLabourRate   = fmt.Errorf("%w: an hour's cost is from nothing up to a hundred thousand kronor", ErrInvalid)
	// ErrCostNoReason is a cost reversed without saying why.
	ErrCostNoReason = fmt.Errorf("%w: say why the cost is reversed, in 300 characters or fewer", ErrInvalid)
)

// RecordStockCost records a cost spent on a stock car. One dated after the
// sale that stands is after it (stock_cost_post_sale, 0099): it changes
// the margin, never the VAT. For whoever buys cars, in a shop that sells them.
func RecordStockCost(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, unitID string, n NewStockCost) (StockCost, error) {
	if !scope.Role.BuysVehicles() {
		return StockCost{}, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return StockCost{}, err
	}
	if !looksLikeUUID(unitID) || (n.SupplierPartyID != "" && !looksLikeUUID(n.SupplierPartyID)) {
		return StockCost{}, ErrNotFound
	}
	if !slices.Contains(CostKinds, n.Kind) {
		return StockCost{}, errCostKind
	}
	if n.VATRateBasis < 0 || n.VATRateBasis > 10000 {
		return StockCost{}, errCostRate
	}
	if n.IncurredOn.IsZero() || n.IncurredOn.After(time.Now().Add(24*time.Hour)) {
		return StockCost{}, errCostDate
	}
	n.ExternalRef, n.Note = strings.TrimSpace(n.ExternalRef), strings.TrimSpace(n.Note)
	if utf8.RuneCountInString(n.ExternalRef) > 100 || utf8.RuneCountInString(n.Note) > 500 {
		return StockCost{}, errCostText
	}
	n.Currency = strings.ToUpper(strings.TrimSpace(n.Currency))
	if n.Currency == "SEK" {
		n.Currency = ""
	}
	if n.Currency != "" {
		if len(n.Currency) != 3 || strings.Trim(n.Currency, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" || n.NetMinor != 0 ||
			n.OriginalMinor < -maxOriginalMinor || n.OriginalMinor > maxOriginalMinor || n.RateMicro < 1 || n.RateMicro > maxRateMicro ||
			strings.TrimSpace(n.RateSource) == "" || utf8.RuneCountInString(n.RateSource) > 200 {
			return StockCost{}, errCostCurrency
		}
		n.NetMinor = money.Scale(n.OriginalMinor, n.RateMicro, 1_000_000)
	} else if n.OriginalMinor != 0 || n.RateMicro != 0 || strings.TrimSpace(n.RateSource) != "" {
		return StockCost{}, errCostCurrency
	}
	vat := money.VAT(n.NetMinor, n.VATRateBasis)
	if n.InputVATMinor != nil {
		vat = *n.InputVATMinor
	}
	abs := func(x int64) int64 {
		if x < 0 {
			return -x
		}
		return x
	}
	if abs(n.NetMinor) > 10_000_000_000 || abs(vat) > abs(n.NetMinor) || (vat != 0 && (vat < 0) != (n.NetMinor < 0)) {
		return StockCost{}, errCostAmount
	}
	source := CostFromManual
	if n.FromInvoice {
		source = CostFromSupplierInvoice
	}
	var out StockCost
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM stock_units WHERE id = $1)`, unitID).Scan(&exists); err != nil {
			return fmt.Errorf("read stock unit: %w", err)
		}
		if !exists {
			return ErrNotFound
		}
		if n.SupplierPartyID != "" {
			if err := partySeenTx(ctx, tx, scope, n.SupplierPartyID); err != nil {
				return err
			}
		}
		var id string
		if err := tx.QueryRow(ctx, `
			INSERT INTO stock_cost_entries (shop_id, stock_unit_id, kind, net_minor, input_vat_minor, vat_rate_basis,
			       supplier_party_id, external_ref, incurred_on, source, happened_at, currency, original_net_minor, rate_micro,
			       rate_source, note, recorded_by)
			VALUES ($1, $2, $3, $4, $5, $6, CASE WHEN $7 <> '' THEN party_now($7::uuid) END, nullif($8, ''), $9, $10,
			        stock_cost_dated($1, $9::date),
			        nullif($11, ''), CASE WHEN $11 <> '' THEN $12::bigint END, CASE WHEN $11 <> '' THEN $13::bigint END,
			        nullif(btrim($14), ''), nullif($15, ''), NULLIF($16, '')::uuid)
			RETURNING id`,
			scope.ShopID, unitID, n.Kind, n.NetMinor, vat, n.VATRateBasis, n.SupplierPartyID, n.ExternalRef,
			n.IncurredOn.Format(time.DateOnly), source, n.Currency, n.OriginalMinor, n.RateMicro, n.RateSource,
			n.Note, scope.UserID).Scan(&id); err != nil {
			return fmt.Errorf("record the cost: %w", err)
		}
		var err error
		out, err = stockCostTx(ctx, tx, id)
		return err
	})
	return out, err
}

const stockCostColumns = `
	SELECT c.id, c.stock_unit_id, c.kind, c.net_minor, c.input_vat_minor, c.vat_rate_basis,
	       coalesce(c.supplier_party_id::text, ''), coalesce(c.external_ref, ''), c.incurred_on, c.source,
	       coalesce(c.work_order_id::text, ''), stock_cost_post_sale(c.stock_unit_id, c.happened_at), coalesce(c.currency, ''), coalesce(c.original_net_minor, 0),
	       coalesce(c.rate_micro, 0), coalesce(c.rate_source, ''), coalesce(c.note, ''), coalesce(c.reverses_id::text, ''),
	       EXISTS (SELECT 1 FROM stock_cost_entries r WHERE r.reverses_id = c.id), c.recorded_at, coalesce(pe.display_name, '')
	FROM stock_cost_entries c
	LEFT JOIN users u ON u.id = c.recorded_by LEFT JOIN people pe ON pe.id = u.person_id`

func scanStockCost(row pgx.Row) (StockCost, error) {
	var c StockCost
	err := row.Scan(&c.ID, &c.StockUnitID, &c.Kind, &c.NetMinor, &c.InputVATMinor, &c.VATRateBasis, &c.SupplierPartyID,
		&c.ExternalRef, &c.IncurredOn, &c.Source, &c.WorkOrderID, &c.PostSale, &c.Currency, &c.OriginalMinor, &c.RateMicro,
		&c.RateSource, &c.Note, &c.ReversesID, &c.Reversed, &c.RecordedAt, &c.RecordedBy)
	return c, err
}

func stockCostTx(ctx context.Context, tx pgx.Tx, id string) (StockCost, error) {
	c, err := scanStockCost(tx.QueryRow(ctx, stockCostColumns+` WHERE c.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return StockCost{}, ErrNotFound
	}
	if err != nil {
		return StockCost{}, fmt.Errorf("read cost: %w", err)
	}
	return c, nil
}

// ReverseStockCost corrects a cost by an entry that takes it back, with why.
// The entry reversed stays. For whoever buys cars, in a shop that sells
// them.
func ReverseStockCost(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, entryID, reason string) (StockCost, error) {
	if !scope.Role.BuysVehicles() {
		return StockCost{}, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return StockCost{}, err
	}
	if !looksLikeUUID(entryID) {
		return StockCost{}, ErrNotFound
	}
	reason, err := checkReason(reason, ErrCostNoReason)
	if err != nil {
		return StockCost{}, err
	}
	var out StockCost
	err = database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('cost:' || $1::text, 0))`, entryID); err != nil {
			return err
		}
		c, err := stockCostTx(ctx, tx, entryID)
		if err != nil {
			return err
		}
		switch {
		case c.Reversed:
			return errCostReversed
		case c.ReversesID != "":
			return errCostReversal
		}
		// Taken back as the original was entered: from where it came, so
		// a job's posting reversed may be posted again, and for the same
		// moment, so on the same side of the sale.
		var id string
		if err := tx.QueryRow(ctx, `
			INSERT INTO stock_cost_entries (shop_id, stock_unit_id, kind, net_minor, input_vat_minor, vat_rate_basis,
			       supplier_party_id, external_ref, incurred_on, source, work_order_id, happened_at, currency,
			       original_net_minor, rate_micro, rate_source, note, reverses_id, recorded_by)
			SELECT shop_id, stock_unit_id, kind, -net_minor, -input_vat_minor, vat_rate_basis, supplier_party_id, external_ref,
			       (clock_timestamp() AT TIME ZONE (SELECT timezone FROM shops WHERE id = shop_id))::date,
			       CASE WHEN source = 'outsourced' THEN 'manual' ELSE source END, work_order_id, happened_at, currency,
			       -original_net_minor, rate_micro, rate_source, $2, id, NULLIF($3, '')::uuid
			FROM stock_cost_entries WHERE id = $1 RETURNING id`, entryID, reason, scope.UserID).Scan(&id); err != nil {
			return fmt.Errorf("reverse the cost: %w", err)
		}
		out, err = stockCostTx(ctx, tx, id)
		return err
	})
	return out, err
}

// CostsOfUnit reads a unit's costs, for whoever may see what a car cost
// (#145): never a salesperson, however they ask. Reading is Core's.
func CostsOfUnit(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, unitID string) (StockCosts, error) {
	if !scope.Role.SeesPurchasePrice() {
		return StockCosts{}, access.ErrForbidden
	}
	if !looksLikeUUID(unitID) {
		return StockCosts{}, ErrNotFound
	}
	var out StockCosts
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM stock_units WHERE id = $1)`, unitID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		rows, err := tx.Query(ctx, stockCostColumns+` WHERE c.stock_unit_id = $1 ORDER BY c.incurred_on, c.recorded_at`, unitID)
		if err != nil {
			return fmt.Errorf("read costs: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanStockCost(rows)
			if err != nil {
				return err
			}
			out.Entries = append(out.Entries, c)
			if c.PostSale {
				out.AfterSale += c.NetMinor
			} else {
				out.BeforeSale += c.NetMinor
			}
			out.InputVATMinor += c.InputVATMinor
		}
		return rows.Err()
	})
	return out, err
}

// SetInternalLabourRate sets what an hour of the workshop's own time on a
// stock car costs the business, before VAT. For whoever runs the shop, in a
// shop that sells cars.
func SetInternalLabourRate(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, minor int64) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return err
	}
	if minor < 0 || minor > 10_000_000 {
		return errLabourRate
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO stock_cost_settings (shop_id, internal_labour_rate_minor, changed_by) VALUES ($1, $2, NULLIF($3, '')::uuid)`,
			scope.ShopID, minor, scope.UserID)
		return err
	})
}

// InternalLabourRate reads what an hour of the workshop's own time on a
// stock car costs; nothing until it is set. For whoever may see what a car
// cost. Reading is Core's.
func InternalLabourRate(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) (int64, error) {
	if !scope.Role.SeesPurchasePrice() {
		return 0, access.ErrForbidden
	}
	var rate int64
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		return internalRateTx(ctx, tx, &rate)
	})
	return rate, err
}

func internalRateTx(ctx context.Context, tx pgx.Tx, rate *int64) error {
	err := tx.QueryRow(ctx, `SELECT internal_labour_rate_minor FROM stock_cost_settings ORDER BY seq DESC LIMIT 1`).Scan(rate)
	if errors.Is(err, pgx.ErrNoRows) {
		*rate = 0
		return nil
	}
	return err
}

// PostJobCost posts what a workshop job on a stock car cost the business,
// once: the parts fitted at what they cost when they left the shelf, and
// the job's labour hours at the internal rate -- not what a customer would
// be charged. The job must have been opened while the car was in stock,
// and be done: invoiced, closed or declined.
// Asked again, it returns the posting there is. For whoever buys cars, in a
// shop that sells cars; the job's own invoice is the workshop's matter.
func PostJobCost(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string) (StockCost, error) {
	if !scope.Role.BuysVehicles() {
		return StockCost{}, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return StockCost{}, err
	}
	if !looksLikeUUID(jobID) {
		return StockCost{}, ErrNotFound
	}
	var out StockCost
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('jobcost:' || $1::text, 0))`, jobID); err != nil {
			return err
		}
		var existing string
		err := tx.QueryRow(ctx, `SELECT id FROM stock_cost_entries WHERE work_order_id = $1 AND source = 'work_order'
			AND reverses_id IS NULL AND NOT EXISTS (SELECT 1 FROM stock_cost_entries r WHERE r.reverses_id = stock_cost_entries.id)`,
			jobID).Scan(&existing)
		if err == nil {
			out, err = stockCostTx(ctx, tx, existing)
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read the job's posting: %w", err)
		}
		var unit string
		err = tx.QueryRow(ctx, `
			SELECT u.id FROM work_orders w
			JOIN stock_units u ON u.vehicle_id = w.vehicle_id AND u.opened_at <= w.opened_at
			                  AND (u.closed_at IS NULL OR u.closed_at > w.opened_at)
			WHERE w.id = $1`, jobID).Scan(&unit)
		if errors.Is(err, pgx.ErrNoRows) {
			var job bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM work_orders WHERE id = $1)`, jobID).Scan(&job); err != nil {
				return err
			}
			if !job {
				return ErrNotFound
			}
			return errJobNotStock
		}
		if err != nil {
			return fmt.Errorf("read the job's car: %w", err)
		}
		// Done: invoiced, closed, or declined after work. An open job may
		// yet take parts and hours, and a posting is what the job cost, not
		// what it has cost so far; one that kept moving would change under
		// the margin it is read into. Ready is not done: a ready job may be
		// handed back for more work. The state is read FOR SHARE, so a job
		// cannot move back while its cost is posted.
		var state State
		if err := tx.QueryRow(ctx, `SELECT state FROM work_orders WHERE id = $1 FOR SHARE`, jobID).Scan(&state); err != nil {
			return fmt.Errorf("read the job: %w", err)
		}
		switch state {
		case StateInvoiced, StateClosed, StateDeclined:
		default:
			return errJobOpen
		}
		var rate int64
		if err := internalRateTx(ctx, tx, &rate); err != nil {
			return fmt.Errorf("read the internal rate: %w", err)
		}
		var parts, labour int64
		var number int64
		if err := tx.QueryRow(ctx, `
			SELECT w.number,
			       coalesce((SELECT round(sum(-m.quantity * coalesce(m.unit_cost_minor, 0)))::bigint FROM stock_movements m
			                 WHERE m.work_order_id = w.id AND m.kind IN ('consumed', 'put_back')), 0),
			       coalesce((SELECT round(sum(l.quantity) * $2)::bigint FROM work_order_lines l
			                 WHERE l.work_order_id = w.id AND l.kind = 'labour' AND l.withdrawn_at IS NULL AND l.declined_at IS NULL), 0)
			FROM work_orders w WHERE w.id = $1`, jobID, rate).Scan(&number, &parts, &labour); err != nil {
			return fmt.Errorf("work out the job's cost: %w", err)
		}
		net := parts + labour
		var id string
		if err := tx.QueryRow(ctx, `
			INSERT INTO stock_cost_entries (shop_id, stock_unit_id, kind, net_minor, input_vat_minor, vat_rate_basis,
			       incurred_on, source, work_order_id, happened_at, note, recorded_by)
			VALUES ($1, $2, 'workshop', $3, 0, 0, (clock_timestamp() AT TIME ZONE (SELECT timezone FROM shops WHERE id = $1))::date,
			        'work_order', $4, (SELECT opened_at FROM work_orders WHERE id = $4), $5, NULLIF($6, '')::uuid)
			RETURNING id`, scope.ShopID, unit, net, jobID,
			fmt.Sprintf("#%d", number), scope.UserID).Scan(&id); err != nil {
			return fmt.Errorf("post the job's cost: %w", err)
		}
		out, err = stockCostTx(ctx, tx, id)
		return err
	})
	return out, err
}
