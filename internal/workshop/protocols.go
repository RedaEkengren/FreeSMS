package workshop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A protocol is an inspection that is evidence for something other than a
// workshop job (#147): a car bought in, a rental handed over or taken back,
// a car handed to its buyer. The same templates, items and photographs as
// the workshop's checks; what differs is whose it is, who may read it, and
// that once finished it is frozen by the database -- a correction is a new
// protocol naming the one it supersedes.
//
// A protocol carries the vehicle and what was found, and no party: a
// technician repairing the damage a check-in found reads the check-in, and
// is never handed the renter.

// Inspection subjects: what an inspection is evidence for.
const (
	SubjectWorkOrder = "work_order"
	SubjectPurchase  = "purchase"
	SubjectCheckOut  = "check_out"
	SubjectCheckIn   = "check_in"
	SubjectDelivery  = "delivery"
)

// subjectColumns is the reference column for each subject that is not a job.
var subjectColumns = map[string]string{
	SubjectPurchase: "purchase_id",
	SubjectCheckOut: "check_out_id",
	SubjectCheckIn:  "check_in_id",
	SubjectDelivery: "delivery_id",
}

// ErrProtocolFinished is a change to a protocol that has been completed or
// abandoned.
var ErrProtocolFinished = fmt.Errorf("%w: the protocol is finished; start a new one that supersedes it", ErrInvalid)

// ProtocolSubject is what a protocol is of: the kind, the purchase, rental
// or sale it is evidence for, and the vehicle.
type ProtocolSubject struct {
	Kind      string
	RefID     string
	VehicleID string
}

// Protocol is one protocol as it is read: the vehicle and what was found.
// No party is on it, by construction.
type Protocol struct {
	ID           string
	Subject      string
	VehicleID    string
	Registration string
	TemplateName string
	PerformedBy  string
	StartedAt    time.Time
	CompletedAt  *time.Time
	AbandonedAt  *time.Time
	SupersedesID string
	Items        []ProtocolItem
	// Photographs removed after the fact as personal data, oldest first.
	// The protocol is evidence; that it had them is part of it.
	Removals []PhotoRemoval
}

// PhotoRemoval is the record a protocol keeps of a photograph removed from
// it: from which checkpoint, when, by whom and why.
type PhotoRemoval struct {
	ItemID    string
	RemovedAt time.Time
	RemovedBy string
	Reason    string
}

// ProtocolItem is one checkpoint: its key, so a check-out and a check-in
// can be put side by side, the shop's own words, and what was found.
type ProtocolItem struct {
	ID     string
	Key    string
	Label  string
	Status string
	Note   string
	Photos []string
}

// subjectAllows decides whether roles may record (writing) or read a
// protocol of this kind, in this shop. Recording is for the role whose work
// it is: the buyer appraises a purchase, the seller hands a car over, the
// rental desk checks a rental out and in -- each in a shop that runs the
// module. Reading is theirs, and the workshop's people who follow jobs,
// while the workshop runs: the car they repair is the one the protocol is
// of.
func subjectAllows(roles access.Roles, scope access.Scope, kind string, writing bool) error {
	var module access.Module
	var may bool
	switch kind {
	case SubjectPurchase:
		module, may = access.Dealer, roles.BuysVehicles()
	case SubjectDelivery:
		module, may = access.Dealer, roles.SellsVehicles()
	case SubjectCheckOut, SubjectCheckIn:
		module, may = access.Rental, roles.HandsOverRentals()
	default:
		return fmt.Errorf("%w: %q is not a protocol's subject", ErrInvalid, kind)
	}
	if err := scope.Require(module); err != nil {
		return err
	}
	if !may && !writing && roles.FollowsJobs() && scope.Uses(access.Workshop) {
		may = true
	}
	if !may {
		return access.ErrForbidden
	}
	return nil
}

