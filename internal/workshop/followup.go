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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// Chasing what is owed, and paying back what is owed back.
//
// The balance stays what the invoice, its credit notes and its payments say.
// Who has the invoice, when they will look at it again, what the customer
// promised and whether they dispute the charge sit beside it as rows: none of
// them changes a krona of the balance, and a dispute only stops reminders going
// out. Reminder fees, interest and collection agencies are not here: they are
// a decision of their own and nothing adds them by itself.
//
// Registering a refund records that the workshop paid money out, by the means
// it names. It does not move money; FreeSMS holds none.

// FollowUpEvent is something done or said about an invoice that is owed.
type FollowUpEvent struct {
	Kind          string
	How           string
	Note          string
	PromisedMinor int64
	PromisedBy    *time.Time
	OwedThen      int64
	By            string
	At            time.Time
}

// Promise is a promise to pay, and where it stands.
type Promise struct {
	AmountMinor int64
	By          time.Time
	// Paid since, enough to cover it.
	Kept bool
	// Its day has gone and it was not kept.
	Broken bool
}

// FollowUp is the follow-up of one invoice.
type FollowUp struct {
	InvoiceID   string
	Reference   string
	Customer    string
	OwnerID     string
	Owner       string
	Next        *time.Time
	Disputed    *time.Time
	DisputeNote string
	Events      []FollowUpEvent
	Promises    []Promise
	Outstanding int64
}

func (f FollowUp) onHold() bool { return f.Disputed != nil }

func originalTx(ctx context.Context, tx pgx.Tx, invoiceID string) (string, error) {
	var credit *string
	if err := tx.QueryRow(ctx, `SELECT credit_of_id::text FROM invoices WHERE id = $1`, invoiceID).Scan(&credit); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("read invoice: %w", err)
	}
	if credit != nil {
		return *credit, nil
	}
	return invoiceID, nil
}

func caseTx(ctx context.Context, tx pgx.Tx, scope access.Scope, invoiceID string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO receivable_cases (shop_id, invoice_id) VALUES ($1, $2) ON CONFLICT (invoice_id) DO NOTHING`, scope.ShopID, invoiceID)
	if err != nil {
		return fmt.Errorf("open the follow-up: %w", err)
	}
	return nil
}

func eventTx(ctx context.Context, tx pgx.Tx, scope access.Scope, invoiceID, kind, how, note string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO receivable_events (shop_id, invoice_id, kind, how, note, by_user)
		VALUES ($1, $2, $3, nullif($4, ''), nullif($5, ''), $6)`, scope.ShopID, invoiceID, kind, how, strings.TrimSpace(note), scope.UserID)
	if err != nil {
		return fmt.Errorf("record the follow-up: %w", err)
	}
	return nil
}

// FollowUpFor reads the follow-up of an invoice. Asked about a credit note it
// answers for the invoice it belongs to.
func FollowUpFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, invoiceID string) (FollowUp, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return FollowUp{}, access.ErrForbidden
	}
	if !looksLikeUUID(invoiceID) {
		return FollowUp{}, ErrNotFound
	}
	var f FollowUp
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		id, err := originalTx(ctx, tx, invoiceID)
		if err != nil {
			return err
		}
		b, err := balanceTx(ctx, tx, id, false)
		if err != nil {
			return err
		}
		f.InvoiceID, f.Reference, f.Customer, f.Outstanding = id, b.Reference, b.CustomerName, b.OutstandingMinor()
		if err := tx.QueryRow(ctx, `
			SELECT coalesce(c.owner_id::text, ''), coalesce(p.display_name, ''), c.next_follow_up::timestamptz, c.disputed_at, coalesce(c.dispute_note, '')
			FROM receivable_cases c LEFT JOIN users u ON u.id = c.owner_id LEFT JOIN people p ON p.id = u.person_id
			WHERE c.invoice_id = $1`, id).Scan(&f.OwnerID, &f.Owner, &f.Next, &f.Disputed, &f.DisputeNote); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read follow-up: %w", err)
		}
		rows, err := tx.Query(ctx, `
			SELECT e.kind, coalesce(e.how, ''), coalesce(e.note, ''), coalesce(e.promised_minor, 0), e.promised_by::timestamptz,
			       coalesce(e.owed_then_minor, 0), coalesce(p.display_name, ''), e.at
			FROM receivable_events e LEFT JOIN users u ON u.id = e.by_user LEFT JOIN people p ON p.id = u.person_id
			WHERE e.invoice_id = $1 ORDER BY e.at DESC, e.id`, id)
		if err != nil {
			return fmt.Errorf("read events: %w", err)
		}
		defer rows.Close()
		now := time.Now()
		for rows.Next() {
			var e FollowUpEvent
			if err := rows.Scan(&e.Kind, &e.How, &e.Note, &e.PromisedMinor, &e.PromisedBy, &e.OwedThen, &e.By, &e.At); err != nil {
				return fmt.Errorf("scan event: %w", err)
			}
			f.Events = append(f.Events, e)
			if e.Kind == "promise" && e.PromisedBy != nil {
				kept := f.Outstanding <= e.OwedThen-e.PromisedMinor
				f.Promises = append(f.Promises, Promise{AmountMinor: e.PromisedMinor, By: *e.PromisedBy, Kept: kept,
					Broken: !kept && e.PromisedBy.AddDate(0, 0, 1).Before(now)})
			}
		}
		return rows.Err()
	})
	return f, err
}

