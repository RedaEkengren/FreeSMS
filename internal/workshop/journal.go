package workshop

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// The payment journal (#152): money that moved in the shop's own channels,
// recorded against an invoice, a deposit or a payout, refunded in part or in
// whole, and matched to the bank's statement. FreeSMS never handles the
// money; a provider that does can be put behind PaymentProvider later (#213)
// without changing any of this.

// PaymentProvider is a service that takes or returns money for the shop,
// with the shop's own account: what the journal asks of one. Nothing here
// calls a provider inside a transaction.
type PaymentProvider interface {
	// Name is the provider's name in the journal, as a payment it reports
	// names it (provider, external id).
	Name() string
	// CreateRequest asks for a payment and returns the provider's id for
	// it; "" when there is nothing to follow.
	CreateRequest(ctx context.Context, r PaymentRequest) (externalID string, err error)
	// Status reads where a request stands.
	Status(ctx context.Context, externalID string) (ProviderStatus, error)
	// Refund gives back part or all of a payment the provider took.
	Refund(ctx context.Context, externalID string, amountMinor int64) (refundID string, err error)
	// VerifyWebhook checks a notification is the provider's and says what
	// it reports.
	VerifyWebhook(headers map[string]string, body []byte) (ProviderEvent, error)
}

// PaymentRequest is a payment asked for.
type PaymentRequest struct {
	AmountMinor int64
	Reference   string
	Description string
}

// ProviderStatus is where a request stands at the provider.
type ProviderStatus string

// The states a request can be in.
const (
	ProviderPending  ProviderStatus = "pending"
	ProviderPaid     ProviderStatus = "paid"
	ProviderFailed   ProviderStatus = "failed"
	ProviderRefunded ProviderStatus = "refunded"
	ProviderUnknown  ProviderStatus = "unknown"
)

// ProviderEvent is what a verified notification reports.
type ProviderEvent struct {
	ExternalID  string
	Status      ProviderStatus
	AmountMinor int64
}

// ErrNoWebhooks is a notification sent to a provider that sends none.
var ErrNoWebhooks = errors.New("workshop: this way of paying sends no notifications")

// ManualProvider is the shop's own channels: the customer pays by bank,
// Swish, the terminal or cash, and somebody records it when it arrives. It
// asks nobody, so it never fails, and it knows nothing a person has not
// recorded.
type ManualProvider struct{}

// Name is "manual".
func (ManualProvider) Name() string { return "manual" }

// CreateRequest has nothing to send: the shop asks the customer itself.
func (ManualProvider) CreateRequest(context.Context, PaymentRequest) (string, error) { return "", nil }

// Status is unknown: what arrived is what was recorded.
func (ManualProvider) Status(context.Context, string) (ProviderStatus, error) {
	return ProviderUnknown, nil
}

// Refund has nothing to send: the shop pays it back itself and records that
// with RefundPayment.
func (ManualProvider) Refund(context.Context, string, int64) (string, error) { return "", nil }

// VerifyWebhook refuses every notification: the shop's own channels send
// none.
func (ManualProvider) VerifyWebhook(map[string]string, []byte) (ProviderEvent, error) {
	return ProviderEvent{}, ErrNoWebhooks
}

// ReferenceLabel is what a way of paying calls its own reference: the
// bank's, Swish's, the terminal's receipt. A catalogue key.
func ReferenceLabel(method string) string {
	switch method {
	case "bank":
		return "Bank reference"
	case "swish":
		return "Swish reference"
	case "card":
		return "Terminal receipt number"
	}
	return "Reference"
}

// The purposes of a deposit and of a payout.
var (
	DepositPurposes = []string{"reservation", "rental", "down_payment", "other"}
	PayoutPurposes  = []string{"purchase", "consignment", "other"}
)

var (
	errPurpose                 = fmt.Errorf("%w: that is not what it can be for", ErrInvalid)
	errExternalTwice           = fmt.Errorf("%w: that payment has already been recorded", ErrInvalid)
	errKeyOtherPayment         = fmt.Errorf("%w: that request was already used to record another payment", ErrInvalid)
	errRefundTooMuch           = fmt.Errorf("%w: that is more than is left of the payment to give back", ErrInvalid)
	errNotAPayment             = fmt.Errorf("%w: only money received is refunded", ErrInvalid)
	errReversedRefund          = fmt.Errorf("%w: that payment has been reversed, so there is nothing of it to give back", ErrInvalid)
	errRefundsStand            = fmt.Errorf("%w: that payment has refunds; reverse them first, then the payment", ErrInvalid)
	errDepositShort            = fmt.Errorf("%w: that is more than the deposit still holds", ErrInvalid)
	errRentalDepositNeedsClaim = fmt.Errorf("%w: a rental's deposit is applied from the damage case it settles", ErrInvalid)
	errGivenBackTooMuch        = fmt.Errorf("%w: that is more than the customer paid beyond the invoice; if the invoice was wrong, credit it first", ErrInvalid)
	// ErrReversalNoReason is reversing a payment without saying why.
	ErrReversalNoReason = fmt.Errorf("%w: say why the payment is reversed, in 300 characters or fewer", ErrInvalid)
	// ErrRefundNoReason is refunding a payment without saying why.
	ErrRefundNoReason = fmt.Errorf("%w: say why the money is given back, in 300 characters or fewer", ErrInvalid)
)

