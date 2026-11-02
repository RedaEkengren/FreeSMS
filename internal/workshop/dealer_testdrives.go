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
)

// Test drives (#173): a car in stock held for a slot on the availability
// ledger, so the workshop, a rental, a delivery or another drive cannot
// have it at the same time -- the database decides, not the page. Before
// the keys leave, the driver's licence is checked and the agreement
// recorded; as they leave and come back, the time and the odometer. A
// walk-in "now" is the same booking, starting now.

// The states of a drive, from its events.
const (
	DriveBooked    = "booked"
	DriveStarted   = "started"
	DriveReturned  = "returned"
	DriveCancelled = "cancelled"
)

// TestDriveLicenceMethods are the ways a licence is looked at.
var TestDriveLicenceMethods = []string{"card_inspected", "digital_licence", "foreign_inspected", "other"}

// unsignedStartReason is the audit log's reason for a drive started
// without a signed agreement: why, in the person's words, is on the drive.
const unsignedStartReason = "Started without a signed agreement; why is noted on the drive"

// defaultTestDriveAge is the youngest driver let out until the shop says:
// the age a Swedish B licence is held from.
const defaultTestDriveAge = 18

// TestDrive is one drive, as the salesperson reads it.
type TestDrive struct {
	ID          string
	StockUnitID string
	Car         string
	DriverID    string
	Driver      string
	LeadID      string
	BlockID     string
	StartsAt    time.Time
	EndsAt      time.Time
	Area        string
	AgreementID string
	// The agreement is signed by the driver.
	Signed     bool
	State      string
	StartedAt  *time.Time
	ReturnedAt *time.Time
	StartKm    *int64
	ReturnKm   *int64
	// The newest licence check, if any.
	Licence *LicenceCheck
	Notes   []string
}

// Overdue reports whether a drive that has started is past its end.
func (d TestDrive) Overdue(now time.Time) bool {
	return d.State == DriveStarted && now.After(d.EndsAt)
}

// LicenceCheck is what was seen of the driver's licence: never its number.
type LicenceCheck struct {
	IDChecked  bool
	Method     string
	Categories []string
	ValidUntil *time.Time
	AgeOK      bool
	Note       string
	CheckedAt  time.Time
	CheckedBy  string
}

// Passes reports whether the check lets the drive start on day: the
// driver's identity seen, a licence for a car valid that day, and old
// enough. day is the shop's date, at midnight UTC as the database hands a
// date back -- never an instant, whose UTC day is not the shop's for two
// hours of every night.
func (c LicenceCheck) Passes(day time.Time) bool {
	return c.IDChecked && c.AgeOK && slices.Contains(c.Categories, "B") &&
		(c.ValidUntil == nil || !c.ValidUntil.Before(day))
}

// NewTestDrive is a drive to book.
type NewTestDrive struct {
	StockUnitID   string
	DriverPartyID string
	LeadID        string
	From, To      time.Time
	Area          string
	Key           string
	// Booked knowing the car is reserved for someone else, where the shop
	// lets that be done with a warning.
	DespiteReservation bool
	// A walk-in driver who is not a party yet: a private person, by name
	// and phone, made in the booking's own transaction, so a booking sent
	// twice makes one and a booking refused makes none. Instead of
	// DriverPartyID.
	NewDriver *NewParty
}

var (
	errDriveTime     = fmt.Errorf("%w: a test drive ends after it starts, within a day, and does not start in the past", ErrInvalid)
	errDriveText     = fmt.Errorf("%w: say where they may drive in 300 characters or fewer", ErrInvalid)
	errDriveCar      = fmt.Errorf("%w: that car is not in stock to be driven", ErrInvalid)
	errDriveKey      = fmt.Errorf("%w: that request was already used for another test drive", ErrInvalid)
	errDriveState    = fmt.Errorf("%w: the test drive is not where that can be done", ErrInvalid)
	errDriveLicence  = fmt.Errorf("%w: the driver's identity, licence and age have not been checked and found good for today", ErrInvalid)
	errDriveUnsigned = fmt.Errorf("%w: the agreement is not signed; whoever runs the shop may let the drive start with a note why", ErrInvalid)
	errDriveOdometer = fmt.Errorf("%w: say what the odometer shows, in whole kilometres", ErrInvalid)
	errDriveLess     = fmt.Errorf("%w: the odometer cannot show less on return than when the drive started", ErrInvalid)
	errDriveMethod   = fmt.Errorf("%w: say how the licence was checked, and what it allows in categories such as B", ErrInvalid)
	errDriveNote     = fmt.Errorf("%w: a note is at most 300 characters on a check and 500 on a drive", ErrInvalid)
	errDriveAge      = fmt.Errorf("%w: the youngest driver is from 16 to 99", ErrInvalid)
	errDriveDriver   = fmt.Errorf("%w: say who drives: a name of up to 200 characters and a phone number", ErrInvalid)
	// ErrMadeMeanwhile is a contract or agreement made again from a page that
	// did not show the newest one: somebody made one meanwhile.
	ErrMadeMeanwhile = fmt.Errorf("%w: somebody made a new one meanwhile; read it again before making another", ErrInvalid)

	// ErrDriveReserved is a car reserved for somebody else: booked again
	// with DespiteReservation it is let through, unless the shop blocks it.
	ErrDriveReserved = fmt.Errorf("%w: the car is reserved for another customer", ErrInvalid)
	// ErrDriveReservedBlocked is the same where the shop does not let a
	// reserved car be driven by anyone else.
	ErrDriveReservedBlocked = fmt.Errorf("%w: the car is reserved for another customer, and this shop does not let it be driven meanwhile", ErrInvalid)
)

