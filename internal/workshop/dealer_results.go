package workshop

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/money"
)

// What each car sold earned (#182): the business margin, the owner's
// figure, beside and apart from the margin scheme's tax margin (#176).
// Read only by a role that may see margins (#145): the rows are read for
// that role and the totals built from them, nothing shared between roles.

// SaleResult is one car sold and what it earned.
type SaleResult struct {
	SaleID      string
	SaleNumber  int64
	Car         string
	Salesperson string
	InvoicedOn  time.Time
	DeliveredOn *time.Time
	// Days in stock until the sale was invoiced.
	StockDays int
	// Invoiced less every VAT on it, credits counted.
	NetRevenueMinor int64
	// What it was bought for; nil when no purchase is recorded (an import,
	// a consignment), and the result is then incomplete, not a profit.
	PurchaseMinor *int64
	// Costs before and after delivery.
	CostsAtDeliveryMinor int64
	CostsMinor           int64
	// What the purchase calculation set out to earn, when one was accepted.
	TargetMarginMinor *int64
}

// Complete reports whether the result can be worked out: it has a
// purchase price.
func (r SaleResult) Complete() bool { return r.PurchaseMinor != nil }

// MarginMinor is what the car earned now, every cost counted; zero when
// incomplete.
func (r SaleResult) MarginMinor() int64 {
	if !r.Complete() {
		return 0
	}
	return money.BusinessMargin(r.NetRevenueMinor, *r.PurchaseMinor, r.CostsMinor, 0)
}

// MarginAtDeliveryMinor is what it earned as delivered, before costs that
// came after (a complaint's repair, #181).
func (r SaleResult) MarginAtDeliveryMinor() int64 {
	if !r.Complete() {
		return 0
	}
	return money.BusinessMargin(r.NetRevenueMinor, *r.PurchaseMinor, r.CostsAtDeliveryMinor, 0)
}

// Results is the cars sold in a period, by invoice date, and their totals
// by salesperson and by month.
type Results struct {
	Rows          []SaleResult
	BySalesperson []ResultTotal
	ByMonth       []ResultTotal
	Incomplete    int
}

// ResultTotal is the complete results of one salesperson or month.
type ResultTotal struct {
	Key         string
	Cars        int
	MarginMinor int64
}

// SaleResults reads the cars sold -- invoiced, credits counted -- from from
// up to to, for whoever may see margins.
func SaleResults(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, from, to time.Time) (Results, error) {
	if !scope.Role.SeesMargin() {
		return Results{}, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return Results{}, err
	}
	var out Results
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			WITH sold AS (
			  SELECT s.id, s.number, s.stock_unit_id, s.salesperson_id, min(i.issued_at) AS invoiced_at,
			         sum(i.gross_minor - i.vat_minor - coalesce(i.margin_scheme_vat_minor, 0)) AS net
			  FROM vehicle_sales s JOIN invoices i ON i.vehicle_sale_id = s.id
			  GROUP BY s.id
			  HAVING min(i.issued_at) FILTER (WHERE i.credit_of_id IS NULL) >= $1
			     AND min(i.issued_at) FILTER (WHERE i.credit_of_id IS NULL) < $2)
			SELECT x.id, x.number, `+carNameSQL+`, coalesce(sp.display_name, ''), x.invoiced_at, dl.delivered_at,
			       (x.invoiced_at::date - u.opened_at::date), x.net, p.tax_purchase_price_minor,
			       coalesce((SELECT sum(c.net_minor) FROM stock_cost_entries c WHERE c.stock_unit_id = u.id
			                 AND (dl.delivered_at IS NULL OR c.happened_at <= dl.delivered_at)), 0),
			       coalesce((SELECT sum(c.net_minor) FROM stock_cost_entries c WHERE c.stock_unit_id = u.id), 0),
			       (SELECT k.target_margin_minor FROM purchase_calculation_acceptances a
			          JOIN purchase_calculations k ON k.id = a.calculation_version_id
			         WHERE k.vehicle_id = u.vehicle_id ORDER BY a.accepted_at DESC LIMIT 1)
			FROM sold x
			JOIN stock_units u ON u.id = x.stock_unit_id JOIN vehicles v ON v.id = u.vehicle_id
			LEFT JOIN vehicle_registrations reg ON reg.vehicle_id = v.id AND reg.valid_to IS NULL
			LEFT JOIN vehicle_purchases p ON p.stock_unit_id = u.id
			LEFT JOIN vehicle_sale_deliveries dl ON dl.sale_id = x.id
			LEFT JOIN users su ON su.id = x.salesperson_id LEFT JOIN people sp ON sp.id = su.person_id
			ORDER BY x.invoiced_at, x.number`, from, to)
		if err != nil {
			return fmt.Errorf("read results: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var r SaleResult
			var delivered *time.Time
			if err := rows.Scan(&r.SaleID, &r.SaleNumber, &r.Car, &r.Salesperson, &r.InvoicedOn, &delivered, &r.StockDays,
				&r.NetRevenueMinor, &r.PurchaseMinor, &r.CostsAtDeliveryMinor, &r.CostsMinor, &r.TargetMarginMinor); err != nil {
				return err
			}
			r.DeliveredOn = delivered
			out.Rows = append(out.Rows, r)
		}
		return rows.Err()
	})
	if err != nil {
		return Results{}, err
	}
	people, months := map[string]*ResultTotal{}, map[string]*ResultTotal{}
	add := func(m map[string]*ResultTotal, key string, r SaleResult) {
		t, ok := m[key]
		if !ok {
			t = &ResultTotal{Key: key}
			m[key] = t
		}
		t.Cars++
		t.MarginMinor += r.MarginMinor()
	}
	for _, r := range out.Rows {
		if !r.Complete() {
			out.Incomplete++
			continue
		}
		add(people, r.Salesperson, r)
		add(months, r.InvoicedOn.Format("2006-01"), r)
	}
	flatten := func(m map[string]*ResultTotal) []ResultTotal {
		var xs []ResultTotal
		for _, t := range m {
			xs = append(xs, *t)
		}
		sort.Slice(xs, func(i, j int) bool { return xs[i].Key < xs[j].Key })
		return xs
	}
	out.BySalesperson, out.ByMonth = flatten(people), flatten(months)
	return out, nil
}