// mayHoldDeposit checks roles and modules for a deposit of purpose: the
// salesperson's reservations and down payments, the rental desk's rental
// deposits, and the front desk any.
func mayHoldDeposit(roles access.Roles, scope access.Scope, purpose string) error {
	switch purpose {
	case "reservation", "down_payment":
		if !roles.SellsVehicles() && !roles.SeesCustomerPersonalData() {
			return access.ErrForbidden
		}
		return scope.Require(access.Dealer)
	case "rental":
		if !roles.HandsOverRentals() && !roles.SeesCustomerPersonalData() {
			return access.ErrForbidden
		}
		return scope.Require(access.Rental)
	case "other":
		if !roles.SeesCustomerPersonalData() {
			return access.ErrForbidden
		}
		return nil
	}
	return errPurpose
}

// mayPayOut checks roles and modules for a payout of purpose: the buyer's,
// who agreed the price, or whoever runs the shop. A payout of a purchase is
// the purchase price, which only they may see.
func mayPayOut(roles access.Roles, scope access.Scope, purpose string) error {
	switch purpose {
	case "purchase", "consignment":
		if !roles.SeesPurchasePrice() {
			return access.ErrForbidden
		}
		return scope.Require(access.Dealer)
	case "other":
		if !roles.RunsTheShop() {
			return access.ErrForbidden
		}
		return nil
	}
	return errPurpose
}

// partySeenTx refuses a party the caller may not see (PartyByID): naming
// one on a deposit, a payout or a contract is reading it.
func partySeenTx(ctx context.Context, tx pgx.Tx, scope access.Scope, partyID string) error {
	var visible bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM party_roles r WHERE r.party_id = ANY((SELECT party_ids_read_as(party_now($1)))::uuid[]) AND r.role = ANY($2))`,
		partyID, partyRolesSeen(scope.Role, PartyRoles)).Scan(&visible); err != nil {
		return fmt.Errorf("read party: %w", err)
	}
	if !visible {
		return ErrNotFound
	}
	return nil
}

// NewHolding is a deposit or a payout to open: for whom, what for, and what
// it is held against or paid for, when that is known.
type NewHolding struct {
	PartyID     string
	Purpose     string
	SubjectKind string
	SubjectID   string
	Note        string
}

// CreateDeposit opens a deposit: money to be held for a party against
// something not yet owed. Holding it is not income; the payments against
// it say what was received, applied and given back.
func CreateDeposit(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, n NewHolding) (string, error) {
	if err := mayHoldDeposit(scope.Role, scope, n.Purpose); err != nil {
		return "", err
	}
	return createHolding(ctx, pool, scope, "deposits", n)
}

// CreatePayout opens a payout: money the shop is to pay a party -- a
// private seller for their car.
func CreatePayout(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, n NewHolding) (string, error) {
	if err := mayPayOut(scope.Role, scope, n.Purpose); err != nil {
		return "", err
	}
	return createHolding(ctx, pool, scope, "payouts", n)
}

func createHolding(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, table string, n NewHolding) (string, error) {
	if !looksLikeUUID(n.PartyID) || (n.SubjectID != "" && !looksLikeUUID(n.SubjectID)) {
		return "", ErrNotFound
	}
	note, err := checkNote(n.Note)
	if err != nil {
		return "", err
	}
	if (n.SubjectKind == "") != (n.SubjectID == "") {
		return "", fmt.Errorf("%w: what it is for needs both its kind and which one", ErrInvalid)
	}
	var id string
	err = database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := partySeenTx(ctx, tx, scope, n.PartyID); err != nil {
			return err
		}
		// Dealer: a car's payout pays its purchase, to those it owes (#162).
		if table == "payouts" {
			if err := payoutSubjectTx(ctx, tx, n.Purpose, n.SubjectKind, n.SubjectID, n.PartyID); err != nil {
				return err
			}
		}
		err := tx.QueryRow(ctx, `
			INSERT INTO `+table+` (shop_id, party_id, purpose, subject_kind, subject_id, note, created_by)
			VALUES ($1, party_now($2), $3, nullif($4, ''), NULLIF($5, '')::uuid, nullif($6, ''), $7) RETURNING id`,
			scope.ShopID, n.PartyID, n.Purpose, n.SubjectKind, n.SubjectID, note, scope.UserID).Scan(&id)
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23514" {
			return errPurpose
		}
		return err
	})
	return id, err
}

// DepositBalance is where a deposit stands, from its payments: received,
// applied to what came to be owed, given back, and held still.
type DepositBalance struct {
	DepositID string
	Purpose   string
	Received  int64
	Applied   int64
	Refunded  int64
	Payments  []Payment
}

// Held is what the shop still holds of it.
func (b DepositBalance) Held() int64 { return b.Received - b.Applied - b.Refunded }

func depositTx(ctx context.Context, tx pgx.Tx, id string, lock bool) (DepositBalance, error) {
	b := DepositBalance{DepositID: id}
	if lock {
		// What it holds and what is taken from it are one step. The row
		// itself is never updated, so the lock is the deposit's id.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('deposit:' || $1::text, 0))`, id); err != nil {
			return b, fmt.Errorf("lock deposit: %w", err)
		}
	}
	if err := tx.QueryRow(ctx, `SELECT purpose FROM deposits WHERE id = $1`, id).Scan(&b.Purpose); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return b, ErrNotFound
		}
		return b, fmt.Errorf("read deposit: %w", err)
	}
	ps, err := paymentsOfTx(ctx, tx, "deposit_id", id)
	if err != nil {
		return b, err
	}
	b.Payments = ps
	for _, p := range ps {
		switch {
		case p.Method == "deposit":
			b.Applied -= p.Settles()
		case p.Settles() > 0:
			b.Received += p.Settles()
		default:
			b.Refunded -= p.Settles()
		}
	}
	return b, nil
}

