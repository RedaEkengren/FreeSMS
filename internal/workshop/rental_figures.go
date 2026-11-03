package workshop

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
)

// A rental unit's figures over a period (#203): how much of the time it
// was there to rent it was rented, what that earned, and how long it stood
// still. The definitions are these, and the functions below are them:
//
//   - Available: its time in the fleet within the period, less the days
//     the shop was closed -- but a closed day it was out on a rental was
//     not lost, so only the closed time it was not rented is taken off.
//   - Rented: from its actual check-out to its check-in, or to now while
//     it is out; never the times booked.
//   - Downtime: its workshop and service blocks.
//   - Buffers: the time held after each rental for cleaning; neither
//     rented nor downtime, shown on its own.
//   - Utilisation: rented over available, in tenths of a per cent.
//   - Revenue: the rent on its rentals' standing invoices, net of VAT and
//     spread over the rental's days, plus its other return charges in the
//     month they were invoiced. A credited invoice counts nothing. Damage
//     compensation is a figure of its own; congestion tax, fines and tolls
//     passed on are not revenue and are shown apart; deposits are no
//     revenue at all.
//
// Cost waits on #204: a line the shop bears costs it the part or the
// time, which no row records yet (its price is nothing by rule).

// UnitFigures are one unit's figures over a period.
type UnitFigures struct {
	UnitID      string
	Unit        string
	Category    string
	Available   time.Duration
	Rented      time.Duration
	Downtime    time.Duration
	Buffers     time.Duration
	RentMinor   int64
	ChargeMinor int64
	DamageMinor int64
	// Congestion tax, fines and tolls passed on: not revenue.
	PassedOnMinor int64
}

// Utilisation is rented over available in tenths of a per cent; 0 when
// it was not available at all.
func (f UnitFigures) Utilisation() int64 {
	if f.Available <= 0 {
		return 0
	}
	return int64(f.Rented) * 1000 / int64(f.Available)
}

// RevenueMinor is rent and the other return charges.
func (f UnitFigures) RevenueMinor() int64 { return f.RentMinor + f.ChargeMinor }

// span is a stretch of time, from inclusive to exclusive.
type span struct{ from, to time.Time }

// overlap is how much of a lies within b.
func overlap(a, b span) time.Duration {
	from, to := a.from, a.to
	if b.from.After(from) {
		from = b.from
	}
	if b.to.Before(to) {
		to = b.to
	}
	if !to.After(from) {
		return 0
	}
	return to.Sub(from)
}

// within is the total of every span's part within all of the limits: a
// span in a membership in the period, say. The spans are not to overlap
// one another, as a unit's rentals and blocks cannot.
func within(spans []span, limits ...[]span) time.Duration {
	var total time.Duration
	for _, s := range spans {
		parts := []span{s}
		for _, lim := range limits {
			var next []span
			for _, p := range parts {
				for _, l := range lim {
					if d := overlap(p, l); d > 0 {
						from, to := p.from, p.to
						if l.from.After(from) {
							from = l.from
						}
						if l.to.Before(to) {
							to = l.to
						}
						next = append(next, span{from, to})
					}
				}
			}
			parts = next
		}
		for _, p := range parts {
			total += p.to.Sub(p.from)
		}
	}
	return total
}

// share is amount spread over whole: the part that part of it is, rounded
// half away from zero, never in floats.
func share(amount int64, part, whole time.Duration) int64 {
	if whole <= 0 {
		return 0
	}
	p, w := int64(part/time.Second), int64(whole/time.Second)
	if w == 0 {
		return 0
	}
	n := amount * p
	if n < 0 {
		return -((-n + w/2) / w)
	}
	return (n + w/2) / w
}

// figuresFor computes a unit's time figures from its spans, the period
// and the shop's closed days.
func figuresFor(period span, memberships, rentals, downtime, buffers, closed []span) (available, rented, down, held time.Duration) {
	in := []span{period}
	rented = within(rentals, memberships, in)
	down = within(downtime, memberships, in)
	held = within(buffers, memberships, in)
	lost := within(closed, memberships, in) - within(closed, rentals, memberships, in)
	available = within(memberships, in) - lost
	return available, rented, down, held
}