const testDriveColumns = `
	SELECT d.id, d.stock_unit_id, ` + carNameSQL + `,
	       d.driver_party_id, coalesce(dn.name, ''), coalesce(d.lead_id::text, ''), d.block_id, d.starts_at, d.ends_at,
	       coalesce(d.area, ''), coalesce(d.agreement_id::text, ''),
	       d.agreement_id IS NOT NULL AND EXISTS (SELECT 1 FROM document_record_parties dp JOIN document_signatures s ON s.document_party_id = dp.id
	                                              WHERE dp.document_id = d.agreement_id AND dp.role = 'driver'),
	       coalesce((SELECT e.kind FROM test_drive_events e WHERE e.test_drive_id = d.id ORDER BY e.at DESC, e.id DESC LIMIT 1), 'booked'),
	       (SELECT e.at FROM test_drive_events e WHERE e.test_drive_id = d.id AND e.kind = 'started'),
	       (SELECT e.at FROM test_drive_events e WHERE e.test_drive_id = d.id AND e.kind = 'returned'),
	       (SELECT o.km FROM test_drive_events e JOIN odometer_readings o ON o.id = e.odometer_reading_id
	         WHERE e.test_drive_id = d.id AND e.kind = 'started'),
	       (SELECT o.km FROM test_drive_events e JOIN odometer_readings o ON o.id = e.odometer_reading_id
	         WHERE e.test_drive_id = d.id AND e.kind = 'returned')
	FROM test_drives d
	JOIN stock_units u ON u.id = d.stock_unit_id
	JOIN vehicles v ON v.id = u.vehicle_id
	LEFT JOIN vehicle_registrations reg ON reg.vehicle_id = v.id AND reg.valid_to IS NULL
	LEFT JOIN LATERAL (
		SELECT coalesce(CASE WHEN pa.kind = 'company' THEN coalesce(c.company_name, pa.company_name) END, cp.display_name, pp.display_name) AS name
		FROM parties pa LEFT JOIN customers c ON c.id = pa.customer_id
		LEFT JOIN people cp ON cp.id = c.person_id LEFT JOIN people pp ON pp.id = pa.person_id
		WHERE pa.id = d.driver_party_id) dn ON true`

func scanTestDrive(row pgx.Row) (TestDrive, error) {
	var d TestDrive
	err := row.Scan(&d.ID, &d.StockUnitID, &d.Car, &d.DriverID, &d.Driver, &d.LeadID, &d.BlockID, &d.StartsAt, &d.EndsAt,
		&d.Area, &d.AgreementID, &d.Signed, &d.State, &d.StartedAt, &d.ReturnedAt, &d.StartKm, &d.ReturnKm)
	return d, err
}

