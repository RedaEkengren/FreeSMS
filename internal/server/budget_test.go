package server

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/testsupport"
)

// "Slow" is the commonest complaint about every system FreeSMS is meant to
// replace, and speed does not go in one commit: it goes a query at a time,
// usually as a page built from a loop over rows, and nobody notices until a
// shop with four years of history opens it. This counts what each page asks
// of the database -- round trips, not milliseconds, which on a CI runner are
// noise -- and holds it to two things: the count must not grow with the
// shop's history, and it must stay within the page's budget below.
//
// A budget raised whenever it fails is a comment. Raising one is a decision,
// made in the commit that does it, with the reason in the message.
var queryBudgets = map[string]int{
	// The technician's screens, on the worst connection: these matter most.
	"tech GET /":                         11,
	"tech GET /jobs/{id}":                21,
	"tech GET /time":                     11,
	"tech GET /inspections/{inspection}": 12,
	"tech GET /mine":                     12,
	"tech GET /mine/count":               13,
	// The job page reads in one transaction (database.Reading). It was 57
	// and 100 when every read began and ended its own.
	//
	// The counter.
	"desk GET /board":       16,
	"desk GET /jobs/{id}":   32,
	"desk GET /receivables": 11,
	"desk GET /parts":       11,
	"desk GET /stock":       26,
	"desk GET /calendar":    29,
	"desk GET /mine":        13,
	"desk GET /mine/count":  14,
	// The calendar reads the rota for its lanes (#100): six more, once,
	// whatever the history. "For me" asks whether the schedule changed: one.
	//
	// An htmx swap is the fast path: one block must not cost a page.
	"desk GET /jobs/{id}/parts (htmx)": 13,
}

// queryCounter counts every query any connection sends while it is on.
type queryCounter struct {
	on    atomic.Bool
	count atomic.Int64
}

func (c *queryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	if c.on.Load() {
		c.count.Add(1)
	}
	return ctx
}

