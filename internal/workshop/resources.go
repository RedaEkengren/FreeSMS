package workshop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// Resources and the one availability ledger (#149). What the business
// controls -- a stock car, a fleet vehicle, a trailer -- is a resource, and
// resource_blocks says when it is taken and by what. The database refuses an
// overlap (migration 0070), so two bookings made at the same moment cannot
// both stand. A job on a vehicle that is a resource blocks it for as long as
// the job is open, by a trigger in the job's own transaction; the car's open
// jobs share one block, which ends with the last of them (0073).

// The kinds of block.
const (
	BlockRental    = "rental"
	BlockTestDrive = "test_drive"
	BlockWorkshop  = "workshop"
	BlockDelivery  = "delivery"
	BlockService   = "service"
)

// Its states. Held and confirmed are active: they block.
const (
	BlockHeld       = "held"
	BlockConfirmed  = "confirmed"
	BlockReleased   = "released"
	BlockExpired    = "expired"
	BlockOverridden = "overridden"
)

// The refusal of an overlap, naming the use in the way -- its kind, which
// any role that may book can be told; not whose rental it is.
var (
	errTakenByRental    = fmt.Errorf("%w: it is taken then by a rental", ErrInvalid)
	errTakenByTestDrive = fmt.Errorf("%w: it is taken then by a test drive", ErrInvalid)
	errTakenByWorkshop  = fmt.Errorf("%w: it is taken then by a job in the workshop", ErrInvalid)
	errTakenByDelivery  = fmt.Errorf("%w: it is taken then by a delivery", ErrInvalid)
	errTakenByService   = fmt.Errorf("%w: it is taken then by a service block", ErrInvalid)
	errTaken            = fmt.Errorf("%w: it is taken then", ErrInvalid)
)

// ErrResourceTaken is any of them: the time asked for overlaps a use of the
// resource that is already booked or held.
var ErrResourceTaken = errors.New("workshop: the resource is taken then")

type takenError struct{ sentence error }

func (e takenError) Error() string { return e.sentence.Error() }
func (e takenError) Unwrap() []error {
	return []error{e.sentence, ErrResourceTaken}
}

func takenBy(kind string) error {
	s := errTaken
	switch kind {
	case BlockRental:
		s = errTakenByRental
	case BlockTestDrive:
		s = errTakenByTestDrive
	case BlockWorkshop:
		s = errTakenByWorkshop
	case BlockDelivery:
		s = errTakenByDelivery
	case BlockService:
		s = errTakenByService
	}
	return takenError{s}
}

// blockConflict turns the ledger's refusal into a sentence: from the
// constraint, or from the job trigger, which names the use in its detail.
// Anything else is returned as it was.
func blockConflict(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23P01" && pg.ConstraintName == "resource_blocks_no_overlap" {
		return takenBy(pg.Detail)
	}
	return err
}

// jobBlockError is an error from writing a job, with the ledger's refusal
// -- the job's car is a resource taken then -- as its sentence.
func jobBlockError(err error, what string) error {
	if c := blockConflict(err); c != err {
		return c
	}
	return fmt.Errorf("%s: %w", what, err)
}

// blockModule is the module a kind of block belongs to. A service block is
// Core's: a stock car and a fleet car both go for service.
func blockModule(kind string) access.Module {
	switch kind {
	case BlockRental:
		return access.Rental
	case BlockTestDrive, BlockDelivery:
		return access.Dealer
	}
	return ""
}

// mayBook checks roles, and the scope's modules, for booking, confirming or
// releasing a block of kind: the rental desk rents, the salesperson takes a
// car out on a test drive and delivers it, and whoever looks after stock or
// fleet sends a car for service. A job blocks its car through the job.
func mayBook(roles access.Roles, scope access.Scope, kind string) error {
	var may bool
	switch kind {
	case BlockRental:
		may = roles.HandsOverRentals()
	case BlockTestDrive, BlockDelivery:
		may = roles.SellsVehicles()
	case BlockService:
		may = roles.HandsOverRentals() || roles.BuysVehicles()
	case BlockWorkshop:
		return fmt.Errorf("%w: a job in the workshop blocks its car through the job", ErrInvalid)
	default:
		return fmt.Errorf("%w: that is not a use a resource can be booked for", ErrInvalid)
	}
	if !may {
		return access.ErrForbidden
	}
	// Written out per module, so the permission contract sees the gate.
	switch blockModule(kind) {
	case access.Rental:
		return scope.Require(access.Rental)
	case access.Dealer:
		return scope.Require(access.Dealer)
	}
	return nil
}

