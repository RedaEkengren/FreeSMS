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

// Getting an invoice or a credit note to the customer, and knowing whether it
// got there.
//
// There is no PDF: the document's rows are frozen, so the page is the document
// and printing is the browser's. A link opens that one document and nothing
// else. Each attempt to deliver it is a row, and a message the provider took is
// not a message the customer has: only opening the link, or the counter
// handing it over, says that.

// DocumentLinkDays is how long a document's link stays open.
const DocumentLinkDays = 90

// SendInput is a document to be sent.
type SendInput struct {
	// "sms" or "email".
	Channel string
	// The phone number or email address.
	Address string
	// Who it is for. The desk confirms this: the vehicle's owner, whoever
	// booked and whoever pays need not be one person.
	RecipientName string
	// The desk has looked at who the invoice is for and who it is going to.
	Confirmed bool
	Subject   string
	// The words, with {link} where the link goes.
	Body      string
	NotBefore time.Time
	// The workshop's address, to make the link whole.
	LinkBase string
}

// SendDocument queues a message with a link to one invoice or credit note. A
// new link closes the earlier ones for the same document: sending again is how
// a link that went to the wrong place is put right.
func SendDocument(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, invoiceID string, in SendInput) (string, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return "", access.ErrForbidden
	}
	if !looksLikeUUID(invoiceID) {
		return "", ErrNotFound
	}
	in.RecipientName, in.Address = strings.TrimSpace(in.RecipientName), strings.TrimSpace(in.Address)
	if in.RecipientName == "" || len([]rune(in.RecipientName)) > 100 {
		return "", fmt.Errorf("%w: say who it is for", ErrInvalid)
	}
	if !in.Confirmed {
		return "", fmt.Errorf("%w: confirm who it is for and where it is going", ErrInvalid)
	}
	if in.Channel != "sms" && in.Channel != "email" {
		return "", fmt.Errorf("%w: a text or an email", ErrInvalid)
	}
	if in.Address == "" {
		return "", fmt.Errorf("%w: nowhere to send it", ErrInvalid)
	}
	if !strings.Contains(in.Body, "{link}") {
		return "", fmt.Errorf("%w: the message has to carry the link", ErrInvalid)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))

	var id string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var jobID string
		if err := tx.QueryRow(ctx, `SELECT work_order_id::text FROM invoices WHERE id = $1`, invoiceID).Scan(&jobID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("read invoice: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE invoice_links SET revoked_at = clock_timestamp(), revoked_by = $2
			WHERE invoice_id = $1 AND revoked_at IS NULL`, invoiceID, scope.UserID); err != nil {
			return fmt.Errorf("close earlier links: %w", err)
		}
		var linkID string
		if err := tx.QueryRow(ctx, `
			INSERT INTO invoice_links (shop_id, invoice_id, token_sha256, created_by, expires_at)
			VALUES ($1, $2, $3, $4, clock_timestamp() + make_interval(days => $5)) RETURNING id`,
			scope.ShopID, invoiceID, sum[:], scope.UserID, DocumentLinkDays).Scan(&linkID); err != nil {
			return fmt.Errorf("make link: %w", err)
		}
		not := in.NotBefore
		if not.IsZero() {
			not = time.Now()
		}
		msgID, err := queueMessageTx(ctx, tx, scope, NewMessage{
			JobID: jobID, About: "invoice", Channel: in.Channel, Recipient: in.Address, Subject: in.Subject,
			Body: strings.ReplaceAll(in.Body, "{link}", strings.TrimRight(in.LinkBase, "/")+"/d/"+token), NotBefore: not,
		})
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO invoice_deliveries (shop_id, invoice_id, kind, recipient_name, message_id, link_id, by_user)
			VALUES ($1, $2, 'message', $3, $4, $5, $6) RETURNING id`,
			scope.ShopID, invoiceID, in.RecipientName, msgID, linkID, scope.UserID).Scan(&id)
	})
	return id, err
}

// HandInput is a document given over without a message.
type HandInput struct {
	RecipientName string
	// "printed", "handed" or "posted".
	How  string
	Note string
}

