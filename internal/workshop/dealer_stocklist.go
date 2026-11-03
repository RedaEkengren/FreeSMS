package workshop

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// The stock list (#168): every car in stock, how long it has been there and,
// for whoever may see what a car cost (#145), what it has cost so far.

// StockRow is one car on the stock list.
type StockRow struct {
	UnitID       string
	VehicleID    string
	Registration string
	VIN          string
	Make, Model  string
	ModelYear    *int
	OdometerKm   *int64
	Status       StockStatus
	Reason       string
	OpenedAt     time.Time
	// Days in stock, counted in the shop's own days: a car bought today
	// has been in stock 0 days.
	DaysInStock int
	// Days since it was first offered for sale in this cycle; nil before.
	DaysForSale *int
	// What it cost, for a role that may see it; nil for any other, and
	// never read from the database for one.
	Figures *StockFigures
}

// StockFigures is what a car in stock has cost the business so far.
type StockFigures struct {
	// The tax purchase price; nil when none is recorded yet (an import, a
	// consignment), shown as missing rather than as nothing.
	PurchaseMinor *int64
	// Costs recorded before the sale (#164), reversals netted.
	CostsMinor int64
}

// ValueMinor is the car's stock value: purchase price and costs. Nil when
// the purchase price is missing, and for a consigned car, which is never
// the business's to value.
func (r StockRow) ValueMinor() *int64 {
	if r.Figures == nil || r.Figures.PurchaseMinor == nil || r.Consigned() {
		return nil
	}
	v := *r.Figures.PurchaseMinor + r.Figures.CostsMinor
	return &v
}

// Km is the latest odometer reading, 0 when there is none.
func (r StockRow) Km() int64 {
	if r.OdometerKm == nil {
		return 0
	}
	return *r.OdometerKm
}

// Consigned reports whether the car is sold on a principal's behalf.
func (r StockRow) Consigned() bool { return r.Reason == StockFromConsignment }

// StatusLabel is the car's status in words, a catalogue key.
func (r StockRow) StatusLabel() string { return r.Status.Label() }

// Ageing is which of the default ageing buckets the car is in, by days in
// stock: "0-30", "31-60", "61-90" or "90+".
func (r StockRow) Ageing() string {
	switch d := r.DaysInStock; {
	case d <= 30:
		return "0-30"
	case d <= 60:
		return "31-60"
	case d <= 90:
		return "61-90"
	default:
		return "90+"
	}
}

// StockList is every open cycle in stock, longest in stock first (the
// cars that need attention), with a
// total value for whoever may see what cars cost. One statement, whatever
// the number of cars. For whoever buys or sells cars, in a shop that sells
// them.
type StockList struct {
	Rows []StockRow
	// The sum of every value the list can give; nil for a role that may
	// not see it. Cars with a missing purchase price are counted apart.
	TotalValueMinor *int64
	MissingValue    int
}

// Stock reads the stock list.
func Stock(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) (StockList, error) {
	if !scope.Role.BuysVehicles() && !scope.Role.SellsVehicles() {
		return StockList{}, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return StockList{}, err
	}
	figures := scope.Role.SeesPurchasePrice()
	var out StockList
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		// $1 decides whether the figures are read at all: for a role that
		// may not see them the columns are NULL in the statement itself.
		rows, err := tx.Query(ctx, `
			WITH today AS (SELECT (clock_timestamp() AT TIME ZONE timezone)::date AS d, timezone AS tz FROM shops)
			SELECT u.id, u.vehicle_id, coalesce(r.registration, ''), coalesce(v.vin, ''), coalesce(v.make, ''), coalesce(v.model, ''),
			       v.model_year,
			       (SELECT o.km FROM odometer_readings o WHERE o.vehicle_id = u.vehicle_id ORDER BY o.read_at DESC LIMIT 1),
			       u.status, u.reason, u.opened_at,
			       t.d - (u.opened_at AT TIME ZONE t.tz)::date,
			       t.d - (u.first_for_sale_at AT TIME ZONE t.tz)::date,
			       CASE WHEN $1 THEN p.tax_purchase_price_minor END,
			       CASE WHEN $1 THEN coalesce((SELECT sum(c.net_minor) FROM stock_cost_entries c
			                                   WHERE c.stock_unit_id = u.id AND NOT stock_cost_post_sale(u.id, c.happened_at)), 0) END
			FROM stock_units u
			CROSS JOIN today t
			JOIN vehicles v ON v.id = u.vehicle_id
			LEFT JOIN vehicle_registrations r ON r.vehicle_id = u.vehicle_id AND r.valid_to IS NULL
			LEFT JOIN vehicle_purchases p ON p.stock_unit_id = u.id
			WHERE u.closed_at IS NULL
			ORDER BY u.opened_at, u.id`, figures)
		if err != nil {
			return fmt.Errorf("read stock: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var row StockRow
			var year *int16
			var purchase, costs *int64
			var forSale *int32
			if err := rows.Scan(&row.UnitID, &row.VehicleID, &row.Registration, &row.VIN, &row.Make, &row.Model, &year,
				&row.OdometerKm, &row.Status, &row.Reason, &row.OpenedAt, &row.DaysInStock, &forSale, &purchase, &costs); err != nil {
				return fmt.Errorf("scan stock: %w", err)
			}
			if year != nil {
				y := int(*year)
				row.ModelYear = &y
			}
			if forSale != nil {
				d := int(*forSale)
				row.DaysForSale = &d
			}
			if figures {
				row.Figures = &StockFigures{PurchaseMinor: purchase}
				if costs != nil {
					row.Figures.CostsMinor = *costs
				}
			}
			out.Rows = append(out.Rows, row)
		}
		return rows.Err()
	})
	if err != nil || !figures {
		return out, err
	}
	var total int64
	for _, r := range out.Rows {
		switch v := r.ValueMinor(); {
		case v != nil:
			total += *v
		case !r.Consigned():
			out.MissingValue++
		}
	}
	out.TotalValueMinor = &total
	return out, nil
}
