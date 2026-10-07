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

// A final check before a car is ready, as a checklist the shop may require.
// Many shops check the work before the car goes back -- a road test, the
// wheel nuts torqued, no warning lights -- and FreeSMS went straight from
// "it is ready" to the board.

// FinalCheckSettings is whether a shop requires a final check, which
// checklist it is, and whether somebody else must do it.
type FinalCheckSettings struct {
	TemplateID string // empty: not required
	ByAnother  bool
}

// FinalCheckSettingsFor reads the shop's setting.
func FinalCheckSettingsFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) (FinalCheckSettings, error) {
	if !scope.Role.RunsTheShop() {
		return FinalCheckSettings{}, access.ErrForbidden
	}
	var s FinalCheckSettings
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		s, err = finalCheckSettingsTx(ctx, tx, scope.ShopID)
		return err
	})
	return s, err
}

func finalCheckSettingsTx(ctx context.Context, tx pgx.Tx, shopID string) (FinalCheckSettings, error) {
	var s FinalCheckSettings
	var id *string
	if err := tx.QueryRow(ctx, `SELECT final_check_template_id, final_check_by_another FROM shops WHERE id = $1`,
		shopID).Scan(&id, &s.ByAnother); err != nil {
		return s, fmt.Errorf("read final check setting: %w", err)
	}
	s.TemplateID = deref(id)
	return s, nil
}

// SaveFinalCheck sets it. An empty template turns it off. Whoever runs the
// shop decides how work is signed off.
func SaveFinalCheck(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, s FinalCheckSettings) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	s.TemplateID = strings.TrimSpace(s.TemplateID)
	if s.TemplateID != "" && !looksLikeUUID(s.TemplateID) {
		return fmt.Errorf("%w: no such checklist", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if s.TemplateID != "" {
			var active bool
			if err := tx.QueryRow(ctx, `SELECT active FROM inspection_templates WHERE id = $1`, s.TemplateID).Scan(&active); err != nil || !active {
				return fmt.Errorf("%w: no such checklist", ErrInvalid)
			}
		}
		_, err := tx.Exec(ctx, `
			UPDATE shops SET final_check_template_id = nullif($2, '')::uuid, final_check_by_another = $3
			WHERE id = $1`, scope.ShopID, s.TemplateID, s.ByAnother)
		return err
	})
}

// FinalCheck is where a job stands against the shop's final check.
type FinalCheck struct {
	Required bool
	Done     bool
	By       string
	// Why it is not done, as a catalogue key, when it is required and is not.
	Missing string
}

// FinalCheckFor reads where a job stands.
func FinalCheckFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string) (FinalCheck, error) {
	var fc FinalCheck
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		fc, err = finalCheckTx(ctx, tx, scope.ShopID, jobID)
		return err
	})
	return fc, err
}

func finalCheckTx(ctx context.Context, tx pgx.Tx, shopID, jobID string) (FinalCheck, error) {
	s, err := finalCheckSettingsTx(ctx, tx, shopID)
	if err != nil || s.TemplateID == "" {
		return FinalCheck{}, err
	}
	fc := FinalCheck{Required: true}
	var inspectionID, performer, by string
	var assigned *string
	err = tx.QueryRow(ctx, `
		SELECT i.id, i.performed_by, coalesce(p.display_name, ''), w.assigned_to
		FROM inspections i
		JOIN work_orders w ON w.id = i.work_order_id
		LEFT JOIN users u ON u.id = i.performed_by
		LEFT JOIN people p ON p.id = u.person_id
		WHERE i.work_order_id = $1 AND i.template_id = $2 AND i.completed_at IS NOT NULL
		ORDER BY i.completed_at DESC LIMIT 1`, jobID, s.TemplateID).Scan(&inspectionID, &performer, &by, &assigned)
	if errors.Is(err, pgx.ErrNoRows) {
		fc.Missing = "The final check has not been done."
		return fc, nil
	}
	if err != nil {
		return fc, fmt.Errorf("read final check: %w", err)
	}
	var failed int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM inspection_items WHERE inspection_id = $1 AND status = 'fail'`,
		inspectionID).Scan(&failed); err != nil {
		return fc, fmt.Errorf("read final check items: %w", err)
	}
	switch {
	case failed > 0:
		fc.Missing = "The final check failed. Put right what it found, then check again."
	case s.ByAnother && assigned != nil && *assigned == performer:
		fc.Missing = "The final check has to be done by somebody other than whoever did the work."
	default:
		fc.Done, fc.By = true, by
	}
	return fc, nil
}

// findingsFromFinalCheckTx turns a completed final check's failed items into
// findings, so the job goes back to work with what was found in front of
// whoever picks it up. Any other checklist is left alone.
func findingsFromFinalCheckTx(ctx context.Context, tx pgx.Tx, scope access.Scope, inspectionID string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO findings (shop_id, work_order_id, note, reported_by)
		SELECT i.shop_id, i.work_order_id,
		       'Slutkontroll: ' || it.label || coalesce(': ' || nullif(trim(it.note), ''), ''),
		       i.performed_by
		FROM inspections i
		JOIN shops s ON s.id = i.shop_id AND s.final_check_template_id = i.template_id
		JOIN inspection_items it ON it.inspection_id = i.id AND it.status = 'fail'
		WHERE i.id = $1`, inspectionID)
	if err != nil {
		return fmt.Errorf("findings from the final check: %w", err)
	}
	return nil
}
