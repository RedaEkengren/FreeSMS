package workshop

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/RedaEkengren/RedaSMS/internal/access"
	"github.com/RedaEkengren/RedaSMS/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// shareLifetime is how long a link the customer is sent stays usable.
//
// Short enough that a forwarded link stops working before it is forgotten
// about, long enough that somebody who looks at it after the weekend is not
// ringing the workshop to ask why it is broken.
const shareLifetime = 14 * 24 * time.Hour

// ErrShareNotUsable is returned for a link that is wrong, expired or revoked.
//
// One error for all three, because whoever is holding a link that does not
// work does not need to be told which kind of not working it is.
var ErrShareNotUsable = errors.New("workshop: that link is not usable")

// Inspection is one run of a checklist against a job.
type Inspection struct {
	ID           string
	WorkOrderID  string
	TemplateName string
	PerformedBy  string
	StartedAt    time.Time
	CompletedAt  *time.Time
	Items        []InspectionItem

	// Filled in on the customer-facing page.
	Registration string
	Make         string
	Model        string
	ShopName     string
}

// Completed reports whether the technician has finished.
func (i Inspection) Completed() bool { return i.CompletedAt != nil }

// Findings are the items worth showing a customer: everything that is not a
// plain pass.
//
// An inspection where nothing is wrong still has value -- it is the evidence
// that the check happened -- but it is not a list of things to approve.
func (i Inspection) Findings() []InspectionItem {
	var out []InspectionItem
	for _, item := range i.Items {
		if item.Status == "attention" || item.Status == "fail" {
			out = append(out, item)
		}
	}
	return out
}

// InspectionItem is one thing that was checked.
type InspectionItem struct {
	ID       string
	Position int
	Label    string
	Status   string
	Note     string
	Photos   []string

	// The customer's most recent answer, if they have given one.
	Decision   string
	DecidedAt  *time.Time
	Superseded int
}

// Decided reports whether the customer has answered this item.
func (i InspectionItem) Decided() bool { return i.Decision != "" }

// Templates lists the checklists a shop has.
func Templates(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]Template, error) {
	var out []Template
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, name FROM inspection_templates WHERE active ORDER BY name`)
		if err != nil {
			return fmt.Errorf("list templates: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var t Template
			if err := rows.Scan(&t.ID, &t.Name); err != nil {
				return fmt.Errorf("scan template: %w", err)
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

// Template is a named checklist.
type Template struct {
	ID   string
	Name string
}

// StartInspection copies a template onto a job.
//
// The labels are copied rather than joined, for the same reason an invoice
// copies its lines: editing the template next month must not change what a
// customer was shown last month.
func StartInspection(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID, templateID string) (string, error) {
	var id string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var name string
		err := tx.QueryRow(ctx,
			`SELECT name FROM inspection_templates WHERE id = $1 AND active`, templateID).Scan(&name)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read template: %w", err)
		}

		if err := tx.QueryRow(ctx, `
			INSERT INTO inspections (shop_id, work_order_id, template_id, template_name, performed_by)
			SELECT $1, w.id, $2, $3, $4 FROM work_orders w WHERE w.id = $5
			RETURNING id`,
			scope.ShopID, templateID, name, scope.UserID, jobID).Scan(&id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("start inspection: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO inspection_items (shop_id, inspection_id, position, label)
			SELECT $1, $2, ti.position, ti.label
			FROM inspection_template_items ti
			WHERE ti.template_id = $3
			ORDER BY ti.position`, scope.ShopID, id, templateID); err != nil {
			return fmt.Errorf("copy template items: %w", err)
		}
		return nil
	})
	return id, err
}