// DepositBalanceFor reads a deposit for those who may hold it.
func DepositBalanceFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, depositID string) (DepositBalance, error) {
	if !looksLikeUUID(depositID) {
		return DepositBalance{}, ErrNotFound
	}
	var b DepositBalance
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if b, err = depositTx(ctx, tx, depositID, false); err != nil {
			return err
		}
		return mayReadHolding(scope.Role, "deposit", b.Purpose)
	})
	if err != nil {
		return DepositBalance{}, err
	}
	return b, nil
}

// mayReadHolding is whether roles may read a deposit or a payout of
// purpose: whoever may make one. Reading is Core's: a module switched off
// keeps its money readable.
func mayReadHolding(roles access.Roles, kind, purpose string) error {
	var may bool
	switch {
	case kind == "payout" && purpose == "other":
		may = roles.RunsTheShop()
	case kind == "payout":
		may = roles.SeesPurchasePrice()
	case purpose == "rental":
		may = roles.HandsOverRentals() || roles.SeesCustomerPersonalData()
	case purpose == "other":
		may = roles.SeesCustomerPersonalData()
	default:
		may = roles.SellsVehicles() || roles.SeesCustomerPersonalData()
	}
	if !may {
		return access.ErrForbidden
	}
	return nil
}

// paymentsOfTx reads the payments against one target.
func paymentsOfTx(ctx context.Context, tx pgx.Tx, column, id string) ([]Payment, error) {
	rows, err := tx.Query(ctx, `
		SELECT p.id, p.series, p.number, p.amount_minor, p.rounding_minor, p.method, p.paid_on,
		       p.recorded_at, coalesce(pe.display_name, ''), coalesce(p.reference, ''),
		       coalesce(p.reverses_id::text, ''), coalesce(p.refunds_id::text, ''),
		       EXISTS (SELECT 1 FROM invoice_payments r WHERE r.reverses_id = p.id)
		FROM invoice_payments p
		LEFT JOIN users u ON u.id = p.recorded_by
		LEFT JOIN people pe ON pe.id = u.person_id
		WHERE p.`+column+` = $1
		ORDER BY p.paid_on, p.recorded_at`, id)
	if err != nil {
		return nil, fmt.Errorf("read payments: %w", err)
	}
	defer rows.Close()
	var out []Payment
	for rows.Next() {
		var p Payment
		if err := rows.Scan(&p.ID, &p.Series, &p.Number, &p.AmountMinor, &p.RoundingMinor, &p.Method,
			&p.PaidOn, &p.RecordedAt, &p.RecordedBy, &p.Reference, &p.Reverses, &p.RefundsID, &p.Reversed); err != nil {
			return nil, fmt.Errorf("scan payment: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// checkMoney is the checks every recorded payment passes: a way of paying,
// a day it moved that is not ahead, an amount above nothing, and the
// provider's id with its provider.
func checkMoney(p NewPayment) error {
	switch {
	case !knownMethod(p.Method):
		return fmt.Errorf("%w: %q is not a way money arrives", ErrInvalid, p.Method)
	case p.PaidOn.IsZero():
		return fmt.Errorf("%w: when did it arrive?", ErrInvalid)
	case p.PaidOn.After(time.Now().Add(24 * time.Hour)):
		return fmt.Errorf("%w: money that has not arrived yet cannot be recorded as arrived", ErrInvalid)
	case p.AmountMinor == nil || *p.AmountMinor <= 0:
		return fmt.Errorf("%w: say how much, above nothing", ErrInvalid)
	case (p.Provider == "") != (p.ExternalID == ""):
		return fmt.Errorf("%w: a provider's id needs the provider, and the other way round", ErrInvalid)
	case len(p.ExternalID) > 200 || len(strings.TrimSpace(p.Key)) > 100 || utf8.RuneCountInString(p.Reference) > 100:
		return fmt.Errorf("%w: a reference is longer than it can be", ErrInvalid)
	}
	return nil
}

// RecordDepositPayment records money received into a deposit.
func RecordDepositPayment(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, depositID string, p NewPayment) (Payment, error) {
	if !looksLikeUUID(depositID) {
		return Payment{}, ErrNotFound
	}
	if err := checkMoney(p); err != nil {
		return Payment{}, err
	}
	out := Payment{DepositID: depositID, AmountMinor: *p.AmountMinor, Method: p.Method, PaidOn: p.PaidOn,
		Reference: strings.TrimSpace(p.Reference), Provider: p.Provider, ExternalID: p.ExternalID, Key: p.Key}
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		b, err := depositTx(ctx, tx, depositID, true)
		if err != nil {
			return err
		}
		if err := mayHoldDeposit(scope.Role, scope, b.Purpose); err != nil {
			return err
		}
		_, err = insertPaymentOnce(ctx, tx, scope, &out)
		return err
	})
	return out, err
}

// RecordPayout records money paid out to the payout's party.
func RecordPayout(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, payoutID string, p NewPayment) (Payment, error) {
	if !looksLikeUUID(payoutID) {
		return Payment{}, ErrNotFound
	}
	if err := checkMoney(p); err != nil {
		return Payment{}, err
	}
	out := Payment{PayoutID: payoutID, AmountMinor: -*p.AmountMinor, Method: p.Method, PaidOn: p.PaidOn,
		Reference: strings.TrimSpace(p.Reference), Provider: p.Provider, ExternalID: p.ExternalID, Key: p.Key}
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var purpose, subjectKind, subjectID, partyID string
		err := tx.QueryRow(ctx, `SELECT purpose, coalesce(subject_kind, ''), coalesce(subject_id::text, ''), party_id FROM payouts WHERE id = $1`,
			payoutID).Scan(&purpose, &subjectKind, &subjectID, &partyID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read payout: %w", err)
		}
		if err := mayPayOut(scope.Role, scope, purpose); err != nil {
			return err
		}
		// The same request again finds the payment it made, whatever the
		// checks say now: the money left when they let it. So the checks
		// are the gate, asked only of a payment not made yet.
		var over *PurchaseBalance
		var why string
		replayed, err := insertPaymentOnceGated(ctx, tx, scope, &out, func() error {
			// Dealer: no money leaves for a car before its checks pass
			// (#162), so a purchase's payout names the purchase it pays.
			if err := payoutSubjectTx(ctx, tx, purpose, subjectKind, subjectID, partyID); err != nil {
				return err
			}
			if purpose != "purchase" {
				return nil
			}
			if err := purchaseChecksClearTx(ctx, tx, subjectID); err != nil {
				return err
			}
			// Nor more than the purchase says is owed, all payouts together
			// (#248): asked, with a reason, not silently accepted.
			b, err := purchaseBalanceTx(ctx, tx, subjectID)
			if err != nil {
				return err
			}
			if *p.AmountMinor > b.LeftMinor()+overpaymentMargin {
				if !p.ConfirmedOverpayment {
					return ErrPayoutOverPrice
				}
				if why, err = checkReason(p.OverpaymentReason, ErrOverpaymentNoReason); err != nil {
					return err
				}
				over = &b
			}
			return nil
		})
		if err != nil || replayed || over == nil {
			return err
		}
		return auditTx(ctx, tx, scope, out.ID, "payment.overpayment_confirmed", "", "", why,
			map[string]any{"purchase_id": subjectID, "amount_minor": -out.AmountMinor, "left_minor": over.LeftMinor()})
	})
	return out, err
}