// StartProtocol copies a template onto a new protocol of a subject.
//
// The template must be one declared for the subject. key is the request's
// own key: a check-out done offline and replayed finds the protocol it
// started rather than starting a second. supersedes names a finished
// protocol of the same subject and vehicle that this one corrects.
func StartProtocol(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, subj ProtocolSubject, templateID, supersedes, key string) (string, error) {
	if err := subjectAllows(scope.Role, scope, subj.Kind, true); err != nil {
		return "", err
	}
	if !looksLikeUUID(subj.RefID) || !looksLikeUUID(subj.VehicleID) || !looksLikeUUID(templateID) ||
		(supersedes != "" && !looksLikeUUID(supersedes)) {
		return "", ErrNotFound
	}
	key = strings.TrimSpace(key)
	if len(key) > 100 {
		return "", fmt.Errorf("%w: the request key is too long", ErrInvalid)
	}
	var id string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if key != "" {
			var found bool
			var err error
			if id, found, err = replayedProtocol(ctx, tx, key, subj); found || err != nil {
				return err
			}
		}
		var name string
		err := tx.QueryRow(ctx, `SELECT name FROM inspection_templates WHERE id = $1 AND active AND $2 = ANY(subjects)`,
			templateID, subj.Kind).Scan(&name)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: that checklist is not one for this", ErrInvalid)
		}
		if err != nil {
			return fmt.Errorf("read template: %w", err)
		}
		if supersedes != "" {
			var ok bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM inspections WHERE id = $1 AND subject = $2 AND vehicle_now(vehicle_id) = vehicle_now($3)
				               AND (completed_at IS NOT NULL OR abandoned_at IS NOT NULL))`,
				supersedes, subj.Kind, subj.VehicleID).Scan(&ok); err != nil {
				return fmt.Errorf("read what it supersedes: %w", err)
			}
			if !ok {
				return fmt.Errorf("%w: a protocol supersedes a finished one of the same thing and the same vehicle", ErrInvalid)
			}
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO inspections (shop_id, subject, `+subjectColumns[subj.Kind]+`, vehicle_id, template_id, template_name,
			                         performed_by, supersedes_id, request_key)
			SELECT $1, $2, $3, v.id, $4, $5, $6, NULLIF($7, '')::uuid, NULLIF($8, '')
			FROM vehicles v WHERE v.id = $9
			ON CONFLICT (shop_id, request_key) WHERE request_key IS NOT NULL DO NOTHING
			RETURNING id`,
			scope.ShopID, subj.Kind, subj.RefID, templateID, name, scope.UserID, supersedes, key, subj.VehicleID).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			// Either there is no such vehicle, or the same request, replayed
			// at the same moment, got there first.
			var found bool
			if key != "" {
				if id, found, err = replayedProtocol(ctx, tx, key, subj); err != nil {
					return err
				}
			}
			if !found {
				return ErrNotFound
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("start protocol: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO inspection_items (shop_id, inspection_id, position, label, item_key)
			SELECT $1, $2, ti.position, ti.label, ti.item_key
			FROM inspection_template_items ti WHERE ti.template_id = $3 ORDER BY ti.position`,
			scope.ShopID, id, templateID); err != nil {
			return fmt.Errorf("copy template items: %w", err)
		}
		return nil
	})
	return id, err
}

// errKeyReused is a request key already used to start a different protocol.
var errKeyReused = fmt.Errorf("%w: that request was already used to start a protocol of something else", ErrInvalid)

// replayedProtocol finds the protocol a request key already started. A
// replay is the same request again, so it must be of the same subject, the
// same purchase, rental or sale, and the same vehicle; a key that started
// something else is refused rather than answered with another car's
// protocol, which findings would then be recorded against.
func replayedProtocol(ctx context.Context, tx pgx.Tx, key string, subj ProtocolSubject) (id string, found bool, err error) {
	var subject, vehicle, ref string
	err = tx.QueryRow(ctx, `
		SELECT id::text, subject, vehicle_id::text, coalesce(`+subjectColumns[subj.Kind]+`::text, '')
		FROM inspections WHERE request_key = $1`, key).Scan(&id, &subject, &vehicle, &ref)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("read the request: %w", err)
	// The database writes an identifier in lower case; one sent in capitals
	// is the same identifier.
	case subject != subj.Kind || !strings.EqualFold(vehicle, subj.VehicleID) || !strings.EqualFold(ref, subj.RefID):
		return "", false, errKeyReused
	}
	return id, true, nil
}

// protocolOf reads the subject of a protocol, or of the protocol an item is
// on; ErrNotFound for a job's inspection, which the workshop's operations
// handle.
func protocolOf(ctx context.Context, tx pgx.Tx, inspectionID, itemID string) (id, subject string, err error) {
	q := `SELECT i.id::text, i.subject FROM inspections i WHERE i.id = $1 AND i.subject <> 'work_order'`
	arg := inspectionID
	if itemID != "" {
		q = `SELECT i.id::text, i.subject FROM inspection_items it JOIN inspections i ON i.id = it.inspection_id
		     WHERE it.id = $1 AND i.subject <> 'work_order'`
		arg = itemID
	}
	err = tx.QueryRow(ctx, q, arg).Scan(&id, &subject)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return id, subject, err
}

// finished turns the database's refusal to change a finished protocol into
// the sentence a person can act on.
func finished(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23001" {
		return ErrProtocolFinished
	}
	return err
}

// RecordProtocolItem records what was found at one checkpoint. An empty
// status leaves the one recorded, as SetItem does.
func RecordProtocolItem(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, itemID, status, note string) error {
	if !looksLikeUUID(itemID) {
		return ErrNotFound
	}
	switch status {
	case "pass", "attention", "fail", "":
	default:
		return fmt.Errorf("%w: %q is not pass, attention or fail", ErrInvalid, status)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, subject, err := protocolOf(ctx, tx, "", itemID)
		if err != nil {
			return err
		}
		if err := subjectAllows(scope.Role, scope, subject, true); err != nil {
			return err
		}
		var s any
		if status != "" {
			s = status
		}
		_, err = tx.Exec(ctx, `UPDATE inspection_items SET status = coalesce($1, status), note = nullif($2, '') WHERE id = $3`,
			s, strings.TrimSpace(note), itemID)
		return finished(err)
	})
}

// AttachProtocolPhoto records a stored photograph against a checkpoint.
func AttachProtocolPhoto(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, itemID, key, contentType string, size int64, originalName string) error {
	if !looksLikeUUID(itemID) {
		return ErrNotFound
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, subject, err := protocolOf(ctx, tx, "", itemID)
		if err != nil {
			return err
		}
		if err := subjectAllows(scope.Role, scope, subject, true); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO attachments (shop_id, inspection_item_id, storage_key, content_type, byte_size, original_name, uploaded_by)
			VALUES ($1, $2, $3, $4, $5, nullif($6, ''), $7)`,
			scope.ShopID, itemID, key, contentType, size, originalName, scope.UserID)
		return finished(err)
	})
}

