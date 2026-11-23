package workshop

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// The supplier's link (#236): a car at the painter or the tyre shop is a step
// somebody outside the shop is doing, and the shop should not have to ring to
// learn it is finished. The link opens one step; it shows the car and the step
// and nothing the shop earns, owes or knows about a person, and the supplier
// can say "done" on it. Nothing else.

// reconSupplierLinkLifetime is long enough for a car that waits a fortnight
// in a queue.
const reconSupplierLinkLifetime = 30 * 24 * time.Hour

var errReconNotOutside = fmt.Errorf("%w: only a step done outside the shop has a supplier's link", ErrInvalid)

// CreateReconSupplierLink makes the link for an outside step and returns its
// token, once. A new one closes the step's earlier links.
func CreateReconSupplierLink(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, stepID string) (string, error) {
	if !scope.Role.BuysVehicles() && !scope.Role.SellsVehicles() {
		return "", access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return "", err
	}
	if !looksLikeUUID(stepID) {
		return "", ErrNotFound
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var unit, kind string
		if err := tx.QueryRow(ctx, `SELECT stock_unit_id, kind FROM recon_steps WHERE id = $1`, stepID).Scan(&unit, &kind); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if kind != ReconOutside {
			return errReconNotOutside
		}
		if _, err := unitOpenTx(ctx, tx, unit); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE recon_supplier_links SET revoked_at = clock_timestamp() WHERE step_id = $1 AND revoked_at IS NULL`, stepID); err != nil {
			return fmt.Errorf("close earlier links: %w", err)
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO recon_supplier_links (shop_id, step_id, token_sha256, created_by, expires_at)
			VALUES ($1, $2, $3, NULLIF($4, '')::uuid, clock_timestamp() + $5::interval)`,
			scope.ShopID, stepID, sum[:], scope.UserID, fmt.Sprintf("%d seconds", int(reconSupplierLinkLifetime.Seconds())))
		return err
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

// SupplierView is all a supplier is shown: the shop, the car, the step and
// where it stands. No price, no customer, no other step.
type SupplierView struct {
	ShopName  string
	ShopPhone string
	Car       string
	StepName  string
	State     string
	Since     time.Time
	DueOn     *time.Time
	CanFinish bool
}

// supplierStepTx finds the step a token opens, locked so a double click is
// told apart. Wrong, expired, closed and a step that is not outside any more
// are one answer.
func supplierStepTx(ctx context.Context, tx pgx.Tx, token string) (stepID, unitID string, v SupplierView, err error) {
	if token == "" {
		return "", "", v, ErrShareNotUsable
	}
	sum := sha256.Sum256([]byte(token))
	var last *string
	err = tx.QueryRow(ctx, `
		SELECT s.id, s.stock_unit_id, z.name, coalesce(z.phone, ''), `+carNameSQL+`, s.name, s.due_on,
		       e.kind, coalesce(e.at, s.created_at)
		FROM recon_supplier_links l
		JOIN recon_steps s   ON s.id = l.step_id AND s.kind = 'outside'
		JOIN stock_units u   ON u.id = s.stock_unit_id
		JOIN vehicles v      ON v.id = u.vehicle_id
		JOIN shops z         ON z.id = s.shop_id
		LEFT JOIN vehicle_registrations reg ON reg.vehicle_id = v.id AND reg.valid_to IS NULL
		LEFT JOIN LATERAL (SELECT x.kind, x.at FROM recon_step_events x WHERE x.step_id = s.id ORDER BY x.seq DESC LIMIT 1) e ON true
		WHERE l.token_sha256 = $1 AND l.revoked_at IS NULL AND l.expires_at > clock_timestamp()`, sum[:]).
		Scan(&stepID, &unitID, &v.ShopName, &v.ShopPhone, &v.Car, &v.StepName, &v.DueOn, &last, &v.Since)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", v, ErrShareNotUsable
	}
	if err != nil {
		return "", "", v, fmt.Errorf("read supplier link: %w", err)
	}
	v.State = reconStateOf(last)
	v.CanFinish = v.State == ReconInProgress
	return stepID, unitID, v, nil
}

// SupplierStep answers the supplier's link.
func SupplierStep(ctx context.Context, pool *pgxpool.Pool, shopID, token string) (SupplierView, error) {
	var out SupplierView
	err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, v, err := supplierStepTx(ctx, tx, token)
		out = v
		return err
	})
	return out, err
}

// SupplierFinishStep is the supplier saying the step is done. It is the one
// move they have, from a step that is under way; a step not started, or
// already finished, is refused rather than quietly accepted, so a stale page
// cannot rewrite the shop's plan. The move is a row like any other; no user
// is on it, which is how a supplier's is told from the shop's own.
func SupplierFinishStep(ctx context.Context, pool *pgxpool.Pool, shopID, token string) error {
	return database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		stepID, unitID, _, err := supplierStepTx(ctx, tx, token)
		if err != nil {
			return err
		}
		// Serialise with the shop's own moves of the same step: the car's
		// cycle is what they lock too.
		if _, err := unitOpenTx(ctx, tx, unitID); err != nil {
			return err
		}
		var last *string
		if err := tx.QueryRow(ctx, `SELECT kind FROM recon_step_events WHERE step_id = $1 ORDER BY seq DESC LIMIT 1`, stepID).Scan(&last); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if reconNext(reconStateOf(last), ReconFinish) == "" {
			return errReconMove
		}
		_, err = tx.Exec(ctx, `INSERT INTO recon_step_events (shop_id, step_id, kind) VALUES (current_setting('app.current_shop')::uuid, $1, 'done')`, stepID)
		return err
	})
}