// ApplyDeposit moves part or all of what a deposit holds onto an invoice
// that has come to be owed -- the rental's damage, the car's price. No
// money moves: two rows, out of the deposit and into the invoice, both
// the shop's own way of paying ("deposit"), in one transaction.
func ApplyDeposit(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, depositID, invoiceID string, amountMinor int64, key string) error {
	if !looksLikeUUID(depositID) || !looksLikeUUID(invoiceID) {
		return ErrNotFound
	}
	if amountMinor <= 0 {
		return fmt.Errorf("%w: say how much, above nothing", ErrInvalid)
	}
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	key = strings.TrimSpace(key)
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		// A rental's deposit settles only a claim that is settled, told
		// and past its objection (#196): from its damage case, never here.
		var purpose string
		if err := tx.QueryRow(ctx, `SELECT purpose FROM deposits WHERE id = $1`, depositID).Scan(&purpose); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("read deposit: %w", err)
		}
		if purpose == "rental" {
			return errRentalDepositNeedsClaim
		}
		_, err := applyDepositTx(ctx, tx, scope, depositID, invoiceID, amountMinor, key)
		return err
	})
}

// applyDepositTx is ApplyDeposit in the caller's transaction, its checks of
// the role and the identifiers made: for an invoice that applies the
// deposit it was paid against as it is issued (#174). Returns the row out
// of the deposit, or nothing when the same request was made before.
func applyDepositTx(ctx context.Context, tx pgx.Tx, scope access.Scope, depositID, invoiceID string, amountMinor int64, key string) (string, error) {
	var applied string
	err := func() error {
		d, err := depositTx(ctx, tx, depositID, true)
		if err != nil {
			return err
		}
		b, err := balanceTx(ctx, tx, invoiceID, true)
		if err != nil {
			return err
		}
		// The same request again, checked under the locks: a double click
		// waits for the first, then finds what it applied.
		if key != "" {
			if found, err := replayedApplicationTx(ctx, tx, key, depositID, b.InvoiceID, amountMinor); found || err != nil {
				return err
			}
		}
		switch {
		case amountMinor > d.Held():
			return fmt.Errorf("%w: that is more than the deposit holds", ErrInvalid)
		case amountMinor > b.OutstandingMinor():
			return fmt.Errorf("%w: that is more than is owed on the invoice", ErrInvalid)
		}
		now := time.Now()
		out := Payment{DepositID: depositID, AmountMinor: -amountMinor, Method: "deposit", PaidOn: now, Reference: b.Reference, Key: key}
		if key == "" {
			if err := insertPayment(ctx, tx, scope, &out); err != nil {
				return err
			}
		} else if err := insertKeyedTx(ctx, tx, scope, &out); err != nil {
			// The key taken by another request since: this one's, or not.
			if errors.Is(err, errKeyTaken) {
				_, err = replayedApplicationTx(ctx, tx, key, depositID, b.InvoiceID, amountMinor)
				if err == nil {
					err = errKeyOtherPayment
				}
			}
			return err
		}
		in := Payment{InvoiceID: b.InvoiceID, AmountMinor: amountMinor, Method: "deposit", PaidOn: now, Reference: out.Series + "-" + fmt.Sprint(out.Number)}
		if err := insertPayment(ctx, tx, scope, &in); err != nil {
			return err
		}
		// The two rows are one application: reversed, they go together
		// (0082).
		if _, err := tx.Exec(ctx, `INSERT INTO deposit_applications (shop_id, deposit_payment_id, invoice_payment_id) VALUES ($1, $2, $3)`,
			scope.ShopID, out.ID, in.ID); err != nil {
			return fmt.Errorf("record the application: %w", err)
		}
		applied = out.ID
		return closeIfFinishedTx(ctx, tx, b.WorkOrderID)
	}()
	return applied, err
}