func (c *queryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestPagesStayWithinTheirQueryBudgetWhateverTheHistory(t *testing.T) {
	counter := &queryCounter{}
	ts, pool := testServerOn(t, testsupport.FreshPoolWith(t, counter))
	tech := signIn(t, ts, techEmail)
	desk := signIn(t, ts, advisorEmail)

	measure := func() map[string]int {
		t.Helper()
		out := map[string]int{}
		for key := range queryBudgets {
			who, rest, _ := strings.Cut(key, " ")
			method, path, _ := strings.Cut(rest, " ")
			htmx := strings.HasSuffix(path, " (htmx)")
			path = strings.TrimSuffix(path, " (htmx)")
			client := tech
			if who == "desk" {
				client = desk
			}
			path = strings.ReplaceAll(strings.ReplaceAll(path, "{id}", jobA), "{inspection}", budgetInspection)
			req, _ := http.NewRequest(method, ts.URL+path, nil)
			if htmx {
				req.Header.Set("HX-Request", "true")
			}
			counter.count.Store(0)
			counter.on.Store(true)
			resp, err := client.Do(req)
			counter.on.Store(false)
			if err != nil {
				t.Fatalf("%s: %v", key, err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s answered %d", key, resp.StatusCode)
			}
			out[key] = int(counter.count.Load())
		}
		return out
	}

	seedInspection(t, pool)
	small := measure()
	seedHistory(t, pool)
	large := measure()

	keys := make([]string, 0, len(queryBudgets))
	for k := range queryBudgets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("%-38s %3d queries with a small shop, %3d with years of history (budget %d)", k, small[k], large[k], queryBudgets[k])
		if large[k] != small[k] {
			t.Errorf("%s: %d queries with a small shop and %d with history -- it grows with the data, which is a loop over rows",
				k, small[k], large[k])
		}
		if large[k] > queryBudgets[k] {
			t.Errorf("%s: %d queries, over its budget of %d", k, large[k], queryBudgets[k])
		}
	}
}

const budgetInspection = "eeeeeeee-0000-0000-0000-000000000001"

// seedInspection gives shop A's job one inspection with one item, so the
// inspection page has something to show before history piles onto it.
func seedInspection(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedShop(t, pool, shopA, []string{
		`INSERT INTO inspections (id, shop_id, work_order_id, template_name, performed_by)
		 VALUES ('` + budgetInspection + `', '` + shopA + `', '` + jobA + `', 'Vårservice', 'aaaaaaaa-0000-0000-0000-000000000002')`,
		`INSERT INTO inspection_items (shop_id, inspection_id, position, label, status)
		 VALUES ('` + shopA + `', '` + budgetInspection + `', 1, 'Punkt 1', 'pass')`,
	})
}

// seedHistory gives shop A years of work: three hundred jobs with lines,
// clocked time, findings, part requests and where the car was, and shop A's
// own job a long history of its own. Enough that a query looping over rows
// shows up as a different count.
func seedHistory(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	const tech = "aaaaaaaa-0000-0000-0000-000000000002"
	seedShop(t, pool, shopA, []string{
		// Three hundred cars and their jobs, open in the three states a
		// board shows.
		`INSERT INTO vehicles (id, shop_id, make, model)
		 SELECT ('cccccccc-0000-0000-0000-' || lpad(g::text, 12, '0'))::uuid, '` + shopA + `', 'Volvo', 'V' || g
		 FROM generate_series(1, 300) g`,
		`INSERT INTO work_orders (id, shop_id, number, vehicle_id, customer_id, complaint, state)
		 SELECT ('dddddddd-0000-0000-0000-' || lpad(g::text, 12, '0'))::uuid, '` + shopA + `', 100 + g,
		        ('cccccccc-0000-0000-0000-' || lpad(g::text, 12, '0'))::uuid,
		        'aaaaaaaa-0000-0000-0000-000000000004', 'Service',
		        (ARRAY['in_progress', 'awaiting_parts', 'approved'])[1 + g % 3]
		 FROM generate_series(1, 300) g`,
		// Five lines each, and shop A's own job forty.
		`INSERT INTO work_order_lines (shop_id, work_order_id, position, kind, description, quantity, unit_price_minor, vat_rate_bp)
		 SELECT '` + shopA + `', w.id, n, 'labour', 'Arbete ' || n, 1, 89500, 2500
		 FROM work_orders w CROSS JOIN generate_series(1, 5) n WHERE w.number > 100`,
		`INSERT INTO work_order_lines (shop_id, work_order_id, position, kind, description, quantity, unit_price_minor, vat_rate_bp)
		 SELECT '` + shopA + `', '` + jobA + `', 100 + n, 'labour', 'Arbete ' || n, 1, 89500, 2500
		 FROM generate_series(1, 40) n`,
		// Clocked time, two entries a job and twenty on shop A's.
		`INSERT INTO time_entries (shop_id, work_order_id, user_id, started_at, ended_at)
		 SELECT '` + shopA + `', w.id, '` + tech + `', now() - interval '3 hours' * n, now() - interval '3 hours' * n + interval '1 hour'
		 FROM work_orders w CROSS JOIN generate_series(1, 2) n`,
		`INSERT INTO time_entries (shop_id, work_order_id, user_id, started_at, ended_at)
		 SELECT '` + shopA + `', '` + jobA + `', '` + tech + `', now() - interval '1 day' * n, now() - interval '1 day' * n + interval '2 hours'
		 FROM generate_series(1, 20) n`,
		// Findings, part requests with deliveries, and where the car was.
		`INSERT INTO findings (shop_id, work_order_id, note, reported_by)
		 SELECT '` + shopA + `', w.id, 'Något mer ' || n, '` + tech + `'
		 FROM work_orders w CROSS JOIN generate_series(1, 3) n`,
		`INSERT INTO part_requests (shop_id, work_order_id, description, requested_by, quantity)
		 SELECT '` + shopA + `', w.id, 'Del ' || n, '` + tech + `', 4
		 FROM work_orders w CROSS JOIN generate_series(1, 3) n`,
		`INSERT INTO part_request_receipts (shop_id, request_id, quantity, source, received_by)
		 SELECT '` + shopA + `', pr.id, 1, 'delivery', '` + tech + `' FROM part_requests pr`,
		`INSERT INTO vehicle_presence (shop_id, work_order_id, event)
		 SELECT '` + shopA + `', w.id, e
		 FROM work_orders w CROSS JOIN unnest(ARRAY['left', 'collected', 'returned']) e`,
		// Inspections with photographs: thirty items on the measured one,
		// and four more on the job with ten each.
		`INSERT INTO inspection_items (shop_id, inspection_id, position, label, status, note)
		 SELECT '` + shopA + `', '` + budgetInspection + `', 1 + n, 'Punkt ' || n, 'fail', 'Slitet'
		 FROM generate_series(1, 30) n`,
		`INSERT INTO inspections (shop_id, work_order_id, template_name, performed_by, completed_at)
		 SELECT '` + shopA + `', '` + jobA + `', 'Kontroll ' || n, '` + tech + `', now()
		 FROM generate_series(1, 4) n`,
		`INSERT INTO inspection_items (shop_id, inspection_id, position, label, status)
		 SELECT '` + shopA + `', i.id, n, 'Punkt ' || n, 'attention'
		 FROM inspections i CROSS JOIN generate_series(1, 10) n
		 WHERE i.work_order_id = '` + jobA + `' AND i.id <> '` + budgetInspection + `'`,
		`INSERT INTO attachments (shop_id, work_order_id, inspection_item_id, storage_key, content_type, byte_size, uploaded_by)
		 SELECT '` + shopA + `', '` + jobA + `', it.id, 'budget-' || it.id, 'image/jpeg', 100, '` + tech + `'
		 FROM inspection_items it JOIN inspections i ON i.id = it.inspection_id
		 WHERE i.work_order_id = '` + jobA + `'`,
		// A catalogue with a history: fifty parts, each received and used.
		`INSERT INTO parts (shop_id, number, name, unit, price_minor)
		 SELECT '` + shopA + `', 'P-' || n, 'Del ' || n, 'each', 10000 FROM generate_series(1, 50) n`,
		`INSERT INTO stock_movements (shop_id, part_id, kind, quantity)
		 SELECT '` + shopA + `', p.id, k, q
		 FROM parts p CROSS JOIN (VALUES ('received', 10), ('consumed', -2), ('counted', 1)) AS m(k, q)`,
		// This week on the planner: sixty bookings, the technician away on
		// the Monday and the shop shut on the Friday.
		`INSERT INTO bookings (shop_id, starts_at, ends_at, technician_id, registration, what, created_by)
		 SELECT '` + shopA + `', date_trunc('week', now()) + interval '1 day' * (n % 5) + interval '8 hours' + interval '30 minutes' * (n % 12),
		        date_trunc('week', now()) + interval '1 day' * (n % 5) + interval '9 hours' + interval '30 minutes' * (n % 12),
		        CASE WHEN n % 2 = 0 THEN '` + tech + `'::uuid END, 'ABC ' || (100 + n), 'Service', '` + tech + `'
		 FROM generate_series(1, 60) n`,
		`INSERT INTO staff_absences (shop_id, user_id, day, reason, created_by)
		 VALUES ('` + shopA + `', '` + tech + `', date_trunc('week', now())::date, 'sick', '` + tech + `')`,
		`INSERT INTO shop_closures (shop_id, day, reason, created_by)
		 VALUES ('` + shopA + `', date_trunc('week', now())::date + 4, 'Inventering', '` + tech + `')`,
		// Two years of rotas, a new one every month, and a change most days.
		`INSERT INTO rotas (shop_id, user_id, valid_from, weeks, created_by)
		 SELECT '` + shopA + `', '` + tech + `', current_date - interval '1 month' * n, 2, '` + tech + `'
		 FROM generate_series(0, 24) n`,
		`INSERT INTO rota_hours (shop_id, rota_id, week, weekday, starts, ends)
		 SELECT '` + shopA + `', r.id, w, d, '07:00', '16:00'
		 FROM rotas r CROSS JOIN generate_series(0, 1) w CROSS JOIN generate_series(1, 5) d`,
		`INSERT INTO shift_changes (shop_id, user_id, day, starts, ends, created_by)
		 SELECT '` + shopA + `', '` + tech + `', current_date - n, '10:00', '18:00', '` + tech + `'
		 FROM generate_series(-14, 700) n`,
	})
}
