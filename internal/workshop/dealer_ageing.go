package workshop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// Where a shop draws the lines on its stock list's ageing (#168). The
// thresholds are the days at which a car moves into the next bucket: 30, 60
// and 90 make 0-30, 31-60, 61-90 and 90+.
var defaultAgeing = []int{30, 60, 90}

var errAgeing = fmt.Errorf("%w: the ageing lines are one to six numbers of days, each more than the one before, at most ten years", ErrInvalid)

// ageingLabels names the buckets a set of thresholds makes.
func ageingLabels(th []int) []string {
	out := make([]string, 0, len(th)+1)
	from := 0
	for _, t := range th {
		out = append(out, strconv.Itoa(from)+"-"+strconv.Itoa(t))
		from = t + 1
	}
	return append(out, strconv.Itoa(th[len(th)-1])+"+")
}

// ageingBucket is the bucket a car of that many days is in. The first bucket
// starts at 0 and each later one the day after the line before it; the last
// is open ended.
func ageingBucket(days int, th []int) string {
	labels := ageingLabels(th)
	for i, t := range th {
		if days <= t {
			return labels[i]
		}
	}
	return labels[len(labels)-1]
}

// SetStockAgeing sets the lines. Whoever runs the shop, in a shop that
// sells cars.
func SetStockAgeing(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, thresholds []int) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return err
	}
	if len(thresholds) < 1 || len(thresholds) > 6 || thresholds[0] < 1 || thresholds[len(thresholds)-1] > 3650 {
		return errAgeing
	}
	for i := 1; i < len(thresholds); i++ {
		if thresholds[i] <= thresholds[i-1] {
			return errAgeing
		}
	}
	th := make([]int32, len(thresholds))
	for i, t := range thresholds {
		th[i] = int32(t)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO stock_ageing_settings (shop_id, thresholds, changed_by) VALUES ($1, $2, NULLIF($3, '')::uuid)`,
			scope.ShopID, th, scope.UserID)
		return err
	})
}

// StockAgeingOf reads the lines in force, as days, and the buckets they
// make. For whoever buys or sells cars.
func StockAgeingOf(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]int, []string, error) {
	if !scope.Role.BuysVehicles() && !scope.Role.SellsVehicles() {
		return nil, nil, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return nil, nil, err
	}
	var th []int
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		th, err = ageingTx(ctx, tx)
		return err
	})
	return th, ageingLabels(th), err
}

func ageingTx(ctx context.Context, tx pgx.Tx) ([]int, error) {
	var raw []int32
	err := tx.QueryRow(ctx, `SELECT thresholds FROM stock_ageing_settings ORDER BY changed_at DESC, id DESC LIMIT 1`).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return slices.Clone(defaultAgeing), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the ageing lines: %w", err)
	}
	out := make([]int, len(raw))
	for i, r := range raw {
		out[i] = int(r)
	}
	return out, nil
}

// StockFilter narrows and orders the stock list. The zero filter is the
// whole list, longest in stock first.
type StockFilter struct {
	// A bucket as the list names it ("31-60"); empty is every car.
	Bucket string
	// A status; empty is every status.
	Status StockStatus
	// "days" (longest in stock first, the default), "days_for_sale" (longest
	// on offer first, cars not yet for sale last) or "registration".
	SortBy string
}

func (f StockFilter) apply(rows []StockRow) []StockRow {
	out := rows[:0:0]
	for _, r := range rows {
		if (f.Bucket == "" || r.Bucket == f.Bucket) && (f.Status == "" || r.Status == f.Status) {
			out = append(out, r)
		}
	}
	switch f.SortBy {
	case "days_for_sale":
		sort.SliceStable(out, func(i, j int) bool {
			a, b := out[i].DaysForSale, out[j].DaysForSale
			switch {
			case a == nil:
				return false
			case b == nil:
				return true
			}
			return *a > *b
		})
	case "registration":
		sort.SliceStable(out, func(i, j int) bool { return out[i].Registration < out[j].Registration })
	default:
		sort.SliceStable(out, func(i, j int) bool { return out[i].DaysInStock > out[j].DaysInStock })
	}
	return out
}

// stockAgeingTx counts the cars still in the shop's hands that have been in
// stock longer than its oldest ageing line, in one statement, in the caller's
// transaction. A car sold and waiting for its delivery is not ageing stock;
// one on consignment is, for it is in the shop's yard. A count, never a
// car's cost.
func stockAgeingTx(ctx context.Context, tx pgx.Tx) ([]ActionItem, error) {
	var n int
	err := tx.QueryRow(ctx, `
		SELECT count(*) FROM stock_units u, shops s
		WHERE u.closed_at IS NULL AND u.status IN ('in_preparation', 'for_sale', 'reserved')
		  AND (clock_timestamp() AT TIME ZONE s.timezone)::date - (u.opened_at AT TIME ZONE s.timezone)::date >
		      coalesce((SELECT thresholds[cardinality(thresholds)] FROM stock_ageing_settings ORDER BY changed_at DESC, id DESC LIMIT 1), $1)`,
		defaultAgeing[len(defaultAgeing)-1]).Scan(&n)
	if err != nil || n == 0 {
		return nil, err
	}
	return []ActionItem{{Kind: "stock_ageing", Module: access.Dealer, SubjectKind: "stock_list", Count: n, Urgency: UrgencySoon}}, nil
}

// stockAgeingWaiting is the stock list's part of the action queue (#168) for
// whoever buys cars and does not sell them: whoever sells reads it in
// dealerWaiting's own transaction, which costs the page nothing more.
func stockAgeingWaiting(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]ActionItem, error) {
	if !scope.Role.BuysVehicles() || scope.Role.SellsVehicles() {
		return nil, nil
	}
	var out []ActionItem
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = stockAgeingTx(ctx, tx)
		return err
	})
	return out, err
}
