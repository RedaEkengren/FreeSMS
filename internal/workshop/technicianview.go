package workshop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
)

// What a technician sees (#225). A technician is shown the work, the
// vehicle and its registration, the parts and the time, and nothing else
// unless whoever runs the shop opens a field: for every technician, in the
// shop's profile, or for one person, as an exception that may end by
// itself. Each change is a row with who, when and why (0090), read back on
// the shop's and the person's audit trail (#151). What is opened is read
// with the session (access.Scope.Opened) and applied where each field is
// read: a field not opened is not fetched.

// TechnicianFieldLabels are the fields in words, as catalogue keys.
var TechnicianFieldLabels = map[string]string{
	access.FieldCostCeiling:   "Cost the customer agreed to, at most",
	access.FieldLinePrices:    "Price of each line, and the total",
	access.FieldHourlyRate:    "The shop's hourly rate",
	access.FieldCustomerName:  "Customer's name, on their own jobs",
	access.FieldCustomerPhone: "Customer's telephone number, on their own jobs",
	access.FieldInvoiceStatus: "Whether the job is invoiced and paid",
}

// TechnicianFieldSetting is one field of the shop's profile.
type TechnicianFieldSetting struct {
	Field  string
	Shown  bool
	Reason string
	By     string
	At     *time.Time
}

// Label is the field in words, as a catalogue key.
func (s TechnicianFieldSetting) Label() string { return TechnicianFieldLabels[s.Field] }

// PersonalData reports whether the field is a customer's, which a
// technician may use only for the job.
func (s TechnicianFieldSetting) PersonalData() bool {
	return s.Field == access.FieldCustomerName || s.Field == access.FieldCustomerPhone
}

// TechnicianException is one field opened for one person.
type TechnicianException struct {
	UserID string
	Person string
	Field  string
	Reason string
	// The reason was taken away by an erasure (0092): the exception
	// stands, why it was given can name somebody who asked to be
	// forgotten.
	ReasonErased bool
	By           string
	At           time.Time
	Until        *time.Time
}

// Label is the field in words, as a catalogue key.
func (e TechnicianException) Label() string { return TechnicianFieldLabels[e.Field] }

// TechnicianViewSettings is the settings page: the profile, the exceptions
// in force, and the technicians an exception can be given to.
type TechnicianViewSettings struct {
	Profile     []TechnicianFieldSetting
	Exceptions  []TechnicianException
	Technicians []Option
}

var (
	errTechnicianField  = fmt.Errorf("%w: that is not something a technician can be shown", ErrInvalid)
	errTechnicianReason = fmt.Errorf("%w: say why, in 300 characters or fewer", ErrInvalid)
	errTechnicianUntil  = fmt.Errorf("%w: an exception ends after today, or not at all", ErrInvalid)
	errNotATechnician   = fmt.Errorf("%w: an exception is for one of the shop's technicians", ErrInvalid)
)

func checkTechnicianField(field string) error {
	if !slices.Contains(access.TechnicianFields, field) {
		return errTechnicianField
	}
	return nil
}