// SetItem records what the technician found.
func SetItem(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, itemID, status, note string) error {
	switch status {
	case "pass", "attention", "fail", "":
	default:
		return fmt.Errorf("%w: %q is not pass, attention or fail", ErrInvalid, status)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var s any
		if status != "" {
			s = status
		}
		tag, err := tx.Exec(ctx,
			`UPDATE inspection_items SET status = $1, note = nullif($2, '') WHERE id = $3`,
			s, strings.TrimSpace(note), itemID)
		if err != nil {
			return fmt.Errorf("set item: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// AttachPhoto records a stored photograph against an item.
func AttachPhoto(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, itemID, key, contentType string, size int64, originalName string) error {
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO attachments
			  (shop_id, inspection_item_id, storage_key, content_type, byte_size, original_name, uploaded_by)
			SELECT $1, i.id, $2, $3, $4, nullif($5, ''), $6
			FROM inspection_items i WHERE i.id = $7`,
			scope.ShopID, key, contentType, size, originalName, scope.UserID, itemID)
		if err != nil {
			return fmt.Errorf("attach photo: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// CompleteInspection marks the checklist as finished.
func CompleteInspection(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, inspectionID string) error {
	if !looksLikeUUID(inspectionID) {
		return ErrNotFound
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE inspections SET completed_at = now() WHERE id = $1 AND completed_at IS NULL`,
			inspectionID)
		if err != nil {
			return fmt.Errorf("complete inspection: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// InspectionsFor lists a job's inspections, newest last.
func InspectionsFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string) ([]Inspection, error) {
	var out []Inspection
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT i.id, i.work_order_id, i.template_name,
			       coalesce(p.display_name, ''), i.started_at, i.completed_at
			FROM inspections i
			LEFT JOIN users u  ON u.id = i.performed_by
			LEFT JOIN people p ON p.id = u.person_id
			WHERE i.work_order_id = $1
			ORDER BY i.started_at`, jobID)
		if err != nil {
			return fmt.Errorf("list inspections: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var i Inspection
			if err := rows.Scan(&i.ID, &i.WorkOrderID, &i.TemplateName,
				&i.PerformedBy, &i.StartedAt, &i.CompletedAt); err != nil {
				return fmt.Errorf("scan inspection: %w", err)
			}
			out = append(out, i)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	for idx := range out {
		items, err := itemsFor(ctx, pool, scope, out[idx].ID)
		if err != nil {
			return nil, err
		}
		out[idx].Items = items
	}
	return out, nil
}

func itemsFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, inspectionID string) ([]InspectionItem, error) {
	var out []InspectionItem
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		return loadItems(ctx, tx, inspectionID, &out)
	})
	return out, err
}

// loadItems reads a checklist with its photographs and the customer's latest
// answer per item.
func loadItems(ctx context.Context, tx pgx.Tx, inspectionID string, out *[]InspectionItem) error {
	rows, err := tx.Query(ctx, `
		SELECT it.id, it.position, it.label, coalesce(it.status, ''), coalesce(it.note, ''),
		       coalesce(d.decision, ''), d.decided_at,
		       (SELECT count(*) FROM inspection_decisions dd WHERE dd.item_id = it.id) AS decisions
		FROM inspection_items it
		-- The latest answer. Earlier ones stay in the table: a customer who
		-- approves and then declines has done two things, and the first is the
		-- record of what they were told when.
		LEFT JOIN LATERAL (
		    SELECT decision, decided_at FROM inspection_decisions
		    WHERE item_id = it.id ORDER BY decided_at DESC, id DESC LIMIT 1
		) d ON true
		WHERE it.inspection_id = $1
		ORDER BY it.position`, inspectionID)
	if err != nil {
		return fmt.Errorf("list items: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var it InspectionItem
		var decisions int
		if err := rows.Scan(&it.ID, &it.Position, &it.Label, &it.Status, &it.Note,
			&it.Decision, &it.DecidedAt, &decisions); err != nil {
			return fmt.Errorf("scan item: %w", err)
		}
		if decisions > 1 {
			it.Superseded = decisions - 1
		}
		*out = append(*out, it)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for idx := range *out {
		photos, err := tx.Query(ctx,
			`SELECT storage_key FROM attachments WHERE inspection_item_id = $1 ORDER BY created_at`,
			(*out)[idx].ID)
		if err != nil {
			return fmt.Errorf("list photos: %w", err)
		}
		for photos.Next() {
			var key string
			if err := photos.Scan(&key); err != nil {
				photos.Close()
				return fmt.Errorf("scan photo: %w", err)
			}
			(*out)[idx].Photos = append((*out)[idx].Photos, key)
		}
		photos.Close()
		if err := photos.Err(); err != nil {
			return err
		}
	}
	return nil
}

// CreateShare makes a link for the customer and returns the token.
//
// The token goes out once and is never stored; what is kept is its SHA-256,
// so a leaked backup does not hand over live links to customers' inspections.
func CreateShare(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, inspectionID string) (string, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return "", access.ErrForbidden
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))

	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO inspection_shares (shop_id, inspection_id, token_sha256, created_by, expires_at)
			SELECT $1, i.id, $2, $3, now() + $4::interval
			FROM inspections i WHERE i.id = $5`,
			scope.ShopID, sum[:], scope.UserID, fmt.Sprintf("%d seconds", int(shareLifetime.Seconds())), inspectionID)
		if err != nil {
			return fmt.Errorf("create share: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

// RevokeShare stops a link working.
//
// Necessary because the link will be forwarded. A customer sends it to a
// partner, into a group chat, to a workplace; when that turns out to have been
// a mistake there has to be something to do about it.
func RevokeShare(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, shareID string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	// An identifier that cannot name a row is "not there", not a server
	// fault. Letting it reach Postgres turns a missing form field into a 500
	// and a log line nobody reads.
	if !looksLikeUUID(shareID) {
		return ErrNotFound
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE inspection_shares SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, shareID)
		if err != nil {
			return fmt.Errorf("revoke share: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// looksLikeUUID screens identifiers that arrive from a form before they reach
// a query.
func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
				return false
			}
		}
	}
	return true
}

// Share is a link, as the front desk sees it.
type Share struct {
	ID        string
	CreatedAt time.Time
	ExpiresAt time.Time
	RevokedAt *time.Time
}

// Usable reports whether the link still works.
func (s Share) Usable() bool { return s.RevokedAt == nil && time.Now().Before(s.ExpiresAt) }

// SharesFor lists the links made for an inspection.
func SharesFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, inspectionID string) ([]Share, error) {
	var out []Share
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, created_at, expires_at, revoked_at FROM inspection_shares
			 WHERE inspection_id = $1 ORDER BY created_at DESC`, inspectionID)
		if err != nil {
			return fmt.Errorf("list shares: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var s Share
			if err := rows.Scan(&s.ID, &s.CreatedAt, &s.ExpiresAt, &s.RevokedAt); err != nil {
				return fmt.Errorf("scan share: %w", err)
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	return out, err
}

// SharedInspection reads what a link is allowed to show.
//
// There is no user here. The token is the whole of the authorisation, so it
// authorises exactly one inspection and nothing else -- no customer address,
// no other jobs, no prices the customer has not been quoted. Treat the page as
// public, because it will be forwarded.
func SharedInspection(ctx context.Context, pool *pgxpool.Pool, shopID, token string) (Inspection, string, error) {
	if token == "" {
		return Inspection{}, "", ErrShareNotUsable
	}
	sum := sha256.Sum256([]byte(token))

	var insp Inspection
	var shareID string
	err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		const lookup = `
			SELECT s.id, i.id, i.work_order_id, i.template_name, i.started_at, i.completed_at,
			       coalesce(p.display_name, ''),
			       coalesce(r.registration, ''), coalesce(v.make, ''), coalesce(v.model, ''),
			       sh.name
			FROM inspection_shares s
			JOIN inspections i ON i.id = s.inspection_id
			JOIN work_orders w ON w.id = i.work_order_id
			JOIN vehicles v    ON v.id = w.vehicle_id
			JOIN shops sh      ON sh.id = i.shop_id
			LEFT JOIN vehicle_registrations r
			       ON r.vehicle_id = v.id AND r.valid_to IS NULL
			LEFT JOIN users u  ON u.id = i.performed_by
			LEFT JOIN people p ON p.id = u.person_id
			WHERE s.token_sha256 = $1
			  AND s.revoked_at IS NULL
			  AND s.expires_at > now()`
		err := tx.QueryRow(ctx, lookup, sum[:]).Scan(
			&shareID, &insp.ID, &insp.WorkOrderID, &insp.TemplateName,
			&insp.StartedAt, &insp.CompletedAt, &insp.PerformedBy,
			&insp.Registration, &insp.Make, &insp.Model, &insp.ShopName)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrShareNotUsable
		}
		if err != nil {
			return fmt.Errorf("read shared inspection: %w", err)
		}
		return loadItems(ctx, tx, insp.ID, &insp.Items)
	})
	if err != nil {
		return Inspection{}, "", err
	}
	return insp, shareID, nil
}

// RecordDecision stores what the customer said about one item.
//
// Appended, never updated. Somebody who approves and then declines ten minutes
// later has done two things; which is current is a question about order, and
// losing the first would lose the record of what they were told when. It is
// also the answer to "who approved this" when a different person arrives to
// collect the car.
func RecordDecision(ctx context.Context, pool *pgxpool.Pool, shopID, token, itemID, decision string) error {
	switch decision {
	case "approved", "declined":
	default:
		return fmt.Errorf("%w: %q is not approved or declined", ErrInvalid, decision)
	}
	sum := sha256.Sum256([]byte(token))

	return database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		// The item must belong to the inspection the token names. Without this
		// join a link to one inspection would decide items on any other.
		tag, err := tx.Exec(ctx, `
			INSERT INTO inspection_decisions (shop_id, item_id, decision, share_id)
			SELECT s.shop_id, it.id, $1, s.id
			FROM inspection_shares s
			JOIN inspection_items it ON it.inspection_id = s.inspection_id
			WHERE s.token_sha256 = $2
			  AND s.revoked_at IS NULL
			  AND s.expires_at > now()
			  AND it.id = $3`,
			decision, sum[:], itemID)
		if err != nil {
			return fmt.Errorf("record decision: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrShareNotUsable
		}
		return nil
	})
}

// ApprovedFindings lists the items a customer has said yes to that have not
// been turned into lines yet, so the front desk can price them in one action.
func ApprovedFindings(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, inspectionID string) ([]InspectionItem, error) {
	items, err := itemsFor(ctx, pool, scope, inspectionID)
	if err != nil {
		return nil, err
	}
	var out []InspectionItem
	for _, it := range items {
		if it.Decision == "approved" {
			out = append(out, it)
		}
	}
	return out, nil
}

// InspectionsForID reads a single inspection with its items.
func InspectionsForID(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string) (Inspection, error) {
	var insp Inspection
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		const q = `
			SELECT i.id, i.work_order_id, i.template_name,
			       coalesce(p.display_name, ''), i.started_at, i.completed_at,
			       coalesce(r.registration, ''), coalesce(v.make, ''), coalesce(v.model, '')
			FROM inspections i
			JOIN work_orders w ON w.id = i.work_order_id
			JOIN vehicles v    ON v.id = w.vehicle_id
			LEFT JOIN vehicle_registrations r
			       ON r.vehicle_id = v.id AND r.valid_to IS NULL
			LEFT JOIN users u  ON u.id = i.performed_by
			LEFT JOIN people p ON p.id = u.person_id
			WHERE i.id = $1`
		err := tx.QueryRow(ctx, q, id).Scan(&insp.ID, &insp.WorkOrderID, &insp.TemplateName,
			&insp.PerformedBy, &insp.StartedAt, &insp.CompletedAt,
			&insp.Registration, &insp.Make, &insp.Model)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read inspection: %w", err)
		}
		return loadItems(ctx, tx, insp.ID, &insp.Items)
	})
	return insp, err
}

// PhotoInShop reports whether a stored photograph belongs to the caller's
// shop, which is what makes it safe to serve.
//
// Row level security does the work: a key from another shop simply is not
// found, so this cannot be talked into serving somebody else's photograph by
// guessing a key.
func PhotoInShop(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, key string) (bool, error) {
	var ok bool
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT exists(SELECT 1 FROM attachments WHERE storage_key = $1)`, key).Scan(&ok)
	})
	return ok, err
}

// PhotoInShare reports whether a photograph belongs to the inspection a link
// names.
//
// The token authorises one inspection, so it must not serve photographs from
// any other -- including other inspections of the same car.
func PhotoInShare(ctx context.Context, pool *pgxpool.Pool, shopID, token, key string) (bool, error) {
	if token == "" {
		return false, nil
	}
	sum := sha256.Sum256([]byte(token))
	var ok bool
	err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT exists(
			    SELECT 1
			    FROM inspection_shares s
			    JOIN inspection_items it ON it.inspection_id = s.inspection_id
			    JOIN attachments a       ON a.inspection_item_id = it.id
			    WHERE s.token_sha256 = $1
			      AND s.revoked_at IS NULL
			      AND s.expires_at > now()
			      AND a.storage_key = $2)`, sum[:], key).Scan(&ok)
	})
	return ok, err
}