// NewResource is a thing to make bookable: a vehicle the business controls,
// or an item with a name of its own.
type NewResource struct {
	VehicleID string
	Name      string
}

// CreateResource makes a resource. A vehicle is one resource however many
// times it is bought and sold; making it again finds the one there is, and
// brings it back into use if it was taken out of it -- the car bought back.
// The period it was out of use stays (resource_retirements).
func CreateResource(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, n NewResource) (string, error) {
	if !scope.Role.KeepsResources() {
		return "", access.ErrForbidden
	}
	n.Name = strings.TrimSpace(n.Name)
	if n.VehicleID == "" && (n.Name == "" || utf8.RuneCountInString(n.Name) > 120) {
		return "", fmt.Errorf("%w: a resource that is not a vehicle needs a name, in 120 characters or fewer", ErrInvalid)
	}
	if n.VehicleID != "" && !looksLikeUUID(n.VehicleID) {
		return "", ErrNotFound
	}
	var id string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if n.VehicleID == "" {
			return tx.QueryRow(ctx, `INSERT INTO resources (shop_id, kind, name, created_by) VALUES ($1, 'item', $2, NULLIF($3, '')::uuid) RETURNING id`,
				scope.ShopID, n.Name, scope.UserID).Scan(&id)
		}
		var err error
		id, err = vehicleResourceTx(ctx, tx, scope, n.VehicleID)
		return err
	})
	return id, err
}

// vehicleResourceTx is CreateResource for a vehicle, in the caller's
// transaction: the vehicle's one resource, made or brought back into use.
// A stock unit opens with it (#160).
func vehicleResourceTx(ctx context.Context, tx pgx.Tx, scope access.Scope, vehicleID string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO resources (shop_id, kind, vehicle_id, created_by)
		SELECT $1, 'vehicle', v.id, NULLIF($3, '')::uuid FROM vehicles v WHERE v.id = $2 AND v.merged_into IS NULL
		ON CONFLICT (vehicle_id) DO NOTHING RETURNING id`, scope.ShopID, vehicleID, scope.UserID).Scan(&id)
	if !errors.Is(err, pgx.ErrNoRows) {
		return id, err
	}
	var retired bool
	err = tx.QueryRow(ctx, `SELECT id, retired_at IS NOT NULL FROM resources WHERE vehicle_id = $1 FOR UPDATE`, vehicleID).Scan(&id, &retired)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil || !retired {
		return id, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE resource_retirements SET returned_at = clock_timestamp(), returned_by = NULLIF($2, '')::uuid
		WHERE resource_id = $1 AND returned_at IS NULL`, id, scope.UserID); err != nil {
		return "", fmt.Errorf("bring back into use: %w", err)
	}
	_, err = tx.Exec(ctx, `UPDATE resources SET retired_at = NULL, retired_by = NULL WHERE id = $1`, id)
	return id, err
}

// RetireResource takes a resource out of use: sold, scrapped, off fleet.
// Refused while anything is still booked or held on it from now on, so a
// renter is not left with a booking for a car that has gone; release or
// override those first. Its past stays, and so does this: each period out
// of use is a row, and CreateResource brings a vehicle back.
func RetireResource(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string) error {
	if !scope.Role.KeepsResources() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return ErrNotFound
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		return retireResourceTx(ctx, tx, scope, id)
	})
}