// FollowUpSet changes who has an invoice and when they look at it next.
type FollowUpSet struct {
	// A user, or empty to leave it as it is.
	OwnerID    string
	ClearOwner bool
	Next       *time.Time
	ClearNext  bool
	Note       string
}

// TakeFollowUp says who has an invoice and when it is to be looked at again.
func TakeFollowUp(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, invoiceID string, in FollowUpSet) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(invoiceID) {
		return ErrNotFound
	}
	in.Note = strings.TrimSpace(in.Note)
	if len([]rune(in.Note)) > 500 {
		return fmt.Errorf("%w: keep the note to 500 characters", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		id, err := originalTx(ctx, tx, invoiceID)
		if err != nil {
			return err
		}
		if in.OwnerID != "" {
			if !looksLikeUUID(in.OwnerID) {
				return ErrNotFound
			}
			var ok bool
			if err := tx.QueryRow(ctx, `SELECT exists(SELECT 1 FROM users WHERE id = $1 AND active AND role IN ('owner', 'admin', 'service_advisor'))`, in.OwnerID).Scan(&ok); err != nil {
				return fmt.Errorf("check owner: %w", err)
			}
			if !ok {
				return fmt.Errorf("%w: that person does not work with invoices", ErrInvalid)
			}
		}
		if err := caseTx(ctx, tx, scope, id); err != nil {
			return err
		}
		if in.OwnerID != "" || in.ClearOwner {
			owner := any(nil)
			if in.OwnerID != "" && !in.ClearOwner {
				owner = in.OwnerID
			}
			if _, err := tx.Exec(ctx, `UPDATE receivable_cases SET owner_id = $2::uuid, updated_at = clock_timestamp() WHERE invoice_id = $1`, id, owner); err != nil {
				return fmt.Errorf("set owner: %w", err)
			}
		}
		if in.Next != nil || in.ClearNext {
			next := any(nil)
			if in.Next != nil && !in.ClearNext {
				next = in.Next.Format("2006-01-02")
			}
			if _, err := tx.Exec(ctx, `UPDATE receivable_cases SET next_follow_up = $2::date, updated_at = clock_timestamp() WHERE invoice_id = $1`, id, next); err != nil {
				return fmt.Errorf("set date: %w", err)
			}
		}
		return eventTx(ctx, tx, scope, id, "taken", "", in.Note)
	})
}

// LogContact writes down that the customer was rung, written to or visited
// about an invoice, and what was said, with the next time to look at it.
func LogContact(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, invoiceID, how, note string, next *time.Time) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(invoiceID) {
		return ErrNotFound
	}
	how, note = strings.TrimSpace(how), strings.TrimSpace(note)
	if how == "" || len([]rune(how)) > 40 || note == "" || len([]rune(note)) > 500 {
		return fmt.Errorf("%w: say how, and what was said", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		id, err := originalTx(ctx, tx, invoiceID)
		if err != nil {
			return err
		}
		if err := caseTx(ctx, tx, scope, id); err != nil {
			return err
		}
		if next != nil {
			if _, err := tx.Exec(ctx, `UPDATE receivable_cases SET next_follow_up = $2::date, updated_at = clock_timestamp() WHERE invoice_id = $1`, id, next.Format("2006-01-02")); err != nil {
				return fmt.Errorf("set date: %w", err)
			}
		}
		return eventTx(ctx, tx, scope, id, "contact", how, note)
	})
}