// TechnicianView reads the settings page. Whoever runs the shop.
func TechnicianView(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) (TechnicianViewSettings, error) {
	if !scope.Role.RunsTheShop() {
		return TechnicianViewSettings{}, access.ErrForbidden
	}
	var out TechnicianViewSettings
	err := inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		latest := map[string]TechnicianFieldSetting{}
		rows, err := tx.Query(ctx, `
			SELECT DISTINCT ON (c.field) c.field, c.shown, c.reason, coalesce(p.display_name, ''), c.changed_at
			FROM technician_view_changes c
			LEFT JOIN users u ON u.id = c.changed_by LEFT JOIN people p ON p.id = u.person_id
			ORDER BY c.field, c.seq DESC`)
		if err != nil {
			return fmt.Errorf("read the profile: %w", err)
		}
		for rows.Next() {
			var s TechnicianFieldSetting
			var at time.Time
			if err := rows.Scan(&s.Field, &s.Shown, &s.Reason, &s.By, &at); err != nil {
				rows.Close()
				return err
			}
			s.At = &at
			latest[s.Field] = s
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, f := range access.TechnicianFields {
			s, ok := latest[f]
			if !ok {
				s = TechnicianFieldSetting{Field: f}
			}
			out.Profile = append(out.Profile, s)
		}

		rows, err = tx.Query(ctx, `
			SELECT e.user_id::text, coalesce(tp.display_name, ''), e.field, coalesce(e.reason, ''), e.reason_erased_at IS NOT NULL,
			       coalesce(bp.display_name, ''), e.at, e.until
			FROM (SELECT DISTINCT ON (user_id, field) * FROM technician_view_exceptions ORDER BY user_id, field, seq DESC) e
			JOIN users tu ON tu.id = e.user_id LEFT JOIN people tp ON tp.id = tu.person_id
			LEFT JOIN users bu ON bu.id = e.by_user LEFT JOIN people bp ON bp.id = bu.person_id
			WHERE e.granted AND (e.until IS NULL OR e.until > clock_timestamp())
			ORDER BY tp.display_name, e.field`)
		if err != nil {
			return fmt.Errorf("read the exceptions: %w", err)
		}
		for rows.Next() {
			var e TechnicianException
			if err := rows.Scan(&e.UserID, &e.Person, &e.Field, &e.Reason, &e.ReasonErased, &e.By, &e.At, &e.Until); err != nil {
				rows.Close()
				return err
			}
			out.Exceptions = append(out.Exceptions, e)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		rows, err = tx.Query(ctx, `
			SELECT u.id::text, coalesce(p.display_name, '') FROM users u JOIN people p ON p.id = u.person_id
			WHERE u.active AND (u.role = 'technician' OR EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = u.id AND r.role = 'technician'))
			ORDER BY p.display_name`)
		if err != nil {
			return fmt.Errorf("read the technicians: %w", err)
		}
		out.Technicians, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Option, error) {
			var o Option
			return o, r.Scan(&o.Value, &o.Label)
		})
		return err
	})
	return out, err
}

// SetTechnicianField shows or hides a field for every technician, with
// why. Whoever runs the shop.
func SetTechnicianField(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, field string, shown bool, reason string) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if err := checkTechnicianField(field); err != nil {
		return err
	}
	reason, err := checkReason(reason, errTechnicianReason)
	if err != nil {
		return err
	}
	return inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO technician_view_changes (shop_id, field, shown, reason, changed_by) VALUES ($1, $2, $3, $4, $5)`,
			scope.ShopID, field, shown, reason, scope.UserID)
		if err != nil {
			return fmt.Errorf("change the profile: %w", err)
		}
		return nil
	})
}

// GrantTechnicianField opens a field for one technician, with why and, if
// given, when it stops applying by itself. Whoever runs the shop.
func GrantTechnicianField(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, userID, field, reason string, until *time.Time) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if err := checkTechnicianField(field); err != nil {
		return err
	}
	reason, err := checkReason(reason, errTechnicianReason)
	if err != nil {
		return err
	}
	if until != nil && !until.After(time.Now()) {
		return errTechnicianUntil
	}
	if !looksLikeUUID(userID) {
		return errNotATechnician
	}
	return inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO technician_view_exceptions (shop_id, user_id, field, granted, until, reason, by_user)
			SELECT $1, u.id, $3, true, $4, $5, $6 FROM users u
			WHERE u.id = $2 AND (u.role = 'technician' OR EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = u.id AND r.role = 'technician'))`,
			scope.ShopID, userID, field, until, reason, scope.UserID)
		if err != nil {
			return fmt.Errorf("grant the exception: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return errNotATechnician
		}
		return nil
	})
}