func testDriveTx(ctx context.Context, tx pgx.Tx, id string) (TestDrive, error) {
	d, err := scanTestDrive(tx.QueryRow(ctx, testDriveColumns+` WHERE d.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return TestDrive{}, ErrNotFound
	}
	if err != nil {
		return TestDrive{}, fmt.Errorf("read test drive: %w", err)
	}
	var c LicenceCheck
	err = tx.QueryRow(ctx, `
		SELECT k.id_checked, k.method, k.categories, k.valid_until, k.age_ok, coalesce(k.note, ''), k.checked_at, coalesce(p.display_name, '')
		FROM test_drive_licence_checks k LEFT JOIN users u ON u.id = k.checked_by LEFT JOIN people p ON p.id = u.person_id
		WHERE k.test_drive_id = $1 ORDER BY k.checked_at DESC, k.id DESC LIMIT 1`, id).Scan(
		&c.IDChecked, &c.Method, &c.Categories, &c.ValidUntil, &c.AgeOK, &c.Note, &c.CheckedAt, &c.CheckedBy)
	switch {
	case err == nil:
		d.Licence = &c
	case !errors.Is(err, pgx.ErrNoRows):
		return TestDrive{}, fmt.Errorf("read the licence check: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT note FROM test_drive_events WHERE test_drive_id = $1 AND note IS NOT NULL ORDER BY at, id`, id)
	if err != nil {
		return TestDrive{}, err
	}
	notes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	d.Notes = notes
	return d, err
}

// testDriveSettingsTx reads the shop's minimum age and whether a reserved
// car is blocked for others.
func testDriveSettingsTx(ctx context.Context, tx pgx.Tx) (age int, blocks bool, err error) {
	err = tx.QueryRow(ctx, `
		SELECT coalesce((SELECT minimum_age FROM test_drive_settings ORDER BY seq DESC LIMIT 1), $1),
		       coalesce((SELECT reserved_blocks FROM test_drive_settings ORDER BY seq DESC LIMIT 1), false)`,
		defaultTestDriveAge).Scan(&age, &blocks)
	return age, blocks, err
}

// BookTestDrive holds a car in stock for a drive, for a driver who is a
// party: on the ledger, so whatever else is booked on it in that time --
// the workshop, a rental, a delivery, another drive -- refuses it, naming
// what is in the way; moving that is an override (OverrideBlock), audited.
// Two bookings of the same slot at once: one stands. A car reserved for
// someone else is a warning (ErrDriveReserved), or a refusal where the shop
// says so. The same request sent again finds the drive it booked. For
// whoever sells cars, in a shop that sells them.
func BookTestDrive(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, n NewTestDrive) (TestDrive, error) {
	if !scope.Role.SellsVehicles() {
		return TestDrive{}, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return TestDrive{}, err
	}
	if n.NewDriver != nil {
		if n.DriverPartyID != "" {
			return TestDrive{}, errDriveDriver
		}
		d := *n.NewDriver
		d.Kind, d.Role, d.Name = "private", PartyBuyer, strings.TrimSpace(d.Name)
		if d.Name == "" || utf8.RuneCountInString(d.Name) > 200 || utf8.RuneCountInString(d.Phone) > 40 {
			return TestDrive{}, errDriveDriver
		}
		if err := mayRecordPartyRole(scope.Role, scope, d.Role); err != nil {
			return TestDrive{}, err
		}
		n.NewDriver = &d
	} else if !looksLikeUUID(n.DriverPartyID) {
		return TestDrive{}, ErrNotFound
	}
	if !looksLikeUUID(n.StockUnitID) || (n.LeadID != "" && !looksLikeUUID(n.LeadID)) {
		return TestDrive{}, ErrNotFound
	}
	n.Area, n.Key = strings.TrimSpace(n.Area), strings.TrimSpace(n.Key)
	if utf8.RuneCountInString(n.Area) > 300 || len(n.Key) > 100 {
		return TestDrive{}, errDriveText
	}
	if n.From.IsZero() || !n.To.After(n.From) || n.To.Sub(n.From) > 24*time.Hour || n.From.Before(time.Now().Add(-15*time.Minute)) {
		return TestDrive{}, errDriveTime
	}
	var out TestDrive
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var resource string
		var status StockStatus
		var ended bool
		err := tx.QueryRow(ctx, `SELECT resource_id, status, closed_at IS NOT NULL FROM stock_units WHERE id = $1`, n.StockUnitID).
			Scan(&resource, &status, &ended)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read the car: %w", err)
		}
		// The resource first, locked, then its blocks: the order every
		// write to it takes (BookResource). The same request in flight
		// has committed by then, and its drive is the answer.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM resources WHERE id = $1 FOR UPDATE`, resource); err != nil {
			return fmt.Errorf("lock the car: %w", err)
		}
		if n.Key != "" {
			var id, unit string
			err := tx.QueryRow(ctx, `SELECT id, stock_unit_id FROM test_drives WHERE request_key = $1`, n.Key).Scan(&id, &unit)
			if err == nil {
				if unit != n.StockUnitID {
					return errDriveKey
				}
				out, err = testDriveTx(ctx, tx, id)
				return err
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("read the request: %w", err)
			}
		}
		if err := driveCarFreeTx(ctx, tx, n.StockUnitID, n.DriverPartyID, status, ended, n.DespiteReservation); err != nil {
			return err
		}
		if n.NewDriver != nil {
			if n.DriverPartyID, err = createPartyTx(ctx, tx, scope, *n.NewDriver, ""); err != nil {
				return err
			}
		} else if err := partySeenTx(ctx, tx, scope, n.DriverPartyID); err != nil {
			return err
		}
		if n.LeadID != "" {
			if _, err := leadTx(ctx, tx, scope, n.LeadID); err != nil {
				return err
			}
		}
		var id string
		if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()`).Scan(&id); err != nil {
			return err
		}
		period := blockRange(n.From, n.To)
		if _, err := tx.Exec(ctx, `SELECT release_expired_holds($1, $2)`, resource, period); err != nil {
			return fmt.Errorf("release expired holds: %w", err)
		}
		if err := takenTx(ctx, tx, resource, period, ""); err != nil {
			return err
		}
		var block string
		err = tx.QueryRow(ctx, `
			INSERT INTO resource_blocks (shop_id, resource_id, kind, status, period, source_kind, source_id, created_by)
			VALUES ($1, $2, 'test_drive', 'confirmed', $3, 'test_drive', $4, NULLIF($5, '')::uuid) RETURNING id`,
			scope.ShopID, resource, period, id, scope.UserID).Scan(&block)
		if err != nil {
			return blockConflict(err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO test_drives (id, shop_id, stock_unit_id, driver_party_id, lead_id, block_id, starts_at, ends_at, area,
			                         request_key, booked_by)
			VALUES ($1, $2, $3, party_now($4), NULLIF($5, '')::uuid, $6, $7, $8, nullif($9, ''), nullif($10, ''), NULLIF($11, '')::uuid)`,
			id, scope.ShopID, n.StockUnitID, n.DriverPartyID, n.LeadID, block, n.From, n.To, n.Area, n.Key, scope.UserID); err != nil {
			return fmt.Errorf("book the test drive: %w", err)
		}
		// Its lead has a drive booked.
		if n.LeadID != "" {
			var st string
			if err := tx.QueryRow(ctx, `SELECT coalesce(status, 'new') FROM lead_now WHERE lead_id = $1`, n.LeadID).Scan(&st); err != nil {
				return err
			}
			if st == LeadNew || st == LeadContacted {
				if err := leadEventTx(ctx, tx, scope, n.LeadID, "status", map[string]any{"status": LeadTestDriveBooked}); err != nil {
					return err
				}
			}
		}
		out, err = testDriveTx(ctx, tx, id)
		return err
	})
	return out, err
}