// RecordPromise writes down that the customer will pay an amount by a day: a
// part-payment agreement. The balance is not touched; whether it was kept is
// read from what was paid afterwards.
func RecordPromise(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, invoiceID string, amountMinor int64, by time.Time, note string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(invoiceID) {
		return ErrNotFound
	}
	note = strings.TrimSpace(note)
	if amountMinor <= 0 || by.IsZero() || len([]rune(note)) > 500 {
		return fmt.Errorf("%w: an amount, a day, and a note of 500 characters or fewer", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		id, err := originalTx(ctx, tx, invoiceID)
		if err != nil {
			return err
		}
		b, err := balanceTx(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if b.OutstandingMinor() <= 0 {
			return fmt.Errorf("%w: nothing is owed on that invoice", ErrInvalid)
		}
		if amountMinor > b.OutstandingMinor() {
			return fmt.Errorf("%w: that is more than is owed", ErrInvalid)
		}
		if err := caseTx(ctx, tx, scope, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE receivable_cases SET next_follow_up = $2::date, updated_at = clock_timestamp() WHERE invoice_id = $1`, id, by.AddDate(0, 0, 1).Format("2006-01-02")); err != nil {
			return fmt.Errorf("set date: %w", err)
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO receivable_events (shop_id, invoice_id, kind, note, promised_minor, promised_by, owed_then_minor, by_user)
			VALUES ($1, $2, 'promise', nullif($3, ''), $4, $5::date, $6, $7)`,
			scope.ShopID, id, note, amountMinor, by.Format("2006-01-02"), b.OutstandingMinor(), scope.UserID)
		return err
	})
}

// Dispute puts an invoice on hold because the customer disputes the charge.
// The balance is what it was; no reminder goes while the hold stands.
func Dispute(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, invoiceID, note string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(invoiceID) {
		return ErrNotFound
	}
	note = strings.TrimSpace(note)
	if note == "" || len([]rune(note)) > 300 {
		return fmt.Errorf("%w: say what is disputed, in 300 characters or fewer", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		id, err := originalTx(ctx, tx, invoiceID)
		if err != nil {
			return err
		}
		if err := caseTx(ctx, tx, scope, id); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE receivable_cases SET disputed_at = clock_timestamp(), dispute_note = $2, updated_at = clock_timestamp()
			WHERE invoice_id = $1 AND disputed_at IS NULL`, id, note)
		if err != nil {
			return fmt.Errorf("dispute: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		return eventTx(ctx, tx, scope, id, "dispute", "", note)
	})
}

// ResolveDispute lifts the hold, with what was settled.
func ResolveDispute(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, invoiceID, note string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(invoiceID) {
		return ErrNotFound
	}
	note = strings.TrimSpace(note)
	if note == "" || len([]rune(note)) > 500 {
		return fmt.Errorf("%w: say how it was settled", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		id, err := originalTx(ctx, tx, invoiceID)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE receivable_cases SET disputed_at = NULL, dispute_note = NULL, updated_at = clock_timestamp()
			WHERE invoice_id = $1 AND disputed_at IS NOT NULL`, id)
		if err != nil {
			return fmt.Errorf("resolve: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		return eventTx(ctx, tx, scope, id, "dispute_resolved", "", note)
	})
}

// ReminderInput is a reminder to send.
type ReminderInput struct {
	Channel       string
	Address       string
	RecipientName string
	Confirmed     bool
	Subject       string
	// With {link} for the invoice and {owed} for what is owed at the moment
	// it goes.
	Body      string
	NotBefore time.Time
	LinkBase  string
}

// ReminderPreview is what a reminder would say about, as of now.
type ReminderPreview struct {
	Reference   string
	Customer    string
	Outstanding int64
	Due         time.Time
	DaysLate    int
	Disputed    bool
	// The customer is not known to have the invoice at all.
	NeverDelivered bool
	Reminders      int
}

// PrepareReminder reads what a reminder would be about: the balance now, how
// late it is, whether the invoice is known to have reached the customer, and
// whether it is on hold.
func PrepareReminder(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, invoiceID string, today time.Time) (ReminderPreview, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return ReminderPreview{}, access.ErrForbidden
	}
	if !looksLikeUUID(invoiceID) {
		return ReminderPreview{}, ErrNotFound
	}
	var p ReminderPreview
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		id, err := originalTx(ctx, tx, invoiceID)
		if err != nil {
			return err
		}
		b, err := balanceTx(ctx, tx, id, false)
		if err != nil {
			return err
		}
		p.Reference, p.Customer, p.Outstanding, p.Due = b.Reference, b.CustomerName, b.OutstandingMinor(), b.Due
		if today.After(b.Due) {
			p.DaysLate = int(today.Sub(b.Due).Hours() / 24)
		}
		if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT disputed_at IS NOT NULL FROM receivable_cases WHERE invoice_id = $1), false),
				(SELECT count(*) FROM receivable_events WHERE invoice_id = $1 AND kind = 'reminder')::int,
				NOT EXISTS (SELECT 1 FROM invoice_deliveries d LEFT JOIN invoice_links l ON l.id = d.link_id
				            WHERE d.invoice_id = $1 AND (d.kind = 'handed' OR l.first_opened_at IS NOT NULL
				                  OR (SELECT e.event FROM message_events e WHERE e.message_id = d.message_id ORDER BY e.at DESC, e.id DESC LIMIT 1) IN ('sent', 'delivered')))`,
			id).Scan(&p.Disputed, &p.Reminders, &p.NeverDelivered); err != nil {
			return fmt.Errorf("read reminder facts: %w", err)
		}
		return nil
	})
	return p, err
}

// QueueReminder queues a reminder about an invoice that is owed and not on
// hold, carrying a link to the invoice itself. It does not close the link the
// customer already has. The balance is read again when the message is about to
// go, so a reminder for something paid in the meantime is never sent.
func QueueReminder(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, invoiceID string, in ReminderInput) (string, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return "", access.ErrForbidden
	}
	if !looksLikeUUID(invoiceID) {
		return "", ErrNotFound
	}
	in.RecipientName, in.Address = strings.TrimSpace(in.RecipientName), strings.TrimSpace(in.Address)
	if in.RecipientName == "" || !in.Confirmed {
		return "", fmt.Errorf("%w: confirm who the reminder is for and where it goes", ErrInvalid)
	}
	if in.Channel != "sms" && in.Channel != "email" {
		return "", fmt.Errorf("%w: a text or an email", ErrInvalid)
	}
	if in.Address == "" || !strings.Contains(in.Body, "{link}") || !strings.Contains(in.Body, "{owed}") {
		return "", fmt.Errorf("%w: an address, and a message that carries the link and what is owed", ErrInvalid)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	var msgID string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		id, err := originalTx(ctx, tx, invoiceID)
		if err != nil {
			return err
		}
		b, err := balanceTx(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if b.OutstandingMinor() <= 0 {
			return ErrNothingOwed
		}
		var disputed bool
		if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT disputed_at IS NOT NULL FROM receivable_cases WHERE invoice_id = $1), false)`, id).Scan(&disputed); err != nil {
			return fmt.Errorf("read hold: %w", err)
		}
		if disputed {
			return fmt.Errorf("%w: the charge is disputed; lift the hold before sending a reminder", ErrInvalid)
		}
		var linkID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO invoice_links (shop_id, invoice_id, token_sha256, created_by, expires_at)
			VALUES ($1, $2, $3, $4, clock_timestamp() + make_interval(days => $5)) RETURNING id`,
			scope.ShopID, id, sum[:], scope.UserID, DocumentLinkDays).Scan(&linkID); err != nil {
			return fmt.Errorf("make link: %w", err)
		}
		_ = linkID
		not := in.NotBefore
		if not.IsZero() {
			not = time.Now()
		}
		msgID, err = queueMessageTx(ctx, tx, scope, NewMessage{
			JobID: b.WorkOrderID, About: "reminder", Channel: in.Channel, Recipient: in.Address, Subject: in.Subject,
			Body: strings.ReplaceAll(in.Body, "{link}", strings.TrimRight(in.LinkBase, "/")+"/d/"+token), NotBefore: not,
			ReminderFor: id,
		})
		if err != nil {
			return err
		}
		if err := caseTx(ctx, tx, scope, id); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO receivable_events (shop_id, invoice_id, kind, how, note, message_id, by_user)
			VALUES ($1, $2, 'reminder', $3, $4, $5, $6)`, scope.ShopID, id, in.Channel, "To "+in.RecipientName, msgID, scope.UserID)
		return err
	})
	return msgID, err
}