// EndTechnicianGrant ends one person's exception before its end, with why.
// Whoever runs the shop. An exception not in force is not found.
func EndTechnicianGrant(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, userID, field, reason string) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if err := checkTechnicianField(field); err != nil {
		return err
	}
	reason, err := checkReason(reason, errTechnicianReason)
	if err != nil {
		return err
	}
	if !looksLikeUUID(userID) {
		return ErrNotFound
	}
	return inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO technician_view_exceptions (shop_id, user_id, field, granted, reason, by_user)
			SELECT $1, $2, $3, false, $4, $5
			WHERE EXISTS (SELECT 1 FROM technician_fields_open f WHERE f.user_id = $2 AND f.field = $3)`,
			scope.ShopID, userID, field, reason, scope.UserID)
		if err != nil {
			return fmt.Errorf("end the exception: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// TechnicianJob is what a job's page shows a technician of the fields
// opened for them, beyond the work: each empty when it is not opened, or,
// for the customer, when the job is not theirs.
type TechnicianJob struct {
	CustomerName  string
	CustomerPhone string
	// What the customer agreed the work may cost, at most, VAT included;
	// nil when not opened or not agreed.
	CeilingMinor *int64
	// "not invoiced", "invoiced" or "paid", as a catalogue key; "" when
	// not opened.
	InvoiceStatus string
	// The shop's hourly rate before VAT; nil when not opened.
	HourlyRateMinor *int64
}

// PersonalData reports whether the page shows the technician anything of
// the customer's, which they may use only for the job.
func (t TechnicianJob) PersonalData() bool { return t.CustomerName != "" || t.CustomerPhone != "" }

// TechnicianJobFor reads the fields opened for the caller on a job, in one
// statement, each only when it is opened: what is not opened is not
// fetched. The customer's name and number only on a job assigned to the
// caller, so a job taken away from them takes those with it at once.
// Whoever works in the workshop; a role shown every field anyway reads
// them where the counter does, and gets nothing here.
func TechnicianJobFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string) (TechnicianJob, error) {
	var out TechnicianJob
	if scope.Role.SeesCustomerPersonalData() || !scope.OpenedForTechnician() {
		return out, nil
	}
	if !looksLikeUUID(jobID) {
		return out, ErrNotFound
	}
	err := inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT CASE WHEN $3 AND w.assigned_to = $2 THEN coalesce(p.display_name, c.company_name, '') ELSE '' END,
			       CASE WHEN $4 AND w.assigned_to = $2 THEN coalesce(p.phone, '') ELSE '' END,
			       CASE WHEN $5 THEN (SELECT d.agreed_gross_minor FROM quote_decisions d
			                          WHERE d.work_order_id = w.id AND d.decision = 'approved'
			                          ORDER BY d.decided_at DESC LIMIT 1) END,
			       CASE WHEN $6 THEN
			         -- Invoiced is an invoice issued for the job and standing:
			         -- not taken back by a credit note. What it came to does
			         -- not say, as an invoice at nothing is still issued.
			         CASE WHEN NOT EXISTS (SELECT 1 FROM invoices i WHERE i.work_order_id = w.id AND i.credit_of_id IS NULL
			                                 AND NOT EXISTS (SELECT 1 FROM invoices cr WHERE cr.credit_of_id = i.id
			                                                 AND i.gross_minor + (SELECT sum(c2.gross_minor) FROM invoices c2 WHERE c2.credit_of_id = i.id) = 0))
			              THEN 'not invoiced'
			              WHEN EXISTS (SELECT 1 FROM invoices i WHERE i.work_order_id = w.id AND i.credit_of_id IS NULL
			                             AND `+owedSQL("i")+` > 0)
			              THEN 'invoiced'
			              ELSE 'paid' END
			       ELSE '' END,
			       CASE WHEN $7 THEN (SELECT s.labour_rate_minor FROM shops s WHERE s.id = w.shop_id) END
			FROM work_orders w
			LEFT JOIN customers c ON c.id = w.customer_id
			LEFT JOIN people p ON p.id = c.person_id
			WHERE w.id = $1`, jobID, scope.UserID,
			scope.SeesOnJob(access.FieldCustomerName), scope.SeesOnJob(access.FieldCustomerPhone),
			scope.SeesOnJob(access.FieldCostCeiling), scope.SeesOnJob(access.FieldInvoiceStatus),
			scope.SeesOnJob(access.FieldHourlyRate)).
			Scan(&out.CustomerName, &out.CustomerPhone, &out.CeilingMinor, &out.InvoiceStatus, &out.HourlyRateMinor)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	out.CustomerName, out.CustomerPhone = strings.TrimSpace(out.CustomerName), strings.TrimSpace(out.CustomerPhone)
	return out, err
}