// retireResourceTx is RetireResource in the caller's transaction: a stock
// unit's cycle ends with it (#160).
func retireResourceTx(ctx context.Context, tx pgx.Tx, scope access.Scope, id string) error {
	var retired bool
	err := tx.QueryRow(ctx, `SELECT retired_at IS NOT NULL FROM resources WHERE id = $1 FOR UPDATE`, id).Scan(&retired)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("read resource: %w", err)
	}
	if retired {
		return nil
	}
	if _, err := tx.Exec(ctx, `SELECT release_expired_holds($1, tstzrange(clock_timestamp(), NULL))`, id); err != nil {
		return fmt.Errorf("release expired holds: %w", err)
	}
	var ahead int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM resource_blocks
		WHERE resource_id = $1 AND status IN ('held', 'confirmed') AND period && tstzrange(clock_timestamp(), NULL)`, id).Scan(&ahead); err != nil {
		return fmt.Errorf("read what is booked: %w", err)
	}
	if ahead > 0 {
		return fmt.Errorf("%w: something is still booked on it; release or replan that first", ErrInvalid)
	}
	// The period out of use is the row; the column is what it says now,
	// which booking and the job trigger read.
	if _, err := tx.Exec(ctx, `INSERT INTO resource_retirements (shop_id, resource_id, retired_by) VALUES ($1, $2, NULLIF($3, '')::uuid)`,
		scope.ShopID, id, scope.UserID); err != nil {
		return fmt.Errorf("take out of use: %w", err)
	}
	_, err = tx.Exec(ctx, `UPDATE resources SET retired_at = now(), retired_by = NULLIF($2, '')::uuid WHERE id = $1`, id, scope.UserID)
	return err
}

// ResourceBooking is a use to block a resource for. From and To are the use itself;
// the buffers before and after are added to what is stored, and To zero is
// open-ended. HoldUntil, set, makes it a hold that runs out then unless
// confirmed. Key is the request's own: replayed, it finds the block it made.
type ResourceBooking struct {
	ResourceID   string
	Kind         string
	From, To     time.Time
	BufferBefore time.Duration
	BufferAfter  time.Duration
	HoldUntil    *time.Time
	SourceKind   string
	SourceID     string
	Reason       string
	Key          string
}

// BookResource blocks a resource for a use. Refused with a sentence naming
// what is in the way when the period, buffers included, overlaps anything
// booked or held; a hold that has run out is let go first, in the same
// transaction. Two bookings of the same time made at once: at most one
// stands, because the database decides.
func BookResource(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, b ResourceBooking) (string, error) {
	if err := mayBook(scope.Role, scope, b.Kind); err != nil {
		return "", err
	}
	if !looksLikeUUID(b.ResourceID) || (b.SourceID != "" && !looksLikeUUID(b.SourceID)) {
		return "", ErrNotFound
	}
	from := b.From.Add(-b.BufferBefore)
	var to *time.Time
	if !b.To.IsZero() {
		t := b.To.Add(b.BufferAfter)
		to = &t
	}
	switch {
	case b.From.IsZero():
		return "", fmt.Errorf("%w: say when it starts", ErrInvalid)
	case b.BufferBefore < 0 || b.BufferAfter < 0:
		return "", fmt.Errorf("%w: a buffer is time added, not taken away", ErrInvalid)
	case to != nil && !b.To.After(b.From):
		return "", fmt.Errorf("%w: it has to end after it starts", ErrInvalid)
	case b.HoldUntil != nil && !b.HoldUntil.After(time.Now()):
		return "", fmt.Errorf("%w: a hold has to run out later than now", ErrInvalid)
	case (b.SourceKind == "") != (b.SourceID == ""):
		return "", fmt.Errorf("%w: what it is for needs both its kind and which one", ErrInvalid)
	}
	reason, err := checkNote(b.Reason)
	if err != nil {
		return "", err
	}
	key := strings.TrimSpace(b.Key)
	if len(key) > 100 {
		return "", fmt.Errorf("%w: the request key is too long", ErrInvalid)
	}
	status := BlockConfirmed
	if b.HoldUntil != nil {
		status = BlockHeld
	}
	period := pgtype.Range[pgtype.Timestamptz]{
		Lower: pgtype.Timestamptz{Time: from, Valid: true}, LowerType: pgtype.Inclusive,
		UpperType: pgtype.Unbounded, Valid: true,
	}
	if to != nil {
		period.Upper, period.UpperType = pgtype.Timestamptz{Time: *to, Valid: true}, pgtype.Exclusive
	}

	var id string
	err = database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		// The resource first, locked, then its blocks: the order every
		// write to it takes. Letting a hold go before taking the resource
		// waits on a rental that holds the resource and wants the hold.
		var retired bool
		err := tx.QueryRow(ctx, `SELECT retired_at IS NOT NULL FROM resources WHERE id = $1 FOR UPDATE`, b.ResourceID).Scan(&retired)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read resource: %w", err)
		}
		// The request read once the resource is held: the same request
		// still in flight has committed by then, and its block is the
		// answer.
		if key != "" {
			err := tx.QueryRow(ctx, `SELECT id FROM resource_blocks WHERE request_key = $1`, key).Scan(&id)
			if err == nil {
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("read the request: %w", err)
			}
		}
		if retired {
			return fmt.Errorf("%w: that has been taken out of use", ErrInvalid)
		}
		if _, err := tx.Exec(ctx, `SELECT release_expired_holds($1, $2)`, b.ResourceID, period); err != nil {
			return fmt.Errorf("release expired holds: %w", err)
		}
		// What is in the way, read first so the refusal can name it; the
		// constraint is what decides, and catches what this misses.
		var taken string
		err = tx.QueryRow(ctx, `
			SELECT kind FROM resource_blocks
			WHERE resource_id = $1 AND status IN ('held', 'confirmed') AND period && $2
			ORDER BY lower(period) LIMIT 1`, b.ResourceID, period).Scan(&taken)
		if err == nil {
			return takenBy(taken)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read what is booked: %w", err)
		}
		// In a savepoint, for the one replay the read above cannot see: the
		// same key in flight on another resource, which this resource's
		// lock does not queue. (A replay on this resource waited on that
		// lock and found its block above.) The key's unique index refuses
		// it once the other commits, and the block found by the key is the
		// answer.
		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		err = sp.QueryRow(ctx, `
			INSERT INTO resource_blocks (shop_id, resource_id, kind, status, period, buffer_before, buffer_after, expires_at,
			                             source_kind, source_id, request_key, reason, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, nullif($9, ''), NULLIF($10, '')::uuid, nullif($11, ''), nullif($12, ''), NULLIF($13, '')::uuid)
			RETURNING id`,
			scope.ShopID, b.ResourceID, b.Kind, status, period, b.BufferBefore, b.BufferAfter, b.HoldUntil,
			b.SourceKind, b.SourceID, key, reason, scope.UserID).Scan(&id)
		if err == nil {
			return sp.Commit(ctx)
		}
		var pg *pgconn.PgError
		if key != "" && errors.As(err, &pg) && (pg.Code == "23505" || pg.Code == "23P01") {
			if rerr := sp.Rollback(ctx); rerr != nil {
				return rerr
			}
			switch ferr := tx.QueryRow(ctx, `SELECT id FROM resource_blocks WHERE request_key = $1`, key).Scan(&id); {
			case ferr == nil:
				return nil
			case !errors.Is(ferr, pgx.ErrNoRows):
				return fmt.Errorf("read the request: %w", ferr)
			}
		}
		return blockConflict(err)
	})
	return id, err
}

// blockKindTx reads a block's kind and state, locked: its resource first,
// then the block, the order every write to a resource takes. A block's
// resource never changes, so reading which one unlocked is safe.
func blockKindTx(ctx context.Context, tx pgx.Tx, id string) (kind, status string, expired bool, err error) {
	if _, err = lockResourceOfTx(ctx, tx, `SELECT resource_id FROM resource_blocks WHERE id = $1`, id); err != nil {
		return "", "", false, err
	}
	err = tx.QueryRow(ctx, `
		SELECT kind, status, status = 'held' AND expires_at <= clock_timestamp()
		FROM resource_blocks WHERE id = $1 FOR UPDATE`, id).Scan(&kind, &status, &expired)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return kind, status, expired, err
}

// ConfirmBlock makes a hold a booking. A hold that has run out is not there
// to confirm: somebody else may have the time by now.
func ConfirmBlock(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string) error {
	if !looksLikeUUID(id) {
		return ErrNotFound
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		kind, status, expired, err := blockKindTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := mayBook(scope.Role, scope, kind); err != nil {
			return err
		}
		switch {
		case status == BlockConfirmed:
			return nil
		case status != BlockHeld || expired:
			return fmt.Errorf("%w: the hold has run out or been let go; book it again", ErrInvalid)
		}
		_, err = tx.Exec(ctx, `UPDATE resource_blocks SET status = 'confirmed' WHERE id = $1`, id)
		return err
	})
}

// ReleaseBlock lets a booking or a hold go: the rental cancelled, the test
// drive not wanted. A job's block goes with the job.
func ReleaseBlock(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string) error {
	if !looksLikeUUID(id) {
		return ErrNotFound
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		kind, status, _, err := blockKindTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := mayBook(scope.Role, scope, kind); err != nil {
			return err
		}
		if status != BlockHeld && status != BlockConfirmed {
			return nil
		}
		_, err = tx.Exec(ctx, `UPDATE resource_blocks SET status = 'released', ended_at = clock_timestamp(), ended_by = NULLIF($2, '')::uuid WHERE id = $1`,
			id, scope.UserID)
		return err
	})
}

// OverrideBlock is the explicit replanning a conflict needs: it ends a block
// that is in the way -- from now, if it has begun -- or, given a new period,
// moves it there, and records who did it, when and why, as an audit event
// (#151). A car in the workshop is rented only after somebody who runs the
// shop has done this for the job's block.
func OverrideBlock(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string, moveFrom, moveTo time.Time, reason string) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return ErrNotFound
	}
	reason, err := checkReason(reason, fmt.Errorf("%w: say why it is replanned, in 300 characters or fewer", ErrInvalid))
	if err != nil {
		return err
	}
	move := !moveFrom.IsZero()
	if move && !moveTo.IsZero() && !moveTo.After(moveFrom) {
		return fmt.Errorf("%w: it has to end after it starts", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, status, expired, err := blockKindTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if (status != BlockHeld && status != BlockConfirmed) || expired {
			return fmt.Errorf("%w: that is no longer booked", ErrInvalid)
		}
		var oldFrom time.Time
		var oldTo *time.Time
		if err := tx.QueryRow(ctx, `SELECT lower(period), upper(period) FROM resource_blocks WHERE id = $1`, id).Scan(&oldFrom, &oldTo); err != nil {
			return fmt.Errorf("read the block: %w", err)
		}
		detail := map[string]any{"old_from": oldFrom, "old_to": oldTo}
		if move {
			var upper any
			if !moveTo.IsZero() {
				upper = moveTo
			}
			detail["change"], detail["new_from"], detail["new_to"] = "moved", moveFrom, upper
			if err := auditTx(ctx, tx, scope, id, "block.overridden", status, "moved", reason, detail); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE resource_blocks SET period = tstzrange($2, $3) WHERE id = $1`, id, moveFrom, upper)
			return blockConflict(err)
		}
		detail["change"] = "ended"
		if err := auditTx(ctx, tx, scope, id, "block.overridden", status, "ended", reason, detail); err != nil {
			return err
		}
		// Begun already: what has been stays, as it was, and it ends now.
		// Not begun: it does not happen.
		_, err = tx.Exec(ctx, `
			UPDATE resource_blocks
			   SET period = CASE WHEN lower(period) < clock_timestamp()
			                     THEN tstzrange(lower(period), clock_timestamp()) ELSE period END,
			       status = CASE WHEN lower(period) < clock_timestamp() THEN status ELSE 'overridden' END,
			       ended_at = clock_timestamp(), ended_by = $2
			 WHERE id = $1`, id, scope.UserID)
		return err
	})
}