// HandDocumentOver records that the document was put in somebody's hands: a
// print at the counter, handed over, posted. The only way to deliver one when
// messaging is not set up.
func HandDocumentOver(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, invoiceID string, in HandInput) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(invoiceID) {
		return ErrNotFound
	}
	in.RecipientName, in.Note = strings.TrimSpace(in.RecipientName), strings.TrimSpace(in.Note)
	if in.RecipientName == "" || len([]rune(in.RecipientName)) > 100 {
		return fmt.Errorf("%w: say who it was given to", ErrInvalid)
	}
	switch in.How {
	case "printed", "handed", "posted":
	default:
		return fmt.Errorf("%w: printed, handed over or posted", ErrInvalid)
	}
	if len([]rune(in.Note)) > 300 {
		return fmt.Errorf("%w: keep the note to 300 characters", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO invoice_deliveries (shop_id, invoice_id, kind, recipient_name, how, note, by_user)
			SELECT $1, i.id, 'handed', $3, $4, nullif($5, ''), $6 FROM invoices i WHERE i.id = $2`,
			scope.ShopID, invoiceID, in.RecipientName, in.How, in.Note, scope.UserID)
		if err != nil {
			return fmt.Errorf("record handover: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// RevokeDocumentLinks closes every open link to a document. It stops working
// at once; what was already printed or read stays printed or read.
func RevokeDocumentLinks(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, invoiceID string) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(invoiceID) {
		return ErrNotFound
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT exists(SELECT 1 FROM invoices WHERE id = $1)`, invoiceID).Scan(&exists); err != nil {
			return fmt.Errorf("read invoice: %w", err)
		}
		if !exists {
			return ErrNotFound
		}
		_, err := tx.Exec(ctx, `UPDATE invoice_links SET revoked_at = clock_timestamp(), revoked_by = $2 WHERE invoice_id = $1 AND revoked_at IS NULL`,
			invoiceID, scope.UserID)
		return err
	})
}

// InvoiceDelivery is one attempt to get a document to the customer.
type InvoiceDelivery struct {
	ID            string
	Kind          string
	RecipientName string
	Channel       string
	Address       string
	How           string
	Note          string
	By            string
	At            time.Time
	// Where the message got to: queued, retry, sent, delivered, failed.
	Message   string
	MessageAt time.Time
	// Whether the customer has opened the link, and how often.
	Opened   *time.Time
	Opens    int
	LinkLive bool
	LinkEnds *time.Time
	HasLink  bool
}

// Status says where the delivery stands, as a catalogue key. A message the
// provider took is "sent", not "received"; only opening the link, or a hand
// over, says the customer has it.
func (d InvoiceDelivery) Status() string {
	switch {
	case d.Kind == "handed":
		switch d.How {
		case "printed":
			return "printed at the counter"
		case "posted":
			return "posted"
		}
		return "handed over"
	case d.Opened != nil:
		return "opened by the customer"
	}
	switch d.Message {
	case "delivered":
		return "delivered to the phone or inbox"
	case "sent":
		return "handed to the provider"
	case "failed":
		return "did not go"
	case "retry":
		return "trying again"
	}
	return "waiting to be sent"
}

// Reached reports whether the customer is known to have it.
func (d InvoiceDelivery) Reached() bool {
	return d.Kind == "handed" || d.Opened != nil || d.Message == "delivered"
}

// Failed reports a delivery that did not go and is not known to have arrived.
func (d InvoiceDelivery) Failed() bool { return d.Message == "failed" && !d.Reached() }

