package workshop

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Limits on a checklist. Generous for a workshop, small enough that a pasted
// manual does not become a thousand-item inspection on a phone.
const (
	maxCheckpoints     = 100
	maxCheckpointRunes = 200
	maxTemplateRunes   = 100
)

// ChecklistTemplate is a template as the shop edits it.
//
// The labels are the shop's own words and go onto the customer's page as
// written; they are never translated.
type ChecklistTemplate struct {
	ID          string
	Name        string
	Active      bool
	Checkpoints []string
	// What it may be used for (#147): a workshop job, or a protocol of car
	// sales or rental. Each names its module.
	Subjects []string
	// How many inspections have been made from it. Editing does not change
	// those -- each copied its labels -- but the page says so.
	Used int
}

// subjectLabels are the words for what a checklist is for, as catalogue keys.
var subjectLabels = map[string]string{
	SubjectWorkOrder: "Workshop jobs",
	SubjectPurchase:  "Buying a car in",
	SubjectDelivery:  "Handing a car over",
	SubjectCheckOut:  "Handing a rental out",
	SubjectCheckIn:   "Taking a rental back",
}

// SubjectLabel is a subject in words.
func SubjectLabel(subject string) string {
	if l, ok := subjectLabels[subject]; ok {
		return l
	}
	return subject
}

// subjectModule is the module a subject belongs to.
func subjectModule(subject string) access.Module {
	switch subject {
	case SubjectPurchase, SubjectDelivery:
		return access.Dealer
	case SubjectCheckOut, SubjectCheckIn:
		return access.Rental
	}
	return access.Workshop
}

// SubjectsOffered is what a checklist may be made for in this shop: the
// subjects of the modules it runs, in the order a form offers them.
func SubjectsOffered(scope access.Scope) []string {
	var out []string
	for _, s := range []string{SubjectWorkOrder, SubjectPurchase, SubjectDelivery, SubjectCheckOut, SubjectCheckIn} {
		if scope.Offers(subjectModule(s)) {
			out = append(out, s)
		}
	}
	return out
}

// UsedFor reports whether the checklist may be used for subject.
func (t ChecklistTemplate) UsedFor(subject string) bool { return slices.Contains(t.Subjects, subject) }

// CheckpointText is the checkpoints as the edit form shows them.
func (t ChecklistTemplate) CheckpointText() string { return strings.Join(t.Checkpoints, "\n") }

// ParseCheckpoints reads one checkpoint per line, dropping blank lines.
//
// One textarea rather than a row of inputs with buttons to move them: on a
// phone, reordering is cutting a line and pasting it, which everybody already
// knows how to do, and it needs no script.
func ParseCheckpoints(text string) []string {
	var out []string
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// ChecklistTemplates lists every template, retired ones included, for the
// people who maintain them.
func ChecklistTemplates(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]ChecklistTemplate, error) {
	if !scope.Role.RunsTheShop() {
		return nil, access.ErrForbidden
	}
	return readChecklists(ctx, pool, scope, "")
}

// ChecklistTemplateByID reads one, for editing.
func ChecklistTemplateByID(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string) (ChecklistTemplate, error) {
	if !scope.Role.RunsTheShop() {
		return ChecklistTemplate{}, access.ErrForbidden
	}
	if !access.IsUUID(id) {
		return ChecklistTemplate{}, ErrNotFound
	}
	all, err := readChecklists(ctx, pool, scope, id)
	if err != nil {
		return ChecklistTemplate{}, err
	}
	if len(all) == 0 {
		return ChecklistTemplate{}, ErrNotFound
	}
	return all[0], nil
}