// StillOwed is asked as a reminder is about to go: what is owed on the
// invoice now, and whether it is on hold. A reminder is sent only for
// something that is owed and not disputed at that moment.
func StillOwed(ctx context.Context, pool *pgxpool.Pool, shopID, invoiceID string) (owedMinor int64, onHold bool, err error) {
	err = database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		b, err := balanceTx(ctx, tx, invoiceID, false)
		if err != nil {
			return err
		}
		owedMinor = b.OutstandingMinor()
		return tx.QueryRow(ctx, `SELECT coalesce((SELECT disputed_at IS NOT NULL FROM receivable_cases WHERE invoice_id = $1), false)`, b.InvoiceID).Scan(&onHold)
	})
	return owedMinor, onHold, err
}

// MessageCancelled records that a message was not sent, and why.
func MessageCancelled(ctx context.Context, pool *pgxpool.Pool, shopID, messageID, detail string) error {
	return database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO message_events (shop_id, message_id, event, detail) VALUES ($1, $2, 'cancelled', $3)`, shopID, messageID, detail)
		return err
	})
}

// RefundInput is money paid out to a customer.
type RefundInput struct {
	// Zero is all that is owed back.
	AmountMinor int64
	Method      string
	PaidOn      time.Time
	Reference   string
}

// RecordRefund writes down that the workshop paid money back to the customer,
// against an invoice that is owed back after a credit. It is an entry in the
// payment ledger like any other, signed the other way. It is not the reversal
// of a payment entered in error, which undoes an entry; this is money that
// really went out. FreeSMS does not move it: whoever paid it did, by the means
// named here.
func RecordRefund(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, invoiceID string, in RefundInput) (Payment, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return Payment{}, access.ErrForbidden
	}
	if !looksLikeUUID(invoiceID) {
		return Payment{}, ErrNotFound
	}
	if !knownMethod(in.Method) {
		return Payment{}, fmt.Errorf("%w: %q is not a way money is paid", ErrInvalid, in.Method)
	}
	if in.PaidOn.IsZero() || in.PaidOn.After(time.Now().Add(24*time.Hour)) {
		return Payment{}, fmt.Errorf("%w: when did it go out? Not in the future", ErrInvalid)
	}
	if in.AmountMinor < 0 {
		return Payment{}, fmt.Errorf("%w: a refund is an amount above nothing", ErrInvalid)
	}
	var out Payment
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		id, err := originalTx(ctx, tx, invoiceID)
		if err != nil {
			return err
		}
		b, err := balanceTx(ctx, tx, id, true)
		if err != nil {
			return err
		}
		back := -b.OutstandingMinor()
		if back <= 0 {
			return fmt.Errorf("%w: nothing is owed back on that invoice", ErrInvalid)
		}
		amount := in.AmountMinor
		if amount == 0 {
			amount = back
		}
		if amount > back {
			return fmt.Errorf("%w: that is more than is owed back", ErrInvalid)
		}
		out = Payment{AmountMinor: -amount, Method: in.Method, PaidOn: in.PaidOn, Reference: strings.TrimSpace(in.Reference)}
		if err := insertPayment(ctx, tx, scope, id, &out, ""); err != nil {
			return err
		}
		if err := caseTx(ctx, tx, scope, id); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO receivable_events (shop_id, invoice_id, kind, how, payment_id, by_user)
			VALUES ($1, $2, 'refund', $3, $4, $5)`, scope.ShopID, id, in.Method, out.ID, scope.UserID)
		return err
	})
	return out, err
}