// DeliveriesFor lists the attempts to deliver a document, newest first.
func DeliveriesFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, invoiceID string) ([]InvoiceDelivery, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(invoiceID) {
		return nil, ErrNotFound
	}
	var out []InvoiceDelivery
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT d.id::text, d.kind, d.recipient_name, coalesce(m.channel, ''), coalesce(m.recipient, ''), coalesce(d.how, ''), coalesce(d.note, ''),
			       coalesce(p.display_name, ''), d.at,
			       coalesce((SELECT e.event FROM message_events e WHERE e.message_id = m.id ORDER BY e.at DESC, e.id DESC LIMIT 1), ''),
			       coalesce((SELECT e.at FROM message_events e WHERE e.message_id = m.id ORDER BY e.at DESC, e.id DESC LIMIT 1), d.at),
			       l.first_opened_at, coalesce(l.opens, 0),
			       l.id IS NOT NULL AND l.revoked_at IS NULL AND l.expires_at > now(), l.expires_at, l.id IS NOT NULL
			FROM invoice_deliveries d
			LEFT JOIN outbound_messages m ON m.id = d.message_id
			LEFT JOIN invoice_links l ON l.id = d.link_id
			LEFT JOIN users u ON u.id = d.by_user LEFT JOIN people p ON p.id = u.person_id
			WHERE d.invoice_id = $1 ORDER BY d.at DESC, d.id`, invoiceID)
		if err != nil {
			return fmt.Errorf("list deliveries: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var d InvoiceDelivery
			if err := rows.Scan(&d.ID, &d.Kind, &d.RecipientName, &d.Channel, &d.Address, &d.How, &d.Note, &d.By, &d.At,
				&d.Message, &d.MessageAt, &d.Opened, &d.Opens, &d.LinkLive, &d.LinkEnds, &d.HasLink); err != nil {
				return fmt.Errorf("scan delivery: %w", err)
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

// Undelivered is a document the customer is not known to have.
type Undelivered struct {
	InvoiceID  string
	Reference  string
	IssuedAt   time.Time
	GrossMinor int64
	Customer   string
	CreditNote bool
	// "failed" when every try went wrong, "none" when nothing was tried.
	Why string
}

// UndeliveredDocuments lists issued documents the customer is not known to have:
// every try failed, or none was made. Recent ones only; the follow-up is for
// what can still be put right.
func UndeliveredDocuments(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, since time.Time) ([]Undelivered, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil, access.ErrForbidden
	}
	var out []Undelivered
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			WITH status AS (
			  SELECT i.id, i.series || '-' || i.number AS ref, i.issued_at, i.gross_minor, i.customer_name, i.credit_of_id IS NOT NULL AS credit,
			         count(d.id) AS tries,
			         bool_or(d.kind = 'handed' OR l.first_opened_at IS NOT NULL
			                 OR (SELECT e.event FROM message_events e WHERE e.message_id = d.message_id ORDER BY e.at DESC, e.id DESC LIMIT 1) = 'delivered') AS reached,
			         bool_or((SELECT e.event FROM message_events e WHERE e.message_id = d.message_id ORDER BY e.at DESC, e.id DESC LIMIT 1) = 'failed') AS failed
			  FROM invoices i
			  LEFT JOIN invoice_deliveries d ON d.invoice_id = i.id
			  LEFT JOIN invoice_links l ON l.id = d.link_id
			  WHERE i.issued_at >= $1
			  GROUP BY i.id)
			SELECT s.id::text, s.ref, s.issued_at, s.gross_minor, s.customer_name, s.credit,
			       CASE WHEN s.tries = 0 THEN 'none' ELSE 'failed' END
			FROM status s
			JOIN invoices o ON o.id = coalesce((SELECT x.credit_of_id FROM invoices x WHERE x.id = s.id), s.id)
			WHERE NOT coalesce(s.reached, false) AND (s.tries = 0 OR coalesce(s.failed, false))
			  -- A document whose invoice is paid up has evidently been received;
			  -- chasing it is for what is still owed.
			  AND (o.gross_minor
			       + coalesce((SELECT sum(c.gross_minor) FROM invoices c WHERE c.credit_of_id = o.id), 0)
			       - coalesce((SELECT sum(p.amount_minor + p.rounding_minor) FROM invoice_payments p WHERE p.invoice_id = o.id), 0)) <> 0
			ORDER BY s.issued_at`, since)
		if err != nil {
			return fmt.Errorf("list undelivered: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var u Undelivered
			if err := rows.Scan(&u.InvoiceID, &u.Reference, &u.IssuedAt, &u.GrossMinor, &u.Customer, &u.CreditNote, &u.Why); err != nil {
				return fmt.Errorf("scan undelivered: %w", err)
			}
			out = append(out, u)
		}
		return rows.Err()
	})
	return out, err
}

// CustomerDocument is the document a link opens, and the balance as of today,
// kept apart from it: the document is what was issued, the balance is now.
type CustomerDocument struct {
	Document Document
	Balance  Balance
	ShopName string
}

// OpenDocumentLink opens a document on the customer's link. Wrong, expired and
// closed links are one answer. Opening is recorded: it is the one thing that
// says the customer has it.
func OpenDocumentLink(ctx context.Context, pool *pgxpool.Pool, shopID, token string) (CustomerDocument, error) {
	if token == "" {
		return CustomerDocument{}, ErrShareNotUsable
	}
	sum := sha256.Sum256([]byte(token))
	var out CustomerDocument
	err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		var invoiceID string
		if err := tx.QueryRow(ctx, `
			UPDATE invoice_links SET opens = opens + 1, last_opened_at = clock_timestamp(),
			       first_opened_at = coalesce(first_opened_at, clock_timestamp())
			WHERE token_sha256 = $1 AND revoked_at IS NULL AND expires_at > now()
			RETURNING invoice_id::text`, sum[:]).Scan(&invoiceID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrShareNotUsable
			}
			return fmt.Errorf("open link: %w", err)
		}
		if err := documentTx(ctx, tx, invoiceID, &out.Document); err != nil {
			return err
		}
		out.Document.Bands = bandsFrom(out.Document.Lines)
		b, err := balanceTx(ctx, tx, invoiceID, false)
		if err != nil {
			return err
		}
		out.Balance = b
		return tx.QueryRow(ctx, `SELECT name FROM shops LIMIT 1`).Scan(&out.ShopName)
	})
	return out, err
}
