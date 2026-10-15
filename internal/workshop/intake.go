package workshop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrInvalid reports input a person can fix.
var ErrInvalid = errors.New("workshop: invalid")

// NormaliseRegistration strips what people vary and machines should not care
// about, so that "abc 12 d", "ABC12D" and "ABC-12D" find the same vehicle.
//
// The original is kept for display: a plate is shown the way it is written.
func NormaliseRegistration(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// TakeIn opens a job for a vehicle arriving at the counter.
//
// The vehicle is created if it is not known. The customer is whoever owns it
// now, and is left empty when nobody does -- see the migration for why a job
// without a customer is allowed and an invoice without one is not.
// Details are what a lookup or a person filled in. They are applied only to a
// vehicle being created: a car already on file has a history, and overwriting
// its make from a third party because somebody opened a job is not an
// improvement.
type Details struct {
	Make      string
	Model     string
	ModelYear *int16
	Engine    string
	VIN       string
}

func TakeIn(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, registration, complaint string, odometerKm *int64) (string, error) {
	return TakeInWith(ctx, pool, scope, registration, complaint, odometerKm, Details{})
}

// TakeInWith opens a job, filling in a new vehicle's details.
func TakeInWith(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, registration, complaint string, odometerKm *int64, d Details) (string, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return "", access.ErrForbidden
	}
	if strings.TrimSpace(complaint) == "" {
		return "", fmt.Errorf("%w: say what is wrong with it", ErrInvalid)
	}

	var jobID string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		jobID, err = takeInTx(ctx, tx, scope, registration, complaint, odometerKm, d)
		return err
	})
	if err != nil {
		return "", err
	}
	return jobID, nil
}

// ErrBookingEnded is a booking cancelled or marked as not coming while its
// intake form was open. The car may well be at the counter; taking it in is
// then a walk-in, and a decision for whoever is standing there.
var ErrBookingEnded = errors.New("workshop: the booking was cancelled or marked as not coming")

// BookingIntake is a booked car being taken in, with what the desk decided
// about it.
type BookingIntake struct {
	BookingID, Registration, Complaint string
	OdometerKm                         *int64
	Details                            Details

	// Who pays. "" decides nothing: the car's owner, if it has one, and
	// otherwise a job that says it has no customer yet. "new" makes a private
	// customer from the booking's name and number; "existing" is CustomerID,
	// chosen by the desk -- a similar name is never enough to choose for
	// them. AlsoOwner records that customer as the car's owner too, which is
	// a separate fact and is never implied.
	Customer   string
	CustomerID string
	AlsoOwner  bool

	// Who does the work, confirmed. Empty gives it to nobody; the booked
	// technician is offered by the form, not assumed here.
	TechnicianID string

	// When the customer was told it will be ready. Only ever what the desk
	// agreed: the booking's slot is not a promise, and the hours sold are
	// not one either.
	PromisedAt *time.Time
}

