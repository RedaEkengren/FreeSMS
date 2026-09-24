package workshop

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LabourTime is one stored time for one operation.
type LabourTime struct {
	ID        string
	Operation string
	Make      string
	Model     string
	YearFrom  *int16
	YearTo    *int16
	Engine    string

	Minutes           int
	AdjustmentMinutes int
	Note              string

	// What the shop has actually clocked against this entry.
	Actuals ActualSpread
}

// TotalMinutes is the book time plus the shop's own adjustment.
func (l LabourTime) TotalMinutes() int { return l.Minutes + l.AdjustmentMinutes }

// Hours renders the total the way a workshop writes it.
func (l LabourTime) Hours() string { return fmt.Sprintf("%.2f", float64(l.TotalMinutes())/60) }

// Scope describes what this time applies to, for showing which rule matched.
func (l LabourTime) Scope() string {
	parts := []string{}
	if l.Make == "" {
		return "any vehicle"
	}
	parts = append(parts, l.Make)
	if l.Model != "" {
		parts = append(parts, l.Model)
	}
	if l.Engine != "" {
		parts = append(parts, l.Engine)
	}
	switch {
	case l.YearFrom != nil && l.YearTo != nil:
		parts = append(parts, fmt.Sprintf("%d–%d", *l.YearFrom, *l.YearTo))
	case l.YearFrom != nil:
		parts = append(parts, fmt.Sprintf("%d onwards", *l.YearFrom))
	case l.YearTo != nil:
		parts = append(parts, fmt.Sprintf("up to %d", *l.YearTo))
	}
	return strings.Join(parts, " ")
}

// specificity scores how narrowly a time is scoped, so that the narrowest
// match wins.
//
// Deterministic and visible, rather than "whichever the database returned
// first". A suggestion nobody can account for is one nobody trusts, and the
// matched rule is shown alongside it for the same reason.
func (l LabourTime) specificity() int {
	score := 0
	if l.Make != "" {
		score += 8
	}
	if l.Model != "" {
		score += 4
	}
	if l.Engine != "" {
		score += 2
	}
	if l.YearFrom != nil || l.YearTo != nil {
		score++
	}
	return score
}

// ActualSpread is what the shop really took, for the same operation.
//
// A single average hides the case this exists to catch: an entry that is
// wildly wrong looks fine as a mean of two jobs. The count and the range are
// what let somebody judge it.
type ActualSpread struct {
	Jobs           int
	MedianMinutes  int
	FastestMinutes int
	SlowestMinutes int
}

// Known reports whether there is anything to compare against yet.
func (a ActualSpread) Known() bool { return a.Jobs > 0 }

// MedianHours renders the middle of the real times.
func (a ActualSpread) MedianHours() string { return fmt.Sprintf("%.2f", float64(a.MedianMinutes)/60) }

// Suggestion is a stored time proposed for a job.
type Suggestion struct {
	LabourTime
	MatchedRule string
}

// SuggestFor returns the stored times that apply to a vehicle, narrowest match
// per operation.
func SuggestFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, vehicleID string) ([]Suggestion, error) {
	var out []Suggestion
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var make, model, engine string
		var year *int16
		err := tx.QueryRow(ctx,
			`SELECT coalesce(make, ''), coalesce(model, ''), coalesce(engine, ''), model_year
			 FROM vehicles WHERE id = $1`, vehicleID).Scan(&make, &model, &engine, &year)
		if err != nil {
			return fmt.Errorf("read vehicle: %w", err)
		}

		const q = `
			SELECT id, operation, coalesce(make, ''), coalesce(model, ''),
			       year_from, year_to, coalesce(engine, ''),
			       minutes, adjustment_minutes, coalesce(note, '')
			FROM labour_times
			WHERE active
			  AND (make   IS NULL OR make   = $1)
			  AND (model  IS NULL OR model  = $2)
			  AND (engine IS NULL OR engine = $3)
			  AND (year_from IS NULL OR ($4::smallint IS NOT NULL AND $4 >= year_from))
			  AND (year_to   IS NULL OR ($4::smallint IS NOT NULL AND $4 <= year_to))`
		rows, err := tx.Query(ctx, q, make, model, engine, year)
		if err != nil {
			return fmt.Errorf("suggest: %w", err)
		}
		defer rows.Close()

		var all []LabourTime
		for rows.Next() {
			var l LabourTime
			if err := rows.Scan(&l.ID, &l.Operation, &l.Make, &l.Model,
				&l.YearFrom, &l.YearTo, &l.Engine, &l.Minutes, &l.AdjustmentMinutes, &l.Note); err != nil {
				return fmt.Errorf("scan time: %w", err)
			}
			all = append(all, l)
		}
		if err := rows.Err(); err != nil {
			return err
		}

		// Narrowest per operation. Ties break on the identifier so that the
		// same shop and the same car always get the same answer.
		best := map[string]LabourTime{}
		for _, l := range all {
			cur, seen := best[l.Operation]
			if !seen || l.specificity() > cur.specificity() ||
				(l.specificity() == cur.specificity() && l.ID < cur.ID) {
				best[l.Operation] = l
			}
		}
		for _, l := range best {
			spread, err := actualsFor(ctx, tx, l.ID)
			if err != nil {
				return err
			}
			l.Actuals = spread
			out = append(out, Suggestion{LabourTime: l, MatchedRule: l.Scope()})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Operation < out[j].Operation })
		return nil
	})
	return out, err
}