// driveCarFreeTx says whether a car in stock may be driven by driver: not
// once its cycle has ended or it is sold, and, reserved for somebody else,
// only knowing it (despite), and never where the shop blocks a reserved
// car. Reserved for the driver themselves, it is theirs to drive. Asked as
// a drive is booked and again as its keys are handed over: the car may
// have been reserved or sold in between.
func driveCarFreeTx(ctx context.Context, tx pgx.Tx, unitID, driver string, status StockStatus, ended, despite bool) error {
	if ended || status == StockSold || status == StockDelivered {
		return errDriveCar
	}
	if status != StockReserved {
		return nil
	}
	holder, err := reservedForTx(ctx, tx, unitID)
	if err != nil {
		return err
	}
	if holder != "" && holder == driver {
		return nil
	}
	_, blocks, err := testDriveSettingsTx(ctx, tx)
	switch {
	case err != nil:
		return err
	case blocks:
		return ErrDriveReservedBlocked
	case !despite:
		return ErrDriveReserved
	}
	return nil
}

// reservedForTx is the party a reserved car is held for, or "" when that
// is not known. A seam for #174: a reservation and who holds it are not
// built yet (the design is with the owner), so the status says only that
// the car is reserved; when they are, this reads the active reservation
// and its holder drives their own reserved car whatever the shop's block.
func reservedForTx(ctx context.Context, tx pgx.Tx, unitID string) (string, error) {
	return "", nil
}

// workDriveTx reads a drive to change, its car's resource locked first,
// then the drive.
func workDriveTx(ctx context.Context, tx pgx.Tx, id string) (TestDrive, error) {
	if !looksLikeUUID(id) {
		return TestDrive{}, ErrNotFound
	}
	if _, err := tx.Exec(ctx, `
		SELECT 1 FROM resources WHERE id = (SELECT b.resource_id FROM test_drives d JOIN resource_blocks b ON b.id = d.block_id WHERE d.id = $1)
		FOR UPDATE`, id); err != nil {
		return TestDrive{}, fmt.Errorf("lock the car: %w", err)
	}
	return testDriveTx(ctx, tx, id)
}

