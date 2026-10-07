package workshop

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// A part used to leave the shelf only on paper, at invoicing. Between the
// bench and the invoice the books said it was on the shelf; a part fitted to
// a job never invoiced, or one that went missing, never showed anywhere. The
// technician now takes it out to the job when it comes off the shelf.
//
// Taking out is a "consumed" movement on the job, and putting back a
// "put_back" one, onto the shelf. The kind was allowed and counted a release
// before anything wrote it (0028), so a rollback reads the shelf right.

// JobPart is one part on a job: what was priced, and what was taken out.
type JobPart struct {
	PartID   string
	Number   string
	Name     string
	Unit     string
	Priced   float64
	TakenOut float64
}

// NotTakenOut is priced and still on the shelf.
func (p JobPart) NotTakenOut() bool { return p.Priced > p.TakenOut }

// Remaining is what is priced and still on the shelf.
func (p JobPart) Remaining() float64 { return max(p.Priced-p.TakenOut, 0) }

// NotPriced is taken out to the job and not on its lines: the cue that stops
// a part being fitted for nothing.
func (p JobPart) NotPriced() bool { return p.TakenOut > p.Priced }

// PartsOnJob lists the parts priced on a job or taken out to it.
func PartsOnJob(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string) ([]JobPart, error) {
	var out []JobPart
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT p.id, p.number, p.name, p.unit,
			       coalesce((SELECT sum(l.quantity) FROM work_order_lines l
			                 WHERE l.work_order_id = $1 AND l.part_id = p.id), 0)::float8,
			       coalesce((SELECT -sum(m.quantity) FROM stock_movements m
			                 WHERE m.work_order_id = $1 AND m.part_id = p.id AND m.kind IN ('consumed', 'put_back')), 0)::float8
			FROM parts p
			WHERE p.id IN (SELECT part_id FROM work_order_lines WHERE work_order_id = $1 AND part_id IS NOT NULL
			               UNION
			               SELECT part_id FROM stock_movements WHERE work_order_id = $1 AND kind IN ('consumed', 'put_back'))
			ORDER BY p.number`, jobID)
		if err != nil {
			return fmt.Errorf("parts on job: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var jp JobPart
			if err := rows.Scan(&jp.PartID, &jp.Number, &jp.Name, &jp.Unit, &jp.Priced, &jp.TakenOut); err != nil {
				return fmt.Errorf("scan part on job: %w", err)
			}
			out = append(out, jp)
		}
		return rows.Err()
	})
	return out, err
}

// TakeOut takes a part off the shelf to a job.
//
// More than was priced, or than was reserved, is allowed: the technician
// found it needed, and the job shows it as taken out and not priced for the
// front desk. A take-out releases the job's reservation of the part as far
// as it goes, so nothing is both put aside and used.
func TakeOut(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID, partID string, quantity float64) error {
	if !scope.Role.HandlesParts() {
		return access.ErrForbidden
	}
	if quantity <= 0 {
		return fmt.Errorf("%w: take out a positive quantity", ErrInvalid)
	}
	if !looksLikeUUID(partID) {
		return ErrNotFound
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := openForParts(ctx, tx, jobID); err != nil {
			return err
		}
		var reserved float64
		if err := tx.QueryRow(ctx, `
			SELECT coalesce(sum(quantity), 0)::float8 FROM stock_movements
			WHERE work_order_id = $1 AND part_id = $2 AND kind IN ('reserved', 'unreserved')`,
			jobID, partID).Scan(&reserved); err != nil {
			return fmt.Errorf("read reservation: %w", err)
		}
		if release := min(reserved, quantity); release > 0 {
			if err := moveTx(ctx, tx, scope, Movement{Kind: "unreserved", Quantity: -release,
				Note: "Taken out to the job"}, partID, jobID, ""); err != nil {
				return err
			}
		}
		return moveTx(ctx, tx, scope, Movement{Kind: "consumed", Quantity: -quantity,
			Note: "Taken out to the job"}, partID, jobID, "")
	})
}

// PutBack returns a part taken out to a job to the shelf: not needed after
// all. A movement, not a deletion, and never more than was taken out.
func PutBack(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID, partID string, quantity float64) error {
	if !scope.Role.HandlesParts() {
		return access.ErrForbidden
	}
	if quantity <= 0 {
		return fmt.Errorf("%w: put back a positive quantity", ErrInvalid)
	}
	if !looksLikeUUID(partID) {
		return ErrNotFound
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := openForParts(ctx, tx, jobID); err != nil {
			return err
		}
		// The part's row, locked as every movement locks it, before reading
		// what was taken out: two put-backs at once cannot both pass.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM parts WHERE id = $1 FOR UPDATE`, partID); err != nil {
			return fmt.Errorf("lock part: %w", err)
		}
		var taken float64
		if err := tx.QueryRow(ctx, `
			SELECT coalesce(-sum(quantity), 0)::float8 FROM stock_movements
			WHERE work_order_id = $1 AND part_id = $2 AND kind IN ('consumed', 'put_back')`, jobID, partID).Scan(&taken); err != nil {
			return fmt.Errorf("read taken out: %w", err)
		}
		if quantity > taken+1e-9 {
			return fmt.Errorf("%w: only %v was taken out to this job", ErrInvalid, taken)
		}
		return moveTx(ctx, tx, scope, Movement{Kind: "put_back", Quantity: quantity,
			Note: "Put back on the shelf"}, partID, jobID, "")
	})
}

// TakeOutByCode takes out the part a scanned or typed code names: the
// label on the box, read by a handheld scanner that types it. It answers
// with the part's number and name only -- a part also carries its cost,
// which is not the technician's business.
func TakeOutByCode(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID, code string, quantity float64) (number, name string, err error) {
	if !scope.Role.HandlesParts() {
		return "", "", access.ErrForbidden
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return "", "", fmt.Errorf("%w: scan or type the part's code", ErrInvalid)
	}
	var partID string
	err = database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		// The same match as the parts desk's scan page.
		err := tx.QueryRow(ctx, `
			SELECT p.id, p.number, p.name FROM parts p
			WHERE p.active AND (p.number = $1
			   OR EXISTS (SELECT 1 FROM part_codes c WHERE c.part_id = p.id AND c.code = $1))
			LIMIT 1`, code).Scan(&partID, &number, &name)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: no part has the code %s", ErrInvalid, code)
		}
		return err
	})
	if err != nil {
		return "", "", err
	}
	return number, name, TakeOut(ctx, pool, scope, jobID, partID, quantity)
}

// openForParts refuses a job nothing can be taken out to any more.
func openForParts(ctx context.Context, tx pgx.Tx, jobID string) error {
	var state State
	if err := tx.QueryRow(ctx, `SELECT state FROM work_orders WHERE id = $1 FOR UPDATE`, jobID).Scan(&state); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("lock order: %w", err)
	}
	switch state {
	case StateInvoiced, StateClosed, StateCancelled:
		return fmt.Errorf("%w: the job is %s; its parts are settled", ErrInvalid, state)
	}
	return nil
}