// actualsFor reads what the shop really took against a stored time.
func actualsFor(ctx context.Context, tx pgx.Tx, labourTimeID string) (ActualSpread, error) {
	const q = `
		SELECT count(*),
		       coalesce(percentile_cont(0.5) WITHIN GROUP (ORDER BY minutes), 0),
		       coalesce(min(minutes), 0), coalesce(max(minutes), 0)
		FROM (
		    SELECT l.id,
		           sum(extract(epoch from (t.ended_at - t.started_at)) / 60) AS minutes
		    FROM work_order_lines l
		    JOIN time_entries t ON t.line_id = l.id AND t.ended_at IS NOT NULL
		    WHERE l.labour_time_id = $1
		    GROUP BY l.id
		) per_job`
	var a ActualSpread
	var median, fastest, slowest float64
	if err := tx.QueryRow(ctx, q, labourTimeID).Scan(&a.Jobs, &median, &fastest, &slowest); err != nil {
		return ActualSpread{}, fmt.Errorf("read actuals: %w", err)
	}
	a.MedianMinutes, a.FastestMinutes, a.SlowestMinutes = int(median), int(fastest), int(slowest)
	return a, nil
}

// LabourTimes lists the whole library.
func LabourTimes(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]LabourTime, error) {
	var out []LabourTime
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, operation, coalesce(make, ''), coalesce(model, ''),
			       year_from, year_to, coalesce(engine, ''),
			       minutes, adjustment_minutes, coalesce(note, '')
			FROM labour_times WHERE active
			ORDER BY operation, make NULLS FIRST, model NULLS FIRST`)
		if err != nil {
			return fmt.Errorf("list times: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var l LabourTime
			if err := rows.Scan(&l.ID, &l.Operation, &l.Make, &l.Model,
				&l.YearFrom, &l.YearTo, &l.Engine, &l.Minutes, &l.AdjustmentMinutes, &l.Note); err != nil {
				return fmt.Errorf("scan time: %w", err)
			}
			out = append(out, l)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range out {
			spread, err := actualsFor(ctx, tx, out[i].ID)
			if err != nil {
				return err
			}
			out[i].Actuals = spread
		}
		return nil
	})
	return out, err
}

// SaveLabourTime adds a time to the library.
func SaveLabourTime(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, l LabourTime) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if strings.TrimSpace(l.Operation) == "" {
		return fmt.Errorf("%w: the operation needs a name", ErrInvalid)
	}
	if l.Minutes <= 0 {
		return fmt.Errorf("%w: the time has to be more than nothing", ErrInvalid)
	}
	if l.Model != "" && l.Make == "" {
		return fmt.Errorf("%w: a model without a make is not a scope anybody can reason about", ErrInvalid)
	}

	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO labour_times
			  (shop_id, operation, make, model, year_from, year_to, engine,
			   minutes, adjustment_minutes, note)
			VALUES ($1, $2, nullif($3,''), nullif($4,''), $5, $6, nullif($7,''), $8, $9, nullif($10,''))`,
			scope.ShopID, strings.TrimSpace(l.Operation), l.Make, l.Model,
			l.YearFrom, l.YearTo, l.Engine, l.Minutes, l.AdjustmentMinutes, l.Note)
		if err != nil {
			return fmt.Errorf("save time: %w", err)
		}
		return nil
	})
}

// LabourRate reads the shop's hourly rate in minor units. Zero means it has
// not been set.
func LabourRate(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) (int64, error) {
	var rate int64
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT labour_rate_minor FROM shops WHERE id = $1`, scope.ShopID).Scan(&rate)
	})
	return rate, err
}

// SetLabourRate stores the shop's hourly rate.
func SetLabourRate(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, minor int64) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if minor < 0 {
		return fmt.Errorf("%w: a rate cannot be negative", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE shops SET labour_rate_minor = $1 WHERE id = $2`, minor, scope.ShopID)
		return err
	})
}