// TakeInBooking takes in the car a booking was for, and ends the booking
// pointing at the job, in one transaction: a job with no booking, or a
// booking pointing at nothing, cannot be left behind by a failure between
// the two.
//
// The booking is locked first, so a second intake of it -- another tab,
// another person, a replay after the answer was lost -- waits for the first
// and then finds it arrived. It gets the first one's job back, with already
// set, and nothing is written twice. One job per booking, not per car: the
// same car may have several jobs, and taking it in without the booking is
// always possible.
//
// A booking moved while the form was open is still the booking, and is taken
// in. One that is not there, malformed, or another shop's is ErrNotFound and
// one cancelled or marked as not coming is ErrBookingEnded -- never a new job
// in its name.
func TakeInBooking(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, in BookingIntake) (jobID string, already bool, err error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return "", false, access.ErrForbidden
	}
	if strings.TrimSpace(in.Complaint) == "" {
		return "", false, fmt.Errorf("%w: say what is wrong with it", ErrInvalid)
	}
	if !looksLikeUUID(in.BookingID) {
		return "", false, ErrNotFound
	}
	switch in.Customer {
	case "", "new":
	case "existing":
		if !looksLikeUUID(in.CustomerID) {
			return "", false, fmt.Errorf("%w: choose which customer", ErrInvalid)
		}
	default:
		return "", false, fmt.Errorf("%w: %q is not a way to say who pays", ErrInvalid, in.Customer)
	}
	if in.TechnicianID != "" && !looksLikeUUID(in.TechnicianID) {
		return "", false, fmt.Errorf("%w: choose who does the work", ErrInvalid)
	}
	err = database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var status, name, phone, bookedTech, bookedTechName string
		var existing *string
		err := tx.QueryRow(ctx, `
			SELECT b.status, b.work_order_id, coalesce(b.customer_name, ''), coalesce(b.customer_phone, ''),
			       coalesce(b.technician_id::text, ''), coalesce(tp.display_name, '')
			FROM bookings b
			LEFT JOIN users tu ON tu.id = b.technician_id LEFT JOIN people tp ON tp.id = tu.person_id
			WHERE b.id = $1 FOR UPDATE OF b`, in.BookingID).Scan(&status, &existing, &name, &phone, &bookedTech, &bookedTechName)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read booking: %w", err)
		}
		switch {
		case status == "arrived" && existing != nil:
			jobID, already = *existing, true
			return nil
		case status != "booked":
			return ErrBookingEnded
		}

		// Who does the work, checked before anything is written. Somebody
		// who has left since the booking is not given it; the desk chooses
		// another, and the booking says what happened.
		techName, err := technicianNameTx(ctx, tx, in.TechnicianID)
		if err != nil {
			return err
		}
		var note string
		if in.TechnicianID != bookedTech {
			switch {
			case in.TechnicianID == "":
				note = "Booked for " + bookedTechName + "; not given to anybody at intake"
			case bookedTech == "":
				note = "Given to " + techName + " at intake"
			default:
				note = "Booked for " + bookedTechName + "; given to " + techName + " at intake"
			}
		}

		var customerID string
		switch in.Customer {
		case "new":
			// Made from what the booking holds, so nothing is retyped. The
			// desk can correct it on the customer's own page.
			if strings.TrimSpace(name) == "" {
				return fmt.Errorf("%w: the booking has no name to make a customer from", ErrInvalid)
			}
			if customerID, err = createCustomerTx(ctx, tx, scope, NewCustomer{Kind: "private", Name: name, Phone: phone}); err != nil {
				return err
			}
		case "existing":
			// One of ours: row level security makes a foreign identifier
			// match nothing, and that is a refusal, not a null.
			var ok bool
			if err := tx.QueryRow(ctx, `SELECT exists(SELECT 1 FROM customers WHERE id = $1)`, in.CustomerID).Scan(&ok); err != nil {
				return fmt.Errorf("check customer: %w", err)
			}
			if !ok {
				return ErrNotFound
			}
			customerID = in.CustomerID
		}

		if jobID, err = takeInTx(ctx, tx, scope, in.Registration, in.Complaint, in.OdometerKm, in.Details); err != nil {
			return err
		}
		if err := assignTx(ctx, tx, scope, jobID, customerID, in.AlsoOwner, in.TechnicianID, in.PromisedAt); err != nil {
			return err
		}
		// A booking made from an accepted proposal brings its lines, agreed,
		// at the price the customer was shown.
		if err := carryAcceptedLinesTx(ctx, tx, scope.ShopID, jobID, in.BookingID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE bookings SET status = 'arrived', work_order_id = $2,
			       technician_id = nullif($3, '')::uuid,
			       vehicle_id = (SELECT vehicle_id FROM work_orders WHERE id = $2)
			WHERE id = $1`, in.BookingID, jobID, in.TechnicianID); err != nil {
			return fmt.Errorf("arrive booking: %w", err)
		}
		return bookingEventTx(ctx, tx, scope, in.BookingID, "arrived", nil, nil, note)
	})
	if err != nil {
		return "", false, err
	}
	return jobID, already, nil
}

// technicianNameTx checks that a technician is working here and returns their
// name. An empty identifier is nobody, which is allowed.
func technicianNameTx(ctx context.Context, tx pgx.Tx, id string) (string, error) {
	if id == "" {
		return "", nil
	}
	var name string
	err := tx.QueryRow(ctx, `
		SELECT p.display_name FROM users u JOIN people p ON p.id = u.person_id
		WHERE u.id = $1 AND u.active AND u.role = 'technician'`, id).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: that technician is not working here", ErrInvalid)
	}
	if err != nil {
		return "", fmt.Errorf("read technician: %w", err)
	}
	return name, nil
}

// assignTx writes what the desk confirmed at intake onto the job: who pays,
// who does the work and when it was promised. Each is optional; a job
// without them says so and nothing is invented.
func assignTx(ctx context.Context, tx pgx.Tx, scope access.Scope, jobID, customerID string, alsoOwner bool, technicianID string, promised *time.Time) error {
	if customerID != "" {
		var vehicleID string
		if err := tx.QueryRow(ctx, `UPDATE work_orders SET customer_id = $2 WHERE id = $1 RETURNING vehicle_id`,
			jobID, customerID).Scan(&vehicleID); err != nil {
			return fmt.Errorf("assign customer: %w", err)
		}
		// The payer and the owner are two facts, written apart: the owner is
		// only changed when the desk says so.
		if alsoOwner {
			if err := recordOwnerTx(ctx, tx, scope, vehicleID, customerID); err != nil {
				return err
			}
		}
	}
	if technicianID != "" {
		if err := assignTechnicianTx(ctx, tx, scope, jobID, technicianID, ""); err != nil {
			return err
		}
	}
	if promised != nil {
		if _, err := tx.Exec(ctx, `UPDATE work_orders SET promised_at = $2 WHERE id = $1`, jobID, promised); err != nil {
			return fmt.Errorf("record promise: %w", err)
		}
	}
	return nil
}

// WalkIn is a car that came without a booking, with what the desk decided
// about it. The customer is chosen the same way as for a booked car; with no
// booking there is no name to make one from, so "new" carries what was typed.
type WalkIn struct {
	Registration, Complaint string
	OdometerKm              *int64
	Details                 Details

	Customer     string // "", "new" or "existing"
	CustomerID   string
	NewName      string
	NewPhone     string
	AlsoOwner    bool
	TechnicianID string
	PromisedAt   *time.Time
}

// TakeInWalkIn opens a job for a car that arrived unannounced. Everything
// beyond the complaint is optional: a car is taken in with whatever is known,
// and the job says what is not.
func TakeInWalkIn(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, in WalkIn) (string, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return "", access.ErrForbidden
	}
	if strings.TrimSpace(in.Complaint) == "" {
		return "", fmt.Errorf("%w: say what is wrong with it", ErrInvalid)
	}
	switch in.Customer {
	case "":
	case "new":
		if strings.TrimSpace(in.NewName) == "" {
			return "", fmt.Errorf("%w: a new customer needs a name", ErrInvalid)
		}
	case "existing":
		if !looksLikeUUID(in.CustomerID) {
			return "", fmt.Errorf("%w: choose which customer", ErrInvalid)
		}
	default:
		return "", fmt.Errorf("%w: %q is not a way to say who pays", ErrInvalid, in.Customer)
	}
	if in.TechnicianID != "" && !looksLikeUUID(in.TechnicianID) {
		return "", fmt.Errorf("%w: choose who does the work", ErrInvalid)
	}
	var jobID string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := technicianNameTx(ctx, tx, in.TechnicianID); err != nil {
			return err
		}
		var customerID string
		switch in.Customer {
		case "new":
			var err error
			if customerID, err = createCustomerTx(ctx, tx, scope, NewCustomer{Kind: "private", Name: strings.TrimSpace(in.NewName), Phone: strings.TrimSpace(in.NewPhone)}); err != nil {
				return err
			}
		case "existing":
			var ok bool
			if err := tx.QueryRow(ctx, `SELECT exists(SELECT 1 FROM customers WHERE id = $1)`, in.CustomerID).Scan(&ok); err != nil {
				return fmt.Errorf("check customer: %w", err)
			}
			if !ok {
				return ErrNotFound
			}
			customerID = in.CustomerID
		}
		var err error
		if jobID, err = takeInTx(ctx, tx, scope, in.Registration, in.Complaint, in.OdometerKm, in.Details); err != nil {
			return err
		}
		return assignTx(ctx, tx, scope, jobID, customerID, in.AlsoOwner, in.TechnicianID, in.PromisedAt)
	})
	if err != nil {
		return "", err
	}
	return jobID, nil
}

// takeInTx opens the job, inside whatever transaction the caller holds.
func takeInTx(ctx context.Context, tx pgx.Tx, scope access.Scope, registration, complaint string, odometerKm *int64, d Details) (string, error) {
	normalised := NormaliseRegistration(registration)
	var jobID string
	var vehicleID string

	if normalised != "" {
		err := tx.QueryRow(ctx, `
			SELECT vehicle_id FROM vehicle_registrations
			WHERE normalised = $1 AND valid_to IS NULL`, normalised).Scan(&vehicleID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("look up registration: %w", err)
		}
	}

	if vehicleID == "" {
		// Unknown vehicle, or none given. Either way a row is created: the
		// alternative is refusing to take the car in, and the car is
		// already here.
		if err := tx.QueryRow(ctx, `
			INSERT INTO vehicles (shop_id, make, model, model_year, engine, vin)
			VALUES ($1, nullif($2,''), nullif($3,''), $4, nullif($5,''), nullif($6,''))
			RETURNING id`,
			scope.ShopID, strings.TrimSpace(d.Make), strings.TrimSpace(d.Model),
			d.ModelYear, strings.TrimSpace(d.Engine), strings.TrimSpace(d.VIN),
		).Scan(&vehicleID); err != nil {
			return "", fmt.Errorf("create vehicle: %w", err)
		}
		if normalised != "" {
			if _, err := tx.Exec(ctx, `
				INSERT INTO vehicle_registrations (shop_id, vehicle_id, registration, normalised)
				VALUES ($1, $2, $3, $4)`,
				scope.ShopID, vehicleID, strings.TrimSpace(registration), normalised); err != nil {
				return "", fmt.Errorf("record registration: %w", err)
			}
		}
	}

	var customerID *string
	var owner string
	err := tx.QueryRow(ctx, `
		SELECT customer_id FROM vehicle_ownership
		WHERE vehicle_id = $1 AND owned_to IS NULL`, vehicleID).Scan(&owner)
	if err == nil {
		customerID = &owner
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("look up owner: %w", err)
	}

	// Order numbers are per shop and taken as the next one up. Proper
	// gap-free numbering under concurrency is the invoice numbering work;
	// a work order number is a label, not an accounting record, and two
	// people taking in cars at the same instant will collide on the unique
	// index and the second one retries. Do not copy this for invoices.
	var number int64
	if err := tx.QueryRow(ctx,
		`SELECT coalesce(max(number), 0) + 1 FROM work_orders`).Scan(&number); err != nil {
		return "", fmt.Errorf("next number: %w", err)
	}

	if err := tx.QueryRow(ctx, `
		INSERT INTO work_orders (shop_id, number, vehicle_id, customer_id, complaint, state, created_by)
		VALUES ($1, $2, $3, $4, $5, 'draft', $6)
		RETURNING id`,
		scope.ShopID, number, vehicleID, customerID, strings.TrimSpace(complaint), scope.UserID,
	).Scan(&jobID); err != nil {
		return "", fmt.Errorf("open job: %w", err)
	}

	// Taking a car in is the car being left with us: the first row of
	// where it is, so a later collection and return read as what they are.
	if _, err := tx.Exec(ctx, `
		INSERT INTO vehicle_presence (shop_id, work_order_id, event, recorded_by)
		VALUES ($1, $2, 'left', $3)`, scope.ShopID, jobID, scope.UserID); err != nil {
		return "", fmt.Errorf("record the car left: %w", err)
	}

	if odometerKm != nil {
		// Recorded even when it is lower than the last reading, and
		// flagged rather than refused: a replaced cluster is legitimate,
		// and a rolled-back odometer is something the shop wants on
		// record.
		var decreasing bool
		if err := tx.QueryRow(ctx, `
			SELECT exists(
			    SELECT 1 FROM odometer_readings
			    WHERE vehicle_id = $1 AND km > $2
			    ORDER BY read_at DESC LIMIT 1)`,
			vehicleID, *odometerKm).Scan(&decreasing); err != nil {
			return "", fmt.Errorf("compare odometer: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO odometer_readings (shop_id, vehicle_id, km, source, decreasing)
			VALUES ($1, $2, $3, 'drop_off', $4)`,
			scope.ShopID, vehicleID, *odometerKm, decreasing); err != nil {
			return "", fmt.Errorf("record odometer: %w", err)
		}
	}
	return jobID, nil
}