// CompleteProtocol finishes a protocol, which freezes it.
func CompleteProtocol(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string) error {
	return endProtocol(ctx, pool, scope, id, `completed_at = now()`, "")
}

// AbandonProtocol records that a protocol was started and left, and why.
// It stays, frozen, rather than disappearing: a check-out begun and not
// finished is something somebody may ask about.
func AbandonProtocol(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" || len([]rune(reason)) > 300 {
		return fmt.Errorf("%w: say why it was left, in 300 characters or fewer", ErrInvalid)
	}
	return endProtocol(ctx, pool, scope, id, `abandoned_at = now(), abandoned_reason = $2`, reason)
}

func endProtocol(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id, set, reason string) error {
	if !looksLikeUUID(id) {
		return ErrNotFound
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, subject, err := protocolOf(ctx, tx, id, "")
		if err != nil {
			return err
		}
		if err := subjectAllows(scope.Role, scope, subject, true); err != nil {
			return err
		}
		args := []any{id}
		if reason != "" {
			args = append(args, reason)
		}
		_, err = tx.Exec(ctx, `UPDATE inspections SET `+set+` WHERE id = $1`, args...)
		return finished(err)
	})
}

// ProtocolByID reads one protocol, for whoever may read its subject.
func ProtocolByID(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string) (Protocol, error) {
	if !looksLikeUUID(id) {
		return Protocol{}, ErrNotFound
	}
	var p Protocol
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, subject, err := protocolOf(ctx, tx, id, "")
		if err != nil {
			return err
		}
		if err := subjectAllows(scope.Role, scope, subject, false); err != nil {
			return err
		}
		ps, err := readProtocols(ctx, tx, `i.id = $1`, id)
		if err != nil {
			return err
		}
		p = ps[0]
		return nil
	})
	return p, err
}

// ProtocolsForVehicle lists the protocols of a vehicle the caller may read,
// oldest first: the check-out and the check-in after it, side by side.
func ProtocolsForVehicle(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, vehicleID string) ([]Protocol, error) {
	if !looksLikeUUID(vehicleID) {
		return nil, ErrNotFound
	}
	var out []Protocol
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		// A finished protocol keeps the vehicle it was made of, which a
		// merge does not rewrite; it is read through the merge instead.
		all, err := readProtocols(ctx, tx, `i.vehicle_id = ANY((SELECT vehicle_ids_read_as($1))::uuid[]) AND i.subject <> 'work_order'`, vehicleID)
		if err != nil {
			return err
		}
		for _, p := range all {
			switch err := subjectAllows(scope.Role, scope, p.Subject, false); {
			case err == nil:
				out = append(out, p)
			case errors.Is(err, access.ErrForbidden), errors.Is(err, access.ErrModuleOff):
				// Not this caller's to read; left out, not refused.
			default:
				return err
			}
		}
		return nil
	})
	return out, err
}