func readChecklists(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string) ([]ChecklistTemplate, error) {
	var out []ChecklistTemplate
	// Core (#147): the same checklists serve a job, a purchase, a check-out.
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT t.id, t.name, t.active, t.subjects,
			       coalesce((SELECT array_agg(i.label ORDER BY i.position)
			                 FROM inspection_template_items i WHERE i.template_id = t.id), '{}'),
			       (SELECT count(*) FROM inspections n WHERE n.template_id = t.id)
			FROM inspection_templates t
			WHERE $1 = '' OR t.id::text = $1
			ORDER BY NOT t.active, t.name`, id)
		if err != nil {
			return fmt.Errorf("list checklists: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var t ChecklistTemplate
			if err := rows.Scan(&t.ID, &t.Name, &t.Active, &t.Subjects, &t.Checkpoints, &t.Used); err != nil {
				return fmt.Errorf("scan checklist: %w", err)
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

// SaveChecklistTemplate creates a template when id is empty, and otherwise
// replaces its name and checkpoints.
//
// Replacing is safe because nothing points at a template's checkpoints: an
// inspection copies the labels when it starts, the way an invoice copies its
// lines, so an inspection made last month still shows what was checked then.
func SaveChecklistTemplate(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id, name string, checkpoints []string) (string, error) {
	if !scope.Role.RunsTheShop() {
		return "", access.ErrForbidden
	}
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return "", fmt.Errorf("%w: a checklist needs a name to be picked by", ErrInvalid)
	case utf8.RuneCountInString(name) > maxTemplateRunes:
		return "", fmt.Errorf("%w: the name is longer than %d characters", ErrInvalid, maxTemplateRunes)
	case len(checkpoints) == 0:
		return "", fmt.Errorf("%w: a checklist needs at least one thing to check", ErrInvalid)
	case len(checkpoints) > maxCheckpoints:
		return "", fmt.Errorf("%w: %d checkpoints is more than %d", ErrInvalid, len(checkpoints), maxCheckpoints)
	}
	seen := make(map[string]bool, len(checkpoints))
	for _, c := range checkpoints {
		if utf8.RuneCountInString(c) > maxCheckpointRunes {
			return "", fmt.Errorf("%w: %q is longer than %d characters", ErrInvalid, c, maxCheckpointRunes)
		}
		// The customer reads these side by side. Two lines saying the same
		// thing with different answers is a question nobody can answer.
		key := strings.ToLower(c)
		if seen[key] {
			return "", fmt.Errorf("%w: %q is on the list twice", ErrInvalid, c)
		}
		seen[key] = true
	}
	if id != "" && !access.IsUUID(id) {
		return "", ErrNotFound
	}

	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if id == "" {
			// A new checklist is for the workshop's jobs where there is a
			// workshop, as every checklist was; otherwise for what the
			// shop's modules do. Changed with SetTemplateSubjects.
			subjects := []string{SubjectWorkOrder}
			if offered := SubjectsOffered(scope); len(offered) > 0 && offered[0] != SubjectWorkOrder {
				subjects = offered
			}
			if err := tx.QueryRow(ctx,
				`INSERT INTO inspection_templates (shop_id, name, subjects) VALUES ($1, $2, $3) RETURNING id`,
				scope.ShopID, name, subjects).Scan(&id); err != nil {
				return fmt.Errorf("create checklist: %w", err)
			}
		} else {
			// Locked, so two people saving at once end with one list rather
			// than the checkpoints of both interleaved.
			tag, err := tx.Exec(ctx, `UPDATE inspection_templates SET name = $1 WHERE id = $2`, name, id)
			if err != nil {
				return fmt.Errorf("rename checklist: %w", err)
			}
			if tag.RowsAffected() == 0 {
				return ErrNotFound
			}
			if _, err := tx.Exec(ctx, `DELETE FROM inspection_template_items WHERE template_id = $1`, id); err != nil {
				return fmt.Errorf("clear checkpoints: %w", err)
			}
		}
		for i, label := range checkpoints {
			if _, err := tx.Exec(ctx, `
				INSERT INTO inspection_template_items (shop_id, template_id, position, label, item_key)
				VALUES ($1, $2, $3, $4, $5)`, scope.ShopID, id, i+1, label, strings.ToLower(label)); err != nil {
				return fmt.Errorf("add checkpoint: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// SetChecklistActive retires a template or brings one back.
//
// Retired rather than deleted: inspections remember which template they came
// from, and a retired one stops being offered without that history going.
func SetChecklistActive(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string, active bool) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if !access.IsUUID(id) {
		return ErrNotFound
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE inspection_templates SET active = $1 WHERE id = $2`, active, id)
		if err != nil {
			return fmt.Errorf("set checklist active: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}