// LicenceCheckInput is what was seen of the driver's licence.
type LicenceCheckInput struct {
	IDChecked  bool
	Method     string
	Categories []string
	ValidUntil *time.Time
	// The driver is at least the shop's minimum age.
	AgeOK bool
	Note  string
}

// CheckDriverLicence records that the driver's identity and licence were
// looked at, by whom and when, what the licence allows and until when:
// never its number. Checking again is another row; the newest stands.
func CheckDriverLicence(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, driveID string, in LicenceCheckInput) error {
	if !scope.Role.SellsVehicles() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return err
	}
	in.Note = strings.TrimSpace(in.Note)
	if !slices.Contains(TestDriveLicenceMethods, in.Method) {
		return errDriveMethod
	}
	cats := make([]string, 0, len(in.Categories))
	for _, c := range in.Categories {
		c = strings.ToUpper(strings.TrimSpace(c))
		if c == "" {
			continue
		}
		if len(c) > 4 || strings.Trim(c, "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") != "" {
			return errDriveMethod
		}
		if !slices.Contains(cats, c) {
			cats = append(cats, c)
		}
	}
	if len(cats) > 12 {
		return errDriveMethod
	}
	if utf8.RuneCountInString(in.Note) > 300 {
		return errDriveNote
	}
	var until any
	if in.ValidUntil != nil {
		until = in.ValidUntil.Format(time.DateOnly)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		d, err := workDriveTx(ctx, tx, driveID)
		if err != nil {
			return err
		}
		if d.State != DriveBooked {
			return errDriveState
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO test_drive_licence_checks (shop_id, test_drive_id, id_checked, method, categories, valid_until, age_ok, note, checked_by)
			VALUES ($1, $2, $3, $4, $5, $6::date, $7, nullif($8, ''), NULLIF($9, '')::uuid)`,
			scope.ShopID, d.ID, in.IDChecked, in.Method, cats, until, in.AgeOK, in.Note, scope.UserID)
		return err
	})
}

// odometerTx records what the car's odometer showed as the keys changed
// hands.
func odometerTx(ctx context.Context, tx pgx.Tx, scope access.Scope, unitID string, km int64) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO odometer_readings (shop_id, vehicle_id, km, source, decreasing)
		SELECT $1, u.vehicle_id, $3, 'test_drive', EXISTS (SELECT 1 FROM odometer_readings o WHERE o.vehicle_id = u.vehicle_id AND o.km > $3)
		FROM stock_units u WHERE u.id = $2 RETURNING id`, scope.ShopID, unitID, km).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("record the odometer: %w", err)
	}
	return id, nil
}