func readProtocols(ctx context.Context, tx pgx.Tx, where string, arg any) ([]Protocol, error) {
	rows, err := tx.Query(ctx, `
		SELECT i.id::text, i.subject, i.vehicle_id::text, coalesce(r.registration, ''), i.template_name,
		       coalesce(p.display_name, ''), i.started_at, i.completed_at, i.abandoned_at,
		       coalesce(i.supersedes_id::text, '')
		FROM inspections i
		CROSS JOIN LATERAL (SELECT vehicle_now(i.vehicle_id) AS now) vn
		LEFT JOIN vehicle_registrations r ON r.vehicle_id = vn.now AND r.valid_to IS NULL
		LEFT JOIN users u ON u.id = i.performed_by
		LEFT JOIN people p ON p.id = u.person_id
		WHERE `+where+` ORDER BY i.started_at, i.id`, arg)
	if err != nil {
		return nil, fmt.Errorf("read protocols: %w", err)
	}
	var out []Protocol
	for rows.Next() {
		var p Protocol
		if err := rows.Scan(&p.ID, &p.Subject, &p.VehicleID, &p.Registration, &p.TemplateName, &p.PerformedBy,
			&p.StartedAt, &p.CompletedAt, &p.AbandonedAt, &p.SupersedesID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan protocol: %w", err)
		}
		out = append(out, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		items, err := tx.Query(ctx, `
			SELECT it.id::text, coalesce(it.item_key, ''), it.label, coalesce(it.status, ''), coalesce(it.note, ''),
			       coalesce((SELECT array_agg(a.storage_key ORDER BY a.created_at) FROM attachments a
			                 WHERE a.inspection_item_id = it.id), '{}')
			FROM inspection_items it WHERE it.inspection_id = $1 ORDER BY it.position`, out[i].ID)
		if err != nil {
			return nil, fmt.Errorf("read items: %w", err)
		}
		for items.Next() {
			var it ProtocolItem
			if err := items.Scan(&it.ID, &it.Key, &it.Label, &it.Status, &it.Note, &it.Photos); err != nil {
				items.Close()
				return nil, fmt.Errorf("scan item: %w", err)
			}
			out[i].Items = append(out[i].Items, it)
		}
		items.Close()
		if err := items.Err(); err != nil {
			return nil, err
		}
		removals, err := tx.Query(ctx, `
			SELECT r.inspection_item_id::text, r.removed_at, coalesce(p.display_name, ''), r.reason
			FROM protocol_photo_removals r
			LEFT JOIN users u ON u.id = r.removed_by
			LEFT JOIN people p ON p.id = u.person_id
			WHERE r.inspection_id = $1 ORDER BY r.removed_at, r.id`, out[i].ID)
		if err != nil {
			return nil, fmt.Errorf("read removals: %w", err)
		}
		for removals.Next() {
			var r PhotoRemoval
			if err := removals.Scan(&r.ItemID, &r.RemovedAt, &r.RemovedBy, &r.Reason); err != nil {
				removals.Close()
				return nil, fmt.Errorf("scan removal: %w", err)
			}
			out[i].Removals = append(out[i].Removals, r)
		}
		removals.Close()
		if err := removals.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// SetTemplateSubjects says what a checklist may be used for: the workshop's
// jobs, and the protocols of the modules the shop runs. Whoever runs the
// shop decides, as they keep the checklists.
func SetTemplateSubjects(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, templateID string, subjects []string) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(templateID) {
		return ErrNotFound
	}
	if len(subjects) == 0 {
		return fmt.Errorf("%w: a checklist is for at least one thing", ErrInvalid)
	}
	for _, s := range subjects {
		if _, known := subjectLabels[s]; !known {
			return fmt.Errorf("%w: %q is not a protocol's subject", ErrInvalid, s)
		}
		if err := scope.Require(subjectModule(s)); err != nil {
			return err
		}
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		// The shop's final check is started on every job from this
		// checklist; taking jobs off it would leave no job able to be
		// signed off. Choose another final check first.
		if !slices.Contains(subjects, SubjectWorkOrder) {
			var final bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM shops WHERE id = $1 AND final_check_template_id = $2)`,
				scope.ShopID, templateID).Scan(&final); err != nil {
				return fmt.Errorf("read the final check: %w", err)
			}
			if final {
				return errFinalCheckForJobs
			}
		}
		tag, err := tx.Exec(ctx, `UPDATE inspection_templates SET subjects = $2 WHERE id = $1`, templateID, subjects)
		if err != nil {
			return fmt.Errorf("set subjects: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}
