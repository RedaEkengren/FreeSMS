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

// A link to the job, for the customer to see where their car is without
// ringing: being worked on, waiting for a part, ready to collect, and once
// invoiced what is owed. It needs no SMS provider -- the front desk sends it
// however it already talks to the customer.

// CreateJobLink makes the customer's link to a job and returns its token,
// once. Making a new one closes the job's earlier links: there is one link
// per job, and a link sent to the wrong number is put right by making another.
func CreateJobLink(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, jobID string) (string, error) {
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
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT true FROM work_orders WHERE id = $1 FOR UPDATE`, jobID).Scan(&exists); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("lock order: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE job_links SET revoked_at = now()
			WHERE work_order_id = $1 AND revoked_at IS NULL`, jobID); err != nil {
			return fmt.Errorf("close earlier links: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO job_links (shop_id, work_order_id, token_sha256, created_by, expires_at)
			VALUES ($1, $2, $3, $4, now() + $5::interval)`,
			scope.ShopID, jobID, sum[:], scope.UserID, fmt.Sprintf("%d seconds", int(jobLinkLifetime.Seconds()))); err != nil {
			return fmt.Errorf("create link: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

// jobLinkLifetime is long enough for a job that waits a week for a part.
const jobLinkLifetime = 30 * 24 * time.Hour

// CustomerStatus is what the customer's link shows. Nothing about money
// until the job is invoiced, and then no more than the invoice says.
type CustomerStatus struct {
	ShopName     string
	ShopPhone    string
	ShopAddress  string
	Registration string
	Make, Model  string
	// What the customer reads: in_work, waiting_for_part, ready, done.
	Stage        string
	CarAway      bool
	ExpectedBack *time.Time

	// Set once there is an invoice.
	Invoiced         bool
	InvoiceReference string
	GrossMinor       int64
	OutstandingMinor int64
	PayTo            string
	Due              time.Time
}

// JobStatus answers a customer's link. Wrong, expired and closed are one
// answer, ErrShareNotUsable: whoever holds a link that does not work does not
// need to be told which kind.
func JobStatus(ctx context.Context, pool *pgxpool.Pool, shopID, token string) (CustomerStatus, error) {
	if token == "" {
		return CustomerStatus{}, ErrShareNotUsable
	}
	sum := sha256.Sum256([]byte(token))
	var st CustomerStatus
	err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		var jobID string
		var state State
		err := tx.QueryRow(ctx, `
			SELECT w.id, w.state, s.name, coalesce(s.phone, ''),
			       concat_ws(', ', s.address_line1, s.postal_code || ' ' || s.city),
			       coalesce(r.registration, ''), coalesce(v.make, ''), coalesce(v.model, ''),
			       coalesce(s.payment_reference, '')
			FROM job_links l
			JOIN work_orders w ON w.id = l.work_order_id
			JOIN shops s       ON s.id = w.shop_id
			JOIN vehicles v    ON v.id = w.vehicle_id
			LEFT JOIN vehicle_registrations r ON r.vehicle_id = v.id AND r.valid_to IS NULL
			WHERE l.token_sha256 = $1 AND l.revoked_at IS NULL AND l.expires_at > now()`, sum[:]).
			Scan(&jobID, &state, &st.ShopName, &st.ShopPhone, &st.ShopAddress,
				&st.Registration, &st.Make, &st.Model, &st.PayTo)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrShareNotUsable
		}
		if err != nil {
			return fmt.Errorf("read link: %w", err)
		}
		st.Stage = customerStage(state)

		p, err := presenceTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		st.CarAway, st.ExpectedBack = !p.Here, p.ExpectedBack

		var invoiceID string
		err = tx.QueryRow(ctx, `
			SELECT id FROM invoices WHERE work_order_id = $1 AND credit_of_id IS NULL
			ORDER BY issued_at DESC LIMIT 1`, jobID).Scan(&invoiceID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read invoice: %w", err)
		}
		b, err := balanceTx(ctx, tx, invoiceID, false)
		if err != nil {
			return err
		}
		st.Invoiced = true
		st.InvoiceReference, st.GrossMinor = b.Reference, b.GrossMinor+b.CreditedMinor
		st.OutstandingMinor, st.Due = b.OutstandingMinor(), b.Due
		return nil
	})
	return st, err
}

// customerStage is the state in the customer's terms. They do not need to
// know the difference between estimated and awaiting approval; they need to
// know whether to come.
func customerStage(s State) string {
	switch s {
	case StateAwaitingParts:
		return "waiting_for_part"
	case StateReady, StateInvoiced:
		return "ready"
	case StateClosed, StateCancelled, StateDeclined:
		return "done"
	}
	return "in_work"
}