// StartTestDrive hands the keys over: the licence checked and good for
// today, the agreement signed -- or, while the signing service is down or
// the paper is not to hand, let go unsigned by whoever runs the shop with
// a note, which is audited -- and the odometer read.
func StartTestDrive(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, driveID string, km int64, unsignedNote string) error {
	if !scope.Role.SellsVehicles() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return err
	}
	if km < 0 || km > 10_000_000 {
		return errDriveOdometer
	}
	unsignedNote = strings.TrimSpace(unsignedNote)
	if utf8.RuneCountInString(unsignedNote) > 500 {
		return errDriveNote
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		d, err := workDriveTx(ctx, tx, driveID)
		if err != nil {
			return err
		}
		if d.State != DriveBooked {
			return errDriveState
		}
		// The car as it is now, not as it was booked. Reserved since, the
		// salesperson handing the keys over knows it.
		var status StockStatus
		var ended bool
		if err := tx.QueryRow(ctx, `SELECT status, closed_at IS NOT NULL FROM stock_units WHERE id = $1`, d.StockUnitID).
			Scan(&status, &ended); err != nil {
			return fmt.Errorf("read the car: %w", err)
		}
		if err := driveCarFreeTx(ctx, tx, d.StockUnitID, d.DriverID, status, ended, true); err != nil {
			return err
		}
		var today time.Time
		if err := tx.QueryRow(ctx, `SELECT (clock_timestamp() AT TIME ZONE (SELECT timezone FROM shops))::date`).Scan(&today); err != nil {
			return err
		}
		if d.Licence == nil || !d.Licence.Passes(today) {
			return errDriveLicence
		}
		var note any
		if !d.Signed {
			if !scope.Role.RunsTheShop() || unsignedNote == "" {
				return errDriveUnsigned
			}
			note = unsignedNote
			// The note can name the driver, so it is kept on the drive,
			// where erasing them reaches it; the audit log, which is
			// append-only, says it happened in fixed words.
			if err := auditTx(ctx, tx, scope, d.ID, "test_drive.started_unsigned", "", "", unsignedStartReason,
				map[string]any{"agreement_id": d.AgreementID}); err != nil {
				return err
			}
		}
		reading, err := odometerTx(ctx, tx, scope, d.StockUnitID, km)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO test_drive_events (shop_id, test_drive_id, kind, by_user, odometer_reading_id, note)
			VALUES ($1, $2, 'started', NULLIF($3, '')::uuid, $4, $5)`, scope.ShopID, d.ID, scope.UserID, reading, note)
		return err
	})
}

// ReturnTestDrive takes the keys back: the time and the odometer, and
// whatever was seen on the car, as observed, never as a debt. The car's
// slot ends now, early or late.
func ReturnTestDrive(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, driveID string, km int64, seen string) error {
	if !scope.Role.SellsVehicles() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return err
	}
	if km < 0 || km > 10_000_000 {
		return errDriveOdometer
	}
	seen = strings.TrimSpace(seen)
	if utf8.RuneCountInString(seen) > 500 {
		return errDriveNote
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		d, err := workDriveTx(ctx, tx, driveID)
		if err != nil {
			return err
		}
		if d.State != DriveStarted {
			return errDriveState
		}
		if d.StartKm != nil && km < *d.StartKm {
			return errDriveLess
		}
		reading, err := odometerTx(ctx, tx, scope, d.StockUnitID, km)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO test_drive_events (shop_id, test_drive_id, kind, by_user, odometer_reading_id, note)
			VALUES ($1, $2, 'returned', NULLIF($3, '')::uuid, $4, nullif($5, ''))`, scope.ShopID, d.ID, scope.UserID, reading, seen); err != nil {
			return err
		}
		return endDriveBlockTx(ctx, tx, scope, d.BlockID)
	})
}

// endDriveBlockTx lets the car's slot go from now: a drive back early
// frees the rest of it, one back late has held the car until now.
func endDriveBlockTx(ctx context.Context, tx pgx.Tx, scope access.Scope, block string) error {
	_, err := tx.Exec(ctx, `
		UPDATE resource_blocks
		   SET status = 'released', ended_at = clock_timestamp(), ended_by = NULLIF($2, '')::uuid
		 WHERE id = $1 AND status = 'confirmed'`, block, scope.UserID)
	if err != nil {
		return fmt.Errorf("let the car's slot go: %w", err)
	}
	return nil
}

// CancelTestDrive calls a drive off before it starts, with why; the slot
// goes back to the ledger.
func CancelTestDrive(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, driveID, why string) error {
	if !scope.Role.SellsVehicles() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return err
	}
	why = strings.TrimSpace(why)
	if why == "" || utf8.RuneCountInString(why) > 500 {
		return errDriveNote
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		d, err := workDriveTx(ctx, tx, driveID)
		if err != nil {
			return err
		}
		if d.State != DriveBooked {
			return errDriveState
		}
		if _, err := tx.Exec(ctx, `INSERT INTO test_drive_events (shop_id, test_drive_id, kind, by_user, note)
			VALUES ($1, $2, 'cancelled', NULLIF($3, '')::uuid, $4)`, scope.ShopID, d.ID, scope.UserID, why); err != nil {
			return err
		}
		return endDriveBlockTx(ctx, tx, scope, d.BlockID)
	})
}

// TestDriveByID reads a drive for whoever sells cars. Reading is Core's.
func TestDriveByID(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string) (TestDrive, error) {
	if !scope.Role.SellsVehicles() {
		return TestDrive{}, access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return TestDrive{}, ErrNotFound
	}
	var out TestDrive
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = testDriveTx(ctx, tx, id)
		return err
	})
	return out, err
}