// RentalFigures are every unit's figures over a period, for whoever runs
// the shop. Units that were in the fleet at no time in it are left out.
func RentalFigures(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, from, to time.Time) ([]UnitFigures, error) {
	if !scope.Role.RunsTheShop() {
		return nil, access.ErrForbidden
	}
	if !to.After(from) {
		return nil, fmt.Errorf("%w: a period ends after it starts", ErrInvalid)
	}
	now := time.Now()
	period := span{from, to}
	var out []UnitFigures
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		spans := func(sql string, args ...any) (map[string][]span, error) {
			rows, err := tx.Query(ctx, sql, args...)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			m := map[string][]span{}
			for rows.Next() {
				var id string
				var a time.Time
				var b *time.Time
				if err := rows.Scan(&id, &a, &b); err != nil {
					return nil, err
				}
				end := now
				if b != nil && b.Before(now) {
					end = *b
				}
				if end.After(a) {
					m[id] = append(m[id], span{a, end})
				}
			}
			return m, rows.Err()
		}
		members, err := spans(`SELECT unit_id::text, joined_at, left_at FROM fleet_memberships
			WHERE joined_at < $2 AND (left_at IS NULL OR left_at > $1)`, from, to)
		if err != nil {
			return fmt.Errorf("read the fleet: %w", err)
		}
		rented, err := spans(`
			SELECT b.unit_id::text, o.at, i.at FROM rental_bookings b
			JOIN rental_handovers o ON o.booking_id = b.id AND o.kind = 'check_out'
			LEFT JOIN rental_handovers i ON i.booking_id = b.id AND i.kind = 'check_in'
			WHERE o.at < $2 AND (i.at IS NULL OR i.at > $1)`, from, to)
		if err != nil {
			return fmt.Errorf("read the rentals: %w", err)
		}
		down, err := spans(`
			SELECT u.id::text, lower(k.period), upper(k.period) FROM resource_blocks k JOIN rental_units u ON u.resource_id = k.resource_id
			WHERE k.kind IN ('workshop', 'service') AND k.status = 'confirmed' AND k.period && tstzrange($1, $2)`, from, to)
		if err != nil {
			return fmt.Errorf("read the downtime: %w", err)
		}
		held, err := spans(`
			SELECT u.id::text, upper(k.period), upper(k.period) + k.buffer_after FROM resource_blocks k JOIN rental_units u ON u.resource_id = k.resource_id
			WHERE k.kind = 'rental' AND k.status = 'confirmed' AND k.buffer_after > '0' AND NOT upper_inf(k.period)
			  AND upper(k.period) < $2 AND upper(k.period) + k.buffer_after > $1`, from, to)
		if err != nil {
			return fmt.Errorf("read the buffers: %w", err)
		}
		var closed []span
		rows, err := tx.Query(ctx, `
			SELECT (day::timestamp AT TIME ZONE s.timezone), ((day + 1)::timestamp AT TIME ZONE s.timezone)
			FROM shop_closures c CROSS JOIN shops s WHERE s.id = c.shop_id
			  AND (day + 1)::timestamp AT TIME ZONE s.timezone > $1 AND day::timestamp AT TIME ZONE s.timezone < $2`, from, to)
		if err != nil {
			return fmt.Errorf("read the closed days: %w", err)
		}
		for rows.Next() {
			var s span
			if err := rows.Scan(&s.from, &s.to); err != nil {
				rows.Close()
				return err
			}
			closed = append(closed, s)
		}
		rows.Close()

		units := map[string]*UnitFigures{}
		rows, err = tx.Query(ctx, `SELECT id::text, fleet_number || coalesce(' · ' || (SELECT name FROM resources r WHERE r.id = u.resource_id), ''),
			coalesce(category, '') FROM rental_units u`)
		if err != nil {
			return fmt.Errorf("read the units: %w", err)
		}
		for rows.Next() {
			f := &UnitFigures{}
			if err := rows.Scan(&f.UnitID, &f.Unit, &f.Category); err != nil {
				rows.Close()
				return err
			}
			units[f.UnitID] = f
		}
		rows.Close()
		for id, f := range units {
			if len(members[id]) == 0 {
				delete(units, id)
				continue
			}
			f.Available, f.Rented, f.Downtime, f.Buffers = figuresFor(period, members[id], rented[id], down[id], held[id], closed)
		}

		// The rent on standing invoices, per rental, spread over its days.
		rows, err = tx.Query(ctx, `
			SELECT b.unit_id::text, o.at, i.at, l.quantity_milli, l.unit_price_minor
			FROM rental_invoice_items x
			JOIN invoices v ON v.id = x.invoice_id AND NOT EXISTS (SELECT 1 FROM invoices c WHERE c.credit_of_id = v.id)
			JOIN rental_booking_lines l ON l.id = x.booking_line_id
			JOIN rental_bookings b ON b.id = l.booking_id
			JOIN rental_handovers o ON o.booking_id = b.id AND o.kind = 'check_out'
			LEFT JOIN rental_handovers i ON i.booking_id = b.id AND i.kind = 'check_in'
			WHERE o.at < $2 AND (i.at IS NULL OR i.at > $1)`, from, to)
		if err != nil {
			return fmt.Errorf("read the rent: %w", err)
		}
		for rows.Next() {
			var unit string
			var out time.Time
			var in *time.Time
			var qty, price int64
			if err := rows.Scan(&unit, &out, &in, &qty, &price); err != nil {
				rows.Close()
				return err
			}
			f := units[unit]
			if f == nil {
				continue
			}
			end := now
			if in != nil {
				end = *in
			}
			whole := span{out, end}
			f.RentMinor += share(RentalCharge{QuantityMilli: qty, UnitPriceMinor: &price}.Net(), overlap(whole, period), whole.to.Sub(whole.from))
		}
		rows.Close()

		// Return charges on standing invoices issued in the period.
		rows, err = tx.Query(ctx, `
			SELECT b.unit_id::text, r.kind, r.quantity_milli, r.unit_price_minor, r.gross_minor, r.vat_rate_bp
			FROM rental_invoice_items x
			JOIN invoices v ON v.id = x.invoice_id AND NOT EXISTS (SELECT 1 FROM invoices c WHERE c.credit_of_id = v.id)
			JOIN rental_charges r ON r.id = x.charge_id
			JOIN rental_bookings b ON b.id = r.booking_id
			WHERE v.issued_at >= $1 AND v.issued_at < $2`, from, to)
		if err != nil {
			return fmt.Errorf("read the charges: %w", err)
		}
		for rows.Next() {
			var unit string
			c := RentalCharge{}
			if err := rows.Scan(&unit, &c.Kind, &c.QuantityMilli, &c.UnitPriceMinor, &c.GrossGiven, &c.VATRateBp); err != nil {
				rows.Close()
				return err
			}
			f := units[unit]
			if f == nil {
				continue
			}
			switch c.Kind {
			case ChargeDamage:
				f.DamageMinor += c.Net()
			case ChargeCongestionTax, ChargeParkingTolls:
				f.PassedOnMinor += c.Net()
			default:
				f.ChargeMinor += c.Net()
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, f := range units {
			out = append(out, *f)
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Category != out[j].Category {
				return out[i].Category < out[j].Category
			}
			return out[i].Unit < out[j].Unit
		})
		return nil
	})
	return out, err
}

// CategoryTotals adds units' figures up per category, in the order given.
func CategoryTotals(units []UnitFigures) []UnitFigures {
	var out []UnitFigures
	index := map[string]int{}
	for _, u := range units {
		i, ok := index[u.Category]
		if !ok {
			i = len(out)
			index[u.Category] = i
			out = append(out, UnitFigures{Category: u.Category, Unit: u.Category})
		}
		t := &out[i]
		t.Available += u.Available
		t.Rented += u.Rented
		t.Downtime += u.Downtime
		t.Buffers += u.Buffers
		t.RentMinor += u.RentMinor
		t.ChargeMinor += u.ChargeMinor
		t.DamageMinor += u.DamageMinor
		t.PassedOnMinor += u.PassedOnMinor
	}
	return out
}