// replayedApplicationTx reports whether key already applied amountMinor of
// depositID to invoiceID -- the same request again -- and refuses a key
// used for anything else.
func replayedApplicationTx(ctx context.Context, tx pgx.Tx, key, depositID, invoiceID string, amountMinor int64) (bool, error) {
	var deposit, invoice, method string
	var amount int64
	err := tx.QueryRow(ctx, `
		SELECT coalesce(p.deposit_id::text, ''), p.method, p.amount_minor, coalesce(i.invoice_id::text, '')
		FROM invoice_payments p
		LEFT JOIN deposit_applications a ON a.deposit_payment_id = p.id
		LEFT JOIN invoice_payments i ON i.id = a.invoice_payment_id
		WHERE p.request_key = $1`, key).Scan(&deposit, &method, &amount, &invoice)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the request: %w", err)
	}
	if method != "deposit" || !strings.EqualFold(deposit, depositID) || !strings.EqualFold(invoice, invoiceID) || amount != -amountMinor {
		return false, errKeyOtherPayment
	}
	return true, nil
}

// RefundPayment gives back part or all of one payment received -- into an
// invoice or a deposit -- as its own row linked to it, never an edit. Why
// is required. Refunds of one payment never add up to more than it was
// (0081), and everything given back on an invoice, by any path, is at most
// what the customer paid beyond it, credits counted: an invoice paid
// exactly is credited before its money goes back (0086). The database
// holds both.
func RefundPayment(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, paymentID string, p NewPayment, reason string) (Payment, error) {
	// Which payment, the operation decides once it knows what it is
	// against; somebody who moves money on nothing at all is refused first.
	if !scope.Role.SeesCustomerPersonalData() && !scope.Role.SellsVehicles() && !scope.Role.HandsOverRentals() && !scope.Role.SeesPurchasePrice() {
		return Payment{}, access.ErrForbidden
	}
	if !looksLikeUUID(paymentID) {
		return Payment{}, ErrNotFound
	}
	if err := checkMoney(p); err != nil {
		return Payment{}, err
	}
	reason, err := checkReason(reason, ErrRefundNoReason)
	if err != nil {
		return Payment{}, err
	}
	var out Payment
	err = database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		orig, err := paymentTargetTx(ctx, tx, scope, paymentID)
		if err != nil {
			return err
		}
		if orig.AmountMinor <= 0 || orig.Reverses != "" || orig.RefundsID != "" || orig.Method == "deposit" {
			return errNotAPayment
		}
		if orig.Reversed {
			return errReversedRefund
		}
		out = Payment{InvoiceID: orig.InvoiceID, DepositID: orig.DepositID, RefundsID: paymentID, AmountMinor: -*p.AmountMinor,
			Method: p.Method, PaidOn: p.PaidOn, Reference: strings.TrimSpace(p.Reference), Provider: p.Provider,
			ExternalID: p.ExternalID, Key: p.Key, Reason: reason}
		if out.Key != "" {
			if found, err := replayedPaymentTx(ctx, tx, out.Key, &out); found || err != nil {
				return err
			}
		}
		if orig.DepositID != "" {
			d, err := depositTx(ctx, tx, orig.DepositID, true)
			if err != nil {
				return err
			}
			if *p.AmountMinor > d.Held() {
				return errDepositShort
			}
		}
		_, err = insertPaymentOnce(ctx, tx, scope, &out)
		return err
	})
	return out, err
}