// TestDrivesOfUnit is every drive of a car in stock, newest first. For
// whoever sells cars. Reading is Core's.
func TestDrivesOfUnit(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, unitID string) ([]TestDrive, error) {
	if !scope.Role.SellsVehicles() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(unitID) {
		return nil, ErrNotFound
	}
	var out []TestDrive
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, testDriveColumns+` WHERE d.stock_unit_id = $1 ORDER BY d.starts_at DESC LIMIT 200`, unitID)
		if err != nil {
			return fmt.Errorf("list test drives: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			d, err := scanTestDrive(rows)
			if err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

// SetTestDriveRules sets the youngest driver the shop lets out and whether
// a car reserved for somebody else may be driven by another. For whoever
// runs the shop.
func SetTestDriveRules(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, minimumAge int, reservedBlocks bool) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return err
	}
	if minimumAge < 16 || minimumAge > 99 {
		return errDriveAge
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO test_drive_settings (shop_id, minimum_age, reserved_blocks, changed_by)
			VALUES ($1, $2, $3, NULLIF($4, '')::uuid)`, scope.ShopID, minimumAge, reservedBlocks, scope.UserID)
		return err
	})
}

// TestDriveRules reads the youngest driver and whether a reserved car is
// blocked for others. For whoever sells cars. Reading is Core's.
func TestDriveRules(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) (minimumAge int, reservedBlocks bool, err error) {
	if !scope.Role.SellsVehicles() {
		return 0, false, access.ErrForbidden
	}
	err = database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		minimumAge, reservedBlocks, err = testDriveSettingsTx(ctx, tx)
		return err
	})
	return minimumAge, reservedBlocks, err
}

// testDrivesOverdue is the action queue's rows for a drive out past its
// end, in the shape dealerWaiting reads, for everyone who sells cars.
const testDrivesOverdue = `
	SELECT 'test_drive_overdue', 'test_drive', d.id::text,
	       ` + carNameSQL + `, d.ends_at, d.ends_at
	FROM test_drives d
	JOIN stock_units u ON u.id = d.stock_unit_id JOIN vehicles v ON v.id = u.vehicle_id
	LEFT JOIN vehicle_registrations reg ON reg.vehicle_id = v.id AND reg.valid_to IS NULL
	WHERE d.ends_at < clock_timestamp()
	  AND EXISTS (SELECT 1 FROM test_drive_events e WHERE e.test_drive_id = d.id AND e.kind = 'started')
	  AND NOT EXISTS (SELECT 1 FROM test_drive_events e WHERE e.test_drive_id = d.id AND e.kind = 'returned')`

// TestDriveTemplateName is the shipped test-drive agreement's name.
const TestDriveTemplateName = "Test-drive agreement (needs legal review)"

// shippedTestDriveTemplates are the shipped agreement's text by language:
// the driver, the time and the area, who carries the excess, fuel, and no
// alcohol or drugs. The blanks are the shop's to fill in; the text is a
// draft until the legal review (#221) has read it.
var shippedTestDriveTemplates = map[string]string{
	"en": `TEST-DRIVE AGREEMENT
{shop}, {shop_org}, lends the car below to the driver for a test drive.

Driver: {driver}
Car: {car}
From {from} to {to}.
Where: {area}

The driver has shown a valid driving licence for the car and drives it in person; nobody else drives it.
The driver keeps to traffic law and drives no faster than the conditions allow. No alcohol, drugs or medicines that affect driving.
Fines, tolls and congestion charges during the drive are the driver's.
Damage caused by the driver's negligence: the driver carries the insurance excess of ____. Damage is recorded on return as observed and the driver is told what was found before anything is charged.
Fuel or charge: ____.
The car is returned to {shop} by {to}.`,
	"sv": `PROVKÖRNINGSAVTAL
{shop}, {shop_org}, lånar ut bilen nedan till föraren för en provkörning.

Förare: {driver}
Bil: {car}
Från {from} till {to}.
Område: {area}

Föraren har visat ett giltigt körkort för bilen och kör den själv; ingen annan kör den.
Föraren följer trafikreglerna och kör efter förhållandena. Ingen alkohol, inga droger och inga läkemedel som påverkar körförmågan.
Böter, vägavgifter och trängselskatt under provkörningen betalas av föraren.
Skada som föraren orsakar genom vårdslöshet: föraren står för försäkringens självrisk på ____. Skador antecknas vid återlämningen som de iakttagits, och föraren får veta vad som hittats innan något debiteras.
Drivmedel eller laddning: ____.
Bilen lämnas tillbaka till {shop} senast {to}.`,
}

var shippedTestDriveNames = map[string]string{"sv": "Provkörningsavtal (behöver juridisk granskning)"}

// ShippedTestDriveTemplate is the shipped agreement's text in a language.
func ShippedTestDriveTemplate(locale string) string { return shippedTestDriveTemplates[locale] }

func ensureTestDriveTemplateTx(ctx context.Context, tx pgx.Tx, shopID string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('test_drive_templates:' || $1::text, 0))`, shopID); err != nil {
		return fmt.Errorf("lock templates: %w", err)
	}
	var some bool
	var locale string
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM document_templates WHERE kind = 'test_drive_agreement'), (SELECT locale FROM shops)`).
		Scan(&some, &locale); err != nil || some {
		return err
	}
	text, ok := shippedTestDriveTemplates[locale]
	if !ok {
		text = shippedTestDriveTemplates["en"]
	}
	name := TestDriveTemplateName
	if n, ok := shippedTestDriveNames[locale]; ok {
		name = n
	}
	var id string
	if err := tx.QueryRow(ctx, `INSERT INTO document_templates (shop_id, kind, name) VALUES ($1, 'test_drive_agreement', $2) RETURNING id`,
		shopID, name).Scan(&id); err != nil {
		return fmt.Errorf("make the shipped template: %w", err)
	}
	_, err := tx.Exec(ctx, `INSERT INTO document_template_versions (shop_id, template_id, version, body) VALUES ($1, $2, 1, $3)`, shopID, id, text)
	return err
}

// TestDriveTemplates lists the test-drive agreement templates, making the
// shipped one the first time. For whoever sells cars.
func TestDriveTemplates(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]DocumentTemplate, error) {
	if !scope.Role.SellsVehicles() {
		return nil, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return nil, err
	}
	if err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		return ensureTestDriveTemplateTx(ctx, tx, scope.ShopID)
	}); err != nil {
		return nil, err
	}
	return DocumentTemplates(ctx, pool, scope, DocTestDriveAgreement)
}

// ProduceTestDriveAgreement freezes the agreement for a drive from a
// template version, with the driver as its party, and ties it to the
// drive, in one transaction with the drive's car locked. replacing is the
// agreement the caller saw, or empty for none: made again, the new one
// supersedes it; when the drive's agreement is no longer that one --
// "make it" pressed twice, two people at once -- nothing more is made and
// the caller is told (ErrMadeMeanwhile), to read the drive again. The driver signs it on
// paper or in person (RecordSignature); a signing service that is down
// leaves it unsigned and the drive still bookable.
func ProduceTestDriveAgreement(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, driveID, templateVersionID, replacing string) (string, error) {
	if !scope.Role.SellsVehicles() {
		return "", access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return "", err
	}
	if err := mayDocument(scope.Role, scope, DocTestDriveAgreement, true); err != nil {
		return "", err
	}
	if !looksLikeUUID(driveID) || (templateVersionID != "" && !looksLikeUUID(templateVersionID)) || (replacing != "" && !looksLikeUUID(replacing)) {
		return "", ErrNotFound
	}
	var doc string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		d, err := workDriveTx(ctx, tx, driveID)
		if err != nil {
			return err
		}
		if d.State != DriveBooked {
			return errDriveState
		}
		if d.AgreementID != replacing {
			return ErrMadeMeanwhile
		}
		// None chosen: the shipped agreement, made here the first time --
		// in a write, never when a page is read.
		if templateVersionID == "" {
			if err := ensureTestDriveTemplateTx(ctx, tx, scope.ShopID); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `
				SELECT v.id FROM document_templates t JOIN document_template_versions v ON v.template_id = t.id
				WHERE t.kind = 'test_drive_agreement' ORDER BY t.created_at, t.id, v.version DESC LIMIT 1`).Scan(&templateVersionID); err != nil {
				return fmt.Errorf("read the shipped agreement: %w", err)
			}
		}
		var shop, org string
		if err := tx.QueryRow(ctx, `SELECT name, coalesce(org_number, '') FROM shops`).Scan(&shop, &org); err != nil {
			return err
		}
		loc, err := shopLocationTx(ctx, tx)
		if err != nil {
			return err
		}
		area := d.Area
		if area == "" {
			area = "-"
		}
		doc, err = produceDocumentTx(ctx, tx, scope, NewDocumentRecord{Kind: DocTestDriveAgreement, SubjectKind: "test_drive",
			SubjectID: driveID, TemplateVersionID: templateVersionID, Supersedes: d.AgreementID,
			Title:   d.Car + " " + d.StartsAt.In(loc).Format("2006-01-02 15:04"),
			Parties: []DocumentPartyRole{{PartyID: d.DriverID, Role: "driver"}},
			Data: map[string]string{"shop": shop, "shop_org": org, "driver": d.Driver, "car": d.Car, "area": area,
				"from": d.StartsAt.In(loc).Format("2006-01-02 15:04"), "to": d.EndsAt.In(loc).Format("2006-01-02 15:04")}})
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE test_drives SET agreement_id = $2 WHERE id = $1`, d.ID, doc)
		return err
	})
	return doc, err
}