// WorkItem is one line of the list of what to attend to.
type WorkItem struct {
	InvoiceID   string
	Reference   string
	Customer    string
	Outstanding int64
	Due         time.Time
	DaysLate    int
	OwnerID     string
	Owner       string
	Next        *time.Time
	Disputed    bool
	LastNote    string
	LastAt      *time.Time
	// "reached" when the customer is known to have the invoice, "tried" when
	// sending it failed, "none" when nothing was tried.
	Delivered string
	// Other invoices still open for the same customer.
	OtherOpen int
	Reminders int
	// Its day to be looked at has come.
	FollowToday bool
}

// Refund reports money owed back to the customer.
func (w WorkItem) Refund() bool { return w.Outstanding < 0 }

// Overdue reports an invoice owed past its day.
func (w WorkItem) Overdue() bool { return w.Outstanding > 0 && w.DaysLate > 0 }

// Worklist lists every invoice with something owed either way, with who has it
// and when it is next to be looked at, refunds first and then the latest due.
func Worklist(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, today time.Time) ([]WorkItem, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	var out []WorkItem
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		loc, err := shopLocationTx(ctx, tx)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			WITH open AS (
			  SELECT i.id, i.series || '-' || i.number AS ref, i.customer_name, i.work_order_id, i.issued_at, coalesce(i.seller_payment_terms_days, 0) AS terms,
			         i.gross_minor
			           + coalesce((SELECT sum(c.gross_minor) FROM invoices c WHERE c.credit_of_id = i.id), 0)
			           - coalesce((SELECT sum(p.amount_minor + p.rounding_minor) FROM invoice_payments p WHERE p.invoice_id = i.id), 0) AS owed
			  FROM invoices i WHERE i.credit_of_id IS NULL)
			SELECT o.id::text, o.ref, o.customer_name, o.owed, (o.issued_at AT TIME ZONE $1)::date + o.terms,
			       coalesce(c.owner_id::text, ''), coalesce(op.display_name, ''), c.next_follow_up, c.disputed_at IS NOT NULL,
			       coalesce((SELECT e.note FROM receivable_events e WHERE e.invoice_id = o.id AND e.note IS NOT NULL ORDER BY e.at DESC, e.id DESC LIMIT 1), ''),
			       (SELECT max(e.at) FROM receivable_events e WHERE e.invoice_id = o.id),
			       CASE
			         WHEN EXISTS (SELECT 1 FROM invoice_deliveries d LEFT JOIN invoice_links l ON l.id = d.link_id
			                       WHERE d.invoice_id = o.id AND (d.kind = 'handed' OR l.first_opened_at IS NOT NULL
			                         OR (SELECT e.event FROM message_events e WHERE e.message_id = d.message_id ORDER BY e.at DESC, e.id DESC LIMIT 1) IN ('sent', 'delivered'))) THEN 'reached'
			         WHEN EXISTS (SELECT 1 FROM invoice_deliveries d WHERE d.invoice_id = o.id) THEN 'tried'
			         ELSE 'none' END,
			       (SELECT count(*) FROM open x JOIN work_orders xw ON xw.id = x.work_order_id JOIN work_orders ow ON ow.id = o.work_order_id
			         WHERE x.id <> o.id AND x.owed <> 0 AND xw.customer_id = ow.customer_id)::int,
			       (SELECT count(*) FROM receivable_events e WHERE e.invoice_id = o.id AND e.kind = 'reminder')::int
			FROM open o
			LEFT JOIN receivable_cases c ON c.invoice_id = o.id
			LEFT JOIN users ou ON ou.id = c.owner_id LEFT JOIN people op ON op.id = ou.person_id
			WHERE o.owed <> 0
			ORDER BY (o.owed < 0) DESC, (o.issued_at AT TIME ZONE $1)::date + o.terms`, loc.String())
		if err != nil {
			return fmt.Errorf("list the worklist: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var w WorkItem
			var due time.Time
			var next *time.Time
			if err := rows.Scan(&w.InvoiceID, &w.Reference, &w.Customer, &w.Outstanding, &due, &w.OwnerID, &w.Owner, &next,
				&w.Disputed, &w.LastNote, &w.LastAt, &w.Delivered, &w.OtherOpen, &w.Reminders); err != nil {
				return fmt.Errorf("scan the worklist: %w", err)
			}
			w.Due = time.Date(due.Year(), due.Month(), due.Day(), 0, 0, 0, 0, loc)
			if next != nil {
				n := time.Date(next.Year(), next.Month(), next.Day(), 0, 0, 0, 0, loc)
				w.Next = &n
			}
			td := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, loc)
			w.FollowToday = w.Next != nil && !w.Next.After(td)
			if td.After(w.Due) {
				w.DaysLate = int(td.Sub(w.Due).Hours() / 24)
			}
			out = append(out, w)
		}
		return rows.Err()
	})
	return out, err
}

// DeskPerson is somebody who can have an invoice to follow up.
type DeskPerson struct {
	ID   string
	Name string
}

// DeskPeople lists the people who work with invoices.
func DeskPeople(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]DeskPerson, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	var out []DeskPerson
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT u.id::text, p.display_name FROM users u JOIN people p ON p.id = u.person_id
			WHERE u.active AND u.role IN ('owner', 'admin', 'service_advisor') ORDER BY p.display_name`)
		if err != nil {
			return fmt.Errorf("list desk people: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var d DeskPerson
			if err := rows.Scan(&d.ID, &d.Name); err != nil {
				return fmt.Errorf("scan desk person: %w", err)
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}