// Block is one use of a resource, as it is read.
type Block struct {
	ID         string
	Kind       string
	Status     string
	From       time.Time
	To         *time.Time // nil: open-ended
	HeldUntil  *time.Time
	Before     time.Duration
	After      time.Duration
	SourceKind string
	SourceID   string
	// Why it was booked, which can say something about a person: for those
	// who run the shop.
	Reason string
}

// Active reports whether it blocks now: confirmed, or a hold not run out.
func (b Block) Active(now time.Time) bool {
	return b.Status == BlockConfirmed || (b.Status == BlockHeld && b.HeldUntil != nil && b.HeldUntil.After(now))
}

// ResourceBlocks lists a resource's blocks that overlap from..to (to zero:
// open-ended), oldest first, for those who keep or book resources. A hold
// that has run out reads as what it is -- not blocking -- whether or not
// anything has let it go yet.
func ResourceBlocks(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, resourceID string, from, to time.Time) ([]Block, error) {
	if !scope.Role.KeepsResources() && !scope.Role.SellsVehicles() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(resourceID) {
		return nil, ErrNotFound
	}
	var upper any
	if !to.IsZero() {
		upper = to
	}
	var out []Block
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id::text, kind,
			       CASE WHEN status = 'held' AND expires_at <= clock_timestamp() THEN 'expired' ELSE status END,
			       lower(period), upper(period), expires_at, buffer_before, buffer_after,
			       coalesce(source_kind, ''), coalesce(source_id::text, ''),
			       CASE WHEN $4 THEN coalesce(reason, '') ELSE '' END
			FROM resource_blocks
			WHERE resource_id = $1 AND period && tstzrange($2, $3)
			ORDER BY lower(period), id`, resourceID, from, upper, scope.Role.RunsTheShop())
		if err != nil {
			return fmt.Errorf("read blocks: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var b Block
			if err := rows.Scan(&b.ID, &b.Kind, &b.Status, &b.From, &b.To, &b.HeldUntil, &b.Before, &b.After,
				&b.SourceKind, &b.SourceID, &b.Reason); err != nil {
				return fmt.Errorf("scan block: %w", err)
			}
			out = append(out, b)
		}
		return rows.Err()
	})
	return out, err
}
