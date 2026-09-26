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

// SearchResult is one hit from the single search field.
type SearchResult struct {
	VehicleID    string
	Registration string
	VIN          string
	Make         string
	Model        string
	ModelYear    *int16
	Label        string

	// Why this matched, so a person can see that searching a name found a
	// car rather than wondering.
	MatchedOn string
	OwnerName string
}

// Describe is what to show as the heading for a vehicle that may have no
// plate at all.
func (s SearchResult) Describe() string {
	switch {
	case s.Registration != "":
		return s.Registration
	case s.VIN != "":
		return s.VIN
	case s.Label != "":
		return s.Label
	}
	return "unidentified vehicle"
}

// Search finds a vehicle by registration, VIN, or the name of whoever owns
// it.
//
// One field, because a person at a counter has one thing in their hand: a
// registration, a chassis number off a plate, or a name on the telephone.
// Asking them which kind it is before they can type is a question the
// software can answer itself.
func Search(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, query string) ([]SearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	normalised := NormaliseRegistration(query)

	var out []SearchResult
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		// Names are searched only for roles that may see them at all. A
		// technician typing a customer's name gets nothing, which is correct
		// and is also why the query is built this way rather than filtered
		// afterwards.
		// Matching looks at every plate the vehicle has ever carried, not only
		// the current one. Somebody reading an old service book has the
		// previous number in front of them, and that is exactly the moment
		// they need to find the car. What is *displayed* is the current plate,
		// so the result is not confusing.
		const q = `
			SELECT DISTINCT ON (v.id)
			       v.id, coalesce(cur.registration, ''), coalesce(v.vin, ''),
			       coalesce(v.make, ''), coalesce(v.model, ''), v.model_year,
			       coalesce(v.label, ''),
			       CASE
			         WHEN m.normalised = $1 AND m.valid_to IS NULL THEN 'registration'
			         WHEN m.normalised = $1 THEN 'a plate it used to carry'
			         WHEN $1 <> '' AND upper(coalesce(v.vin, '')) LIKE '%' || $1 || '%' THEN 'chassis number'
			         WHEN v.label ILIKE '%' || $2 || '%' THEN 'description'
			         ELSE 'owner'
			       END,
			       CASE WHEN $4 THEN coalesce(cp.display_name, c.company_name, '') ELSE '' END
			FROM vehicles v
			LEFT JOIN vehicle_registrations m ON m.vehicle_id = v.id
			LEFT JOIN vehicle_registrations cur
			       ON cur.vehicle_id = v.id AND cur.valid_to IS NULL
			LEFT JOIN vehicle_ownership o
			       ON o.vehicle_id = v.id AND o.owned_to IS NULL
			LEFT JOIN customers c ON c.id = o.customer_id
			LEFT JOIN people cp   ON cp.id = c.person_id
			WHERE ($1 <> '' AND m.normalised = $1)
			   OR ($1 <> '' AND upper(coalesce(v.vin, '')) LIKE '%' || $1 || '%')
			   OR ($4 AND (cp.display_name ILIKE '%' || $2 || '%'
			            OR c.company_name ILIKE '%' || $2 || '%'))
			   OR ($2 <> '' AND v.label ILIKE '%' || $2 || '%')
			-- A vehicle that matches on its current plate sorts above one that
			-- matches on a plate it gave up, because the current holder is
			-- almost always the one being looked for.
			ORDER BY v.id, (m.valid_to IS NULL) DESC NULLS LAST, m.valid_from DESC NULLS LAST
			LIMIT $3`
		rows, err := tx.Query(ctx, q, normalised, query, 25, scope.Role.SeesCustomerPersonalData())
		if err != nil {
			return fmt.Errorf("search: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var s SearchResult
			if err := rows.Scan(&s.VehicleID, &s.Registration, &s.VIN, &s.Make, &s.Model,
				&s.ModelYear, &s.Label, &s.MatchedOn, &s.OwnerName); err != nil {
				return fmt.Errorf("scan result: %w", err)
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	return out, err
}

// Vehicle is everything known about one car.
type Vehicle struct {
	ID           string
	Registration string
	VIN          string
	Make         string
	Model        string
	ModelYear    *int16
	Engine       string
	Label        string

	OwnerName     string
	OwnedSince    *time.Time
	History       []HistoryEntry
	Readings      []Reading
	Registrations []RegistrationPeriod
}

// Describe is the heading for a vehicle that may have no plate.
func (v Vehicle) Describe() string {
	switch {
	case v.Registration != "":
		return v.Registration
	case v.VIN != "":
		return v.VIN
	case v.Label != "":
		return v.Label
	}
	return "unidentified vehicle"
}

// HistoryEntry is one job in the vehicle's past.
//
// The technical part -- what was done, when, at what mileage -- belongs to the
// vehicle and survives a sale. Who paid and what they paid does not.
type HistoryEntry struct {
	WorkOrderID string
	Number      int64
	State       string
	OpenedAt    time.Time
	Complaint   string
	Lines       []string
	OdometerKm  *int64

	// PreviousOwner is true when the job happened before the current owner
	// bought the car. The customer's name and the money are then withheld,
	// because showing a new owner what the last one was charged is not the
	// workshop's to give away.
	PreviousOwner bool
	CustomerName  string
	GrossMinor    int64
	HasInvoice    bool
}

// StateLabel is the condition the job was left in, as a catalogue key.
func (h HistoryEntry) StateLabel() string { return State(h.State).Label() }

// TotalMinor is what the job came to, and nil where the viewer may not see
// it: a previous owner's bill is not the new owner's to read. The page formats
// what it is given and decides nothing.
func (h HistoryEntry) TotalMinor() *int64 {
	if h.PreviousOwner || !h.HasInvoice {
		return nil
	}
	gross := h.GrossMinor
	return &gross
}

// Total renders what the job came to as a plain decimal, for anything that is
// not a rendered page.
func (h HistoryEntry) Total() string {
	if h.TotalMinor() == nil {
		return ""
	}
	return money.Format(h.GrossMinor)
}

// Reading is one odometer entry.
type Reading struct {
	Km         int64
	ReadAt     time.Time
	Source     string
	Decreasing bool
}

// RegistrationPeriod is a plate the vehicle carried, and when.
type RegistrationPeriod struct {
	Registration string
	ValidFrom    time.Time
	ValidTo      *time.Time
}

// Current reports whether the vehicle carries this plate now.
func (r RegistrationPeriod) Current() bool { return r.ValidTo == nil }

// VehicleByID reads a vehicle with its history.
func VehicleByID(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string) (Vehicle, error) {
	if !looksLikeUUID(id) {
		return Vehicle{}, ErrNotFound
	}
	var v Vehicle
	seesPeople := scope.Role.SeesCustomerPersonalData()

	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var ownedSince *time.Time
		var owner string
		const head = `
			SELECT v.id, coalesce(r.registration, ''), coalesce(v.vin, ''),
			       coalesce(v.make, ''), coalesce(v.model, ''), v.model_year,
			       coalesce(v.engine, ''), coalesce(v.label, ''),
			       coalesce(cp.display_name, c.company_name, ''), o.owned_from
			FROM vehicles v
			LEFT JOIN vehicle_registrations r
			       ON r.vehicle_id = v.id AND r.valid_to IS NULL
			LEFT JOIN vehicle_ownership o
			       ON o.vehicle_id = v.id AND o.owned_to IS NULL
			LEFT JOIN customers c ON c.id = o.customer_id
			LEFT JOIN people cp   ON cp.id = c.person_id
			WHERE v.id = $1`
		err := tx.QueryRow(ctx, head, id).Scan(&v.ID, &v.Registration, &v.VIN,
			&v.Make, &v.Model, &v.ModelYear, &v.Engine, &v.Label, &owner, &ownedSince)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read vehicle: %w", err)
		}
		v.OwnedSince = ownedSince
		if seesPeople {
			v.OwnerName = owner
		}

		// Every plate this vehicle has carried. A personalised plate that
		// moved away is part of the record, not something to hide: it is how
		// somebody makes sense of a service book that says a different number.
		regs, err := tx.Query(ctx,
			`SELECT registration, valid_from, valid_to FROM vehicle_registrations
			 WHERE vehicle_id = $1 ORDER BY valid_from DESC`, id)
		if err != nil {
			return fmt.Errorf("read registrations: %w", err)
		}
		for regs.Next() {
			var p RegistrationPeriod
			if err := regs.Scan(&p.Registration, &p.ValidFrom, &p.ValidTo); err != nil {
				regs.Close()
				return fmt.Errorf("scan registration: %w", err)
			}
			v.Registrations = append(v.Registrations, p)
		}
		regs.Close()
		if err := regs.Err(); err != nil {
			return err
		}

		// Jobs, newest first, with whether each one predates the current
		// ownership.
		const history = `
			SELECT w.id, w.number, w.state, w.opened_at, coalesce(w.complaint, ''),
			       ($2::timestamptz IS NOT NULL AND w.opened_at < $2::timestamptz) AS previous_owner,
			       coalesce(cp.display_name, c.company_name, ''),
			       coalesce(i.gross_minor, 0), i.id IS NOT NULL,
			       (SELECT o.km FROM odometer_readings o
			         WHERE o.vehicle_id = w.vehicle_id AND o.read_at <= w.opened_at
			         ORDER BY o.read_at DESC LIMIT 1)
			FROM work_orders w
			LEFT JOIN customers c ON c.id = w.customer_id
			LEFT JOIN people cp   ON cp.id = c.person_id
			LEFT JOIN invoices i  ON i.work_order_id = w.id AND i.credit_of_id IS NULL
			WHERE w.vehicle_id = $1
			ORDER BY w.opened_at DESC`
		rows, err := tx.Query(ctx, history, id, ownedSince)
		if err != nil {
			return fmt.Errorf("read history: %w", err)
		}
		for rows.Next() {
			var h HistoryEntry
			var customer string
			if err := rows.Scan(&h.WorkOrderID, &h.Number, &h.State, &h.OpenedAt, &h.Complaint,
				&h.PreviousOwner, &customer, &h.GrossMinor, &h.HasInvoice, &h.OdometerKm); err != nil {
				rows.Close()
				return fmt.Errorf("scan history: %w", err)
			}
			if seesPeople && !h.PreviousOwner {
				h.CustomerName = customer
			}
			v.History = append(v.History, h)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		// What was done, per job. Kept to descriptions: the technical record
		// travels with the car, the prices do not.
		for idx := range v.History {
			lines, err := tx.Query(ctx,
				`SELECT description FROM work_order_lines
				 WHERE work_order_id = $1 ORDER BY position`, v.History[idx].WorkOrderID)
			if err != nil {
				return fmt.Errorf("read lines: %w", err)
			}
			for lines.Next() {
				var d string
				if err := lines.Scan(&d); err != nil {
					lines.Close()
					return fmt.Errorf("scan line: %w", err)
				}
				v.History[idx].Lines = append(v.History[idx].Lines, d)
			}
			lines.Close()
			if err := lines.Err(); err != nil {
				return err
			}
		}

		readings, err := tx.Query(ctx,
			`SELECT km, read_at, source, decreasing FROM odometer_readings
			 WHERE vehicle_id = $1 ORDER BY read_at DESC LIMIT 20`, id)
		if err != nil {
			return fmt.Errorf("read odometer: %w", err)
		}
		defer readings.Close()
		for readings.Next() {
			var r Reading
			if err := readings.Scan(&r.Km, &r.ReadAt, &r.Source, &r.Decreasing); err != nil {
				return fmt.Errorf("scan reading: %w", err)
			}
			v.Readings = append(v.Readings, r)
		}
		return readings.Err()
	})
	return v, err
}