// paymentTargetTx reads a payment, locks what it is against, and checks the
// caller may move money on that: the front desk an invoice's, whoever holds
// a deposit or makes a payout theirs. Roles only, not the module: money the
// shop holds is given back, and a mistake put right, after the module that
// took it is switched off.
func paymentTargetTx(ctx context.Context, tx pgx.Tx, scope access.Scope, paymentID string) (Payment, error) {
	var p Payment
	err := tx.QueryRow(ctx, `
		SELECT id, coalesce(invoice_id::text, ''), coalesce(deposit_id::text, ''), coalesce(payout_id::text, ''),
		       amount_minor, rounding_minor, method, coalesce(reverses_id::text, ''), coalesce(refunds_id::text, ''),
		       EXISTS (SELECT 1 FROM invoice_payments r WHERE r.reverses_id = p.id)
		FROM invoice_payments p WHERE id = $1`, paymentID).Scan(&p.ID, &p.InvoiceID, &p.DepositID, &p.PayoutID,
		&p.AmountMinor, &p.RoundingMinor, &p.Method, &p.Reverses, &p.RefundsID, &p.Reversed)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrNotFound
	}
	if err != nil {
		return p, fmt.Errorf("read payment: %w", err)
	}
	switch {
	case p.InvoiceID != "":
		if !scope.Role.SeesCustomerPersonalData() {
			return p, access.ErrForbidden
		}
		_, err = balanceTx(ctx, tx, p.InvoiceID, true)
	case p.DepositID != "":
		var d DepositBalance
		if d, err = depositTx(ctx, tx, p.DepositID, true); err == nil {
			err = mayReadHolding(scope.Role, "deposit", d.Purpose)
		}
	default:
		var purpose string
		if err = tx.QueryRow(ctx, `SELECT purpose FROM payouts WHERE id = $1`, p.PayoutID).Scan(&purpose); err == nil {
			err = mayReadHolding(scope.Role, "payout", purpose)
		}
	}
	return p, err
}

// StatementLine is one line of the bank's statement, typed or imported.
type StatementLine struct {
	ID          string
	BookedOn    time.Time
	AmountMinor int64
	Reference   string
	ExternalID  string
}

// AddStatementLines records lines of the bank's statement, for matching.
// A line with the bank's own id is recorded once, however often it is
// imported; returns how many were new. Whoever runs the shop, who answers
// for the books.
func AddStatementLines(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, source string, lines []StatementLine) (int, error) {
	if !scope.Role.RunsTheShop() {
		return 0, access.ErrForbidden
	}
	if source != "typed" && source != "imported" {
		return 0, fmt.Errorf("%w: a statement line is typed or imported", ErrInvalid)
	}
	for _, l := range lines {
		if l.BookedOn.IsZero() || l.AmountMinor == 0 || utf8.RuneCountInString(l.Reference) > 200 || len(l.ExternalID) > 200 {
			return 0, fmt.Errorf("%w: a statement line needs its day and an amount", ErrInvalid)
		}
	}
	added := 0
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		for _, l := range lines {
			tag, err := tx.Exec(ctx, `
				INSERT INTO bank_statement_lines (shop_id, booked_on, amount_minor, reference, source, external_id, created_by)
				VALUES ($1, $2::date, $3, nullif($4, ''), $5, nullif($6, ''), $7)
				ON CONFLICT (shop_id, external_id) WHERE external_id IS NOT NULL DO NOTHING`,
				scope.ShopID, l.BookedOn.Format("2006-01-02"), l.AmountMinor, strings.TrimSpace(l.Reference), source,
				strings.TrimSpace(l.ExternalID), scope.UserID)
			if err != nil {
				return fmt.Errorf("record statement line: %w", err)
			}
			added += int(tag.RowsAffected())
		}
		return nil
	})
	return added, err
}

// MatchPayment says a payment is a line of the bank's statement: the same
// money, the same way. Each is matched once; a wrong match is undone, not
// deleted.
func MatchPayment(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, paymentID, lineID string) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(paymentID) || !looksLikeUUID(lineID) {
		return ErrNotFound
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var paid, line int64
		var method string
		if err := tx.QueryRow(ctx, `SELECT amount_minor, method FROM invoice_payments WHERE id = $1`, paymentID).Scan(&paid, &method); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("read payment: %w", err)
		}
		if err := tx.QueryRow(ctx, `SELECT amount_minor FROM bank_statement_lines WHERE id = $1`, lineID).Scan(&line); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("read statement line: %w", err)
		}
		switch {
		case method == "cash" || method == "deposit":
			return fmt.Errorf("%w: cash and a deposit applied never reach the bank", ErrInvalid)
		case paid != line:
			return fmt.Errorf("%w: the line is for a different amount", ErrInvalid)
		}
		_, err := tx.Exec(ctx, `INSERT INTO payment_matches (shop_id, payment_id, statement_line_id, matched_by) VALUES ($1, $2, $3, $4)`,
			scope.ShopID, paymentID, lineID, scope.UserID)
		if isUniqueViolation(err, "payment_matches_payment_idx") || isUniqueViolation(err, "payment_matches_line_idx") {
			return fmt.Errorf("%w: that payment or that line is already matched", ErrInvalid)
		}
		return err
	})
}

// UndoMatch undoes a payment's match, which stays recorded as undone.
func UndoMatch(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, paymentID string) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(paymentID) {
		return ErrNotFound
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE payment_matches SET undone_at = clock_timestamp(), undone_by = $2 WHERE payment_id = $1 AND undone_at IS NULL`,
			paymentID, scope.UserID)
		if err == nil && tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return err
	})
}

// Reconciliation is what does not match yet: money recorded as through the
// bank, Swish or the terminal with no line on the statement, and lines on
// the statement with no payment.
type Reconciliation struct {
	Payments []Payment
	Lines    []StatementLine
}

// Unreconciled reads what is not matched, for whoever runs the shop.
func Unreconciled(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) (Reconciliation, error) {
	if !scope.Role.RunsTheShop() {
		return Reconciliation{}, access.ErrForbidden
	}
	var r Reconciliation
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT p.id, p.series, p.number, p.amount_minor, p.method, p.paid_on, coalesce(p.reference, '')
			FROM invoice_payments p
			WHERE p.method NOT IN ('cash', 'deposit')
			  AND NOT EXISTS (SELECT 1 FROM payment_matches m WHERE m.payment_id = p.id AND m.undone_at IS NULL)
			ORDER BY p.paid_on, p.number`)
		if err != nil {
			return fmt.Errorf("read unmatched payments: %w", err)
		}
		for rows.Next() {
			var p Payment
			if err := rows.Scan(&p.ID, &p.Series, &p.Number, &p.AmountMinor, &p.Method, &p.PaidOn, &p.Reference); err != nil {
				rows.Close()
				return err
			}
			r.Payments = append(r.Payments, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `
			SELECT l.id, l.booked_on, l.amount_minor, coalesce(l.reference, ''), coalesce(l.external_id, '')
			FROM bank_statement_lines l
			WHERE NOT EXISTS (SELECT 1 FROM payment_matches m WHERE m.statement_line_id = l.id AND m.undone_at IS NULL)
			ORDER BY l.booked_on, l.id`)
		if err != nil {
			return fmt.Errorf("read unmatched lines: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var l StatementLine
			if err := rows.Scan(&l.ID, &l.BookedOn, &l.AmountMinor, &l.Reference, &l.ExternalID); err != nil {
				return err
			}
			r.Lines = append(r.Lines, l)
		}
		return rows.Err()
	})
	return r, err
}

// unmatchedPaymentDays is how long money recorded as through the bank may
// go without a line on the statement before somebody is asked to look: the
// bank's own clearing takes days, and an item that is true for every
// payment on the day it is recorded would be noise.
const unmatchedPaymentDays = 5

// unmatchedPaymentsTx is the queue's item for payments with no line on the
// statement after unmatchedPaymentDays (#158): one item, a count, saying
// nothing of whose they are. It runs in the transaction of the money
// provider (uncheckedFlowsWaiting), for whoever runs the shop, because each
// provider of its own costs the home page a transaction.
func unmatchedPaymentsTx(ctx context.Context, tx pgx.Tx) ([]ActionItem, error) {
	var n int
	err := tx.QueryRow(ctx, `
		SELECT count(*) FROM invoice_payments p
		WHERE p.method NOT IN ('cash', 'deposit')
		  AND p.paid_on < current_date - $1::int
		  AND NOT EXISTS (SELECT 1 FROM payment_matches m WHERE m.payment_id = p.id AND m.undone_at IS NULL)`,
		unmatchedPaymentDays).Scan(&n)
	if err != nil || n == 0 {
		return nil, err
	}
	return []ActionItem{{Kind: "payments_unmatched", SubjectKind: "reconcile", Count: n, Urgency: UrgencySoon}}, nil
}

// Match is a payment matched to a line of the bank's statement, standing.
type Match struct {
	Payment Payment
	Line    StatementLine
	At      time.Time
}

// Matches reads the matches standing, newest first, so a wrong one can be
// undone: whoever runs the shop.
func Matches(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, limit int) ([]Match, error) {
	if !scope.Role.RunsTheShop() {
		return nil, access.ErrForbidden
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var out []Match
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT p.id, p.series, p.number, p.amount_minor, p.method, p.paid_on, coalesce(p.reference, ''),
			       l.id, l.booked_on, l.amount_minor, coalesce(l.reference, ''), coalesce(l.external_id, ''), m.matched_at
			FROM payment_matches m
			JOIN invoice_payments p ON p.id = m.payment_id
			JOIN bank_statement_lines l ON l.id = m.statement_line_id
			WHERE m.undone_at IS NULL
			ORDER BY m.matched_at DESC LIMIT $1`, limit)
		if err != nil {
			return fmt.Errorf("read matches: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var m Match
			if err := rows.Scan(&m.Payment.ID, &m.Payment.Series, &m.Payment.Number, &m.Payment.AmountMinor, &m.Payment.Method,
				&m.Payment.PaidOn, &m.Payment.Reference, &m.Line.ID, &m.Line.BookedOn, &m.Line.AmountMinor, &m.Line.Reference,
				&m.Line.ExternalID, &m.At); err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

// ParseStatementCSV reads a bank's statement exported as text: the day
// booked, the amount -- negative going out, öre after a comma or a point
// -- the reference and, where the bank gives one, its own id for the
// line, so importing the same file twice records each line once.
// Semicolons or commas between fields, as Swedish banks export either; a
// first line naming the columns is skipped. Every line is read before any
// is recorded.
func ParseStatementCSV(r io.Reader) ([]StatementLine, error) {
	raw, err := io.ReadAll(io.LimitReader(r, 2<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: the file could not be read", ErrInvalid)
	}
	text := strings.TrimPrefix(string(raw), "\ufeff")
	cr := csv.NewReader(strings.NewReader(text))
	if first, _, _ := strings.Cut(text, "\n"); strings.Count(first, ";") > strings.Count(first, ",") {
		cr.Comma = ';'
	}
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	records, err := cr.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("%w: the file is not separated text", ErrInvalid)
	}
	var out []StatementLine
	for i, rec := range records {
		if len(rec) == 0 || (len(rec) == 1 && strings.TrimSpace(rec[0]) == "") {
			continue
		}
		if len(rec) < 3 {
			return nil, fmt.Errorf("%w: line %d has %d fields, not 3 or 4", ErrInvalid, i+1, len(rec))
		}
		day, err := time.Parse("2006-01-02", strings.TrimSpace(rec[0]))
		if err != nil {
			if i == 0 {
				continue // the header
			}
			return nil, fmt.Errorf("%w: line %d: the day does not read", ErrInvalid, i+1)
		}
		v := strings.TrimSpace(rec[1])
		sign := int64(1)
		if strings.HasPrefix(v, "-") || strings.HasPrefix(v, "−") {
			sign, v = -1, strings.TrimLeft(v, "-−")
		}
		amount, err := parseKronor(v)
		if err != nil || amount == 0 {
			return nil, fmt.Errorf("%w: line %d: the amount does not read", ErrInvalid, i+1)
		}
		l := StatementLine{BookedOn: day, AmountMinor: sign * amount, Reference: strings.TrimSpace(rec[2])}
		if len(rec) > 3 {
			l.ExternalID = strings.TrimSpace(rec[3])
		}
		out = append(out, l)
	}
	return out, nil
}
