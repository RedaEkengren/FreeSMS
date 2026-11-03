package workshop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/money"
)

// Trade-ins (#165). A trade-in is part of a deal's terms, frozen in the
// contract version like everything agreed, and once handed over a purchase
// of its own (#161) with its own checks and eligibility. The car sold keeps
// its full price for its own VAT; the credit settles what is owed. It is
// held as a deposit for the buyer from when the trade-in is received and
// its checks have passed, and applied to the sale's invoice as a payment
// already received -- whichever comes first, the invoice or the car.

// TradeIn is a car the buyer trades in, as the terms state it.
type TradeIn struct {
	Registration string
	VIN          string
	Make, Model  string
	OdometerKm   *int64
	// What it is credited at, and of that what settles the buyer's loan
	// on it, paid to the lender.
	CreditMinor int64
	LoanMinor   int64
	Lender      string
	SellerType  string
	Note        string
	// Filled in when read: the key it is received by, and whether it has
	// been, as which purchase.
	Key        string
	PurchaseID string
	Credited   bool
}

// Car is the trade-in as a person names it.
func (t TradeIn) Car() string {
	name := strings.TrimSpace(t.Registration)
	if name == "" {
		name = t.VIN
	}
	if m := strings.TrimSpace(t.Make + " " + t.Model); m != "" {
		name += " (" + m + ")"
	}
	return name
}

// EquityMinor is what the trade-in credits the buyer once the loan on it
// is settled.
func (t TradeIn) EquityMinor() int64 { return t.CreditMinor - t.LoanMinor }

var (
	errTradeIn       = fmt.Errorf("%w: a trade-in is a car by its plate or chassis number, credited at up to a billion kronor, with what kind of seller the buyer is", ErrInvalid)
	errTradeInTwice  = fmt.Errorf("%w: the same car is traded in twice", ErrInvalid)
	errTradeInEquity = fmt.Errorf("%w: a loan larger than the trade-in's credit is not handled yet: the difference is a charge whose VAT the accountant has to settle first", ErrInvalid)
	errTradeInNone   = fmt.Errorf("%w: the deal's terms have no such trade-in", ErrInvalid)
	errTradeInDeal   = fmt.Errorf("%w: a trade-in is received once the deal is contracted", ErrInvalid)
)

// tradeInKey is the key a trade-in is found by across versions of the
// terms: its normalised plate, else its chassis number. A key asked for
// is read the same way, so "TRD 123" and "trd123" find the same car.
func tradeInKey(t TradeIn) string {
	if p := NormaliseRegistration(t.Registration); p != "" {
		return p
	}
	return strings.ToUpper(strings.TrimSpace(t.VIN))
}

// checkTradeIns checks and tidies the trade-ins of a set of terms.
func checkTradeIns(ts []TradeIn) error {
	if len(ts) > 4 {
		return errTradeIn
	}
	seen := map[string]bool{}
	for i := range ts {
		t := &ts[i]
		t.Registration, t.VIN = strings.TrimSpace(t.Registration), strings.ToUpper(strings.TrimSpace(t.VIN))
		t.Make, t.Model = strings.TrimSpace(t.Make), strings.TrimSpace(t.Model)
		t.Lender, t.Note = strings.TrimSpace(t.Lender), strings.TrimSpace(t.Note)
		t.Key = tradeInKey(*t)
		switch {
		case t.Key == "" || utf8.RuneCountInString(t.Registration) > 20 || utf8.RuneCountInString(t.VIN) > 40 ||
			utf8.RuneCountInString(t.Make) > 100 || utf8.RuneCountInString(t.Model) > 100 ||
			utf8.RuneCountInString(t.Lender) > 200 || utf8.RuneCountInString(t.Note) > 500:
			return errTradeIn
		case t.CreditMinor < 0 || t.CreditMinor > 100_000_000_000 || t.LoanMinor < 0 || !slices.Contains(SellerTypes, t.SellerType):
			return errTradeIn
		case t.OdometerKm != nil && (*t.OdometerKm < 0 || *t.OdometerKm > 9_999_999):
			return errTradeIn
		case t.LoanMinor > t.CreditMinor:
			return errTradeInEquity
		case seen[t.Key]:
			return errTradeInTwice
		}
		seen[t.Key] = true
	}
	return nil
}

// insertTradeInsTx writes a version of the terms' trade-ins.
func insertTradeInsTx(ctx context.Context, tx pgx.Tx, scope access.Scope, terms string, ts []TradeIn) error {
	for i, t := range ts {
		if _, err := tx.Exec(ctx, `
			INSERT INTO vehicle_sale_trade_ins (shop_id, terms_id, position, registration, vin, make, model, key, odometer_km,
			                                    credit_minor, loan_minor, lender, seller_type, note)
			VALUES ($1, $2, $3, nullif($4, ''), nullif($5, ''), nullif($6, ''), nullif($7, ''), $8, $9, $10, $11, nullif($12, ''), $13, nullif($14, ''))`,
			scope.ShopID, terms, i+1, t.Registration, t.VIN, t.Make, t.Model, t.Key, t.OdometerKm, t.CreditMinor, t.LoanMinor,
			t.Lender, t.SellerType, t.Note); err != nil {
			return fmt.Errorf("record the trade-ins: %w", err)
		}
	}
	return nil
}

// tradeInsTx reads a deal's trade-ins as its terms in force state them,
// with whether each was received and credited.
func tradeInsTx(ctx context.Context, tx pgx.Tx, saleID, terms string) ([]TradeIn, error) {
	rows, err := tx.Query(ctx, `
		SELECT coalesce(t.registration, ''), coalesce(t.vin, ''), coalesce(t.make, ''), coalesce(t.model, ''), t.odometer_km,
		       t.credit_minor, t.loan_minor, coalesce(t.lender, ''), t.seller_type, coalesce(t.note, ''), t.key,
		       coalesce(r.purchase_id::text, ''),
		       EXISTS (SELECT 1 FROM deposits d WHERE d.subject_kind = 'sale' AND d.subject_id = $1 AND d.note = 'trade-in ' || t.key)
		FROM vehicle_sale_trade_ins t
		LEFT JOIN vehicle_sale_trade_in_receipts r ON r.sale_id = $1 AND r.key = t.key
		WHERE t.terms_id = $2 ORDER BY t.position`, saleID, terms)
	if err != nil {
		return nil, fmt.Errorf("read trade-ins: %w", err)
	}
	defer rows.Close()
	var out []TradeIn
	for rows.Next() {
		var t TradeIn
		if err := rows.Scan(&t.Registration, &t.VIN, &t.Make, &t.Model, &t.OdometerKm, &t.CreditMinor, &t.LoanMinor, &t.Lender,
			&t.SellerType, &t.Note, &t.Key, &t.PurchaseID, &t.Credited); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// tradeInSettlement is how the contract says what is to pay with
// trade-ins: the amount, and what it is made of. Without trade-ins it is
// the total as it always was.
func tradeInSettlement(total int64, ts []TradeIn, sv bool) string {
	if len(ts) == 0 {
		return money.Format(total)
	}
	due := total
	var parts []string
	for _, t := range ts {
		due -= t.EquityMinor()
		if sv {
			p := fmt.Sprintf("inbyte av %s krediterat %s", t.Car(), money.Format(t.CreditMinor))
			if t.LoanMinor > 0 {
				p += fmt.Sprintf(", varav %s löser lånet hos %s", money.Format(t.LoanMinor), dashIfEmpty(t.Lender))
			}
			parts = append(parts, p)
		} else {
			p := fmt.Sprintf("trade-in of %s credited at %s", t.Car(), money.Format(t.CreditMinor))
			if t.LoanMinor > 0 {
				p += fmt.Sprintf(", of which %s settles the loan with %s", money.Format(t.LoanMinor), dashIfEmpty(t.Lender))
			}
			parts = append(parts, p)
		}
	}
	if sv {
		return fmt.Sprintf("%s (bil och tillbehör %s, avgår %s)", money.Format(due), money.Format(total), strings.Join(parts, "; "))
	}
	return fmt.Sprintf("%s (car and extras %s, less %s)", money.Format(due), money.Format(total), strings.Join(parts, "; "))
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ReceiveTradeIn records a contracted deal's trade-in handed over: a
// purchase of its own (#161) from the buyer, at the credit agreed, opening
// its cycle in stock. Once its checks before money leaves have passed
// (#162), its equity -- the credit, less the loan settled with the lender
// -- is held for the buyer as a deposit and applied to the sale's invoice
// if there is one. For whoever buys cars. Repeated, it records nothing
// more. Reports whether the trade-in was credited.
func ReceiveTradeIn(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, saleID, key string, receivedOn time.Time) (bool, error) {
	if !scope.Role.BuysVehicles() {
		return false, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return false, err
	}
	if !looksLikeUUID(saleID) {
		return false, ErrNotFound
	}
	key = tradeInKey(TradeIn{Registration: key})
	// What the terms say: a buyer reads every deal (dealSeer).
	var t TradeIn
	var buyer string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		d, err := dealTx(ctx, tx, scope, saleID)
		if err != nil {
			return err
		}
		if d.State != DealContracted {
			return errTradeInDeal
		}
		ts, err := tradeInsTx(ctx, tx, saleID, d.TermsID)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(ts, func(x TradeIn) bool { return x.Key == key })
		if i < 0 {
			return errTradeInNone
		}
		t, buyer = ts[i], d.Buyers[0].PartyID
		// Handing their car over, the buyer is its seller: from now on a
		// seller as well, as a purchase makes anyone it buys from.
		if _, err := tx.Exec(ctx, `
			INSERT INTO party_roles (shop_id, party_id, role, started_by)
			SELECT $1, party_now($2), 'seller', NULLIF($3, '')::uuid
			WHERE NOT EXISTS (SELECT 1 FROM party_roles WHERE party_id = party_now($2) AND role = 'seller' AND valid_to IS NULL)`,
			scope.ShopID, buyer, scope.UserID); err != nil {
			return fmt.Errorf("make the buyer the trade-in's seller: %w", err)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	vehicle, err := CarForPurchase(ctx, pool, scope, CarToBuy{Registration: t.Registration, VIN: t.VIN, Make: t.Make, Model: t.Model})
	if err != nil {
		return false, err
	}
	p, err := RecordPurchase(ctx, pool, scope, NewPurchase{VehicleID: vehicle, Reason: StockFromTradeIn, SellerPartyID: buyer,
		PurchasedOn: receivedOn, OdometerKm: t.OdometerKm, PriceMinor: t.CreditMinor, SellerType: t.SellerType,
		Note: t.Note, Key: "trade-in:" + saleID + ":" + key})
	if err != nil {
		return false, err
	}
	var credited bool
	err = database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO vehicle_sale_trade_in_receipts (shop_id, sale_id, key, purchase_id, received_by)
			VALUES ($1, $2, $3, $4, NULLIF($5, '')::uuid) ON CONFLICT (sale_id, key) DO NOTHING`,
			scope.ShopID, saleID, key, p.ID, scope.UserID); err != nil {
			return fmt.Errorf("record the trade-in received: %w", err)
		}
		var err error
		credited, err = creditTradeInTx(ctx, tx, scope, saleID, buyer, t, p.ID)
		return err
	})
	return credited, err
}

// CreditTradeIn credits a received trade-in whose checks have since
// passed: what ReceiveTradeIn does once they have. For whoever buys cars.
func CreditTradeIn(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, saleID, key string) (bool, error) {
	if !scope.Role.BuysVehicles() {
		return false, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return false, err
	}
	if !looksLikeUUID(saleID) {
		return false, ErrNotFound
	}
	key = tradeInKey(TradeIn{Registration: key})
	var credited bool
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		d, err := dealTx(ctx, tx, scope, saleID)
		if err != nil {
			return err
		}
		ts, err := tradeInsTx(ctx, tx, saleID, d.TermsID)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(ts, func(x TradeIn) bool { return x.Key == key })
		if i < 0 || ts[i].PurchaseID == "" {
			return errTradeInNone
		}
		credited, err = creditTradeInTx(ctx, tx, scope, saleID, d.Buyers[0].PartyID, ts[i], ts[i].PurchaseID)
		return err
	})
	return credited, err
}

// creditTradeInTx holds a received trade-in's equity for the buyer as a
// deposit, once, and applies it to the sale's invoice if one stands --
// only once the purchase's checks have passed; before that it reports
// false and credits nothing.
func creditTradeInTx(ctx context.Context, tx pgx.Tx, scope access.Scope, saleID, buyer string, t TradeIn, purchaseID string) (bool, error) {
	if err := purchaseChecksClearTx(ctx, tx, purchaseID); errors.Is(err, ErrChecksOutstanding) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	note := "trade-in " + t.Key
	var deposit string
	err := tx.QueryRow(ctx, `SELECT id FROM deposits WHERE subject_kind = 'sale' AND subject_id = $1 AND note = $2`, saleID, note).Scan(&deposit)
	if errors.Is(err, pgx.ErrNoRows) {
		if t.EquityMinor() == 0 {
			return true, nil
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO deposits (shop_id, party_id, purpose, subject_kind, subject_id, note, created_by)
			VALUES ($1, party_now($2), 'down_payment', 'sale', $3, $4, $5) RETURNING id`,
			scope.ShopID, buyer, saleID, note, scope.UserID).Scan(&deposit); err != nil {
			return false, fmt.Errorf("hold the trade-in's credit: %w", err)
		}
		in := Payment{DepositID: deposit, AmountMinor: t.EquityMinor(), Method: "other", PaidOn: time.Now(),
			Reference: "Trade-in " + t.Car(), Provider: "trade_in", ExternalID: purchaseID}
		if _, err := insertPaymentOnce(ctx, tx, scope, &in); err != nil {
			return false, err
		}
	} else if err != nil {
		return false, fmt.Errorf("read the trade-in's credit: %w", err)
	}
	var invoice string
	if err := tx.QueryRow(ctx, `SELECT coalesce(sale_invoice_standing($1)::text, '')`, saleID).Scan(&invoice); err != nil {
		return false, fmt.Errorf("read the sale's invoice: %w", err)
	}
	if invoice != "" {
		if err := applyHeldTx(ctx, tx, scope, deposit, invoice); err != nil {
			return false, err
		}
	}
	return true, nil
}

// applyHeldTx applies what a deposit holds to an invoice, up to what the
// invoice owes; nothing when either is nothing.
func applyHeldTx(ctx context.Context, tx pgx.Tx, scope access.Scope, deposit, invoice string) error {
	d, err := depositTx(ctx, tx, deposit, true)
	if err != nil {
		return err
	}
	b, err := balanceTx(ctx, tx, invoice, true)
	if err != nil {
		return err
	}
	if amount := min(d.Held(), b.OutstandingMinor()); amount > 0 {
		return applyDepositTx(ctx, tx, scope, deposit, invoice, amount, "")
	}
	return nil
}

// applySaleDepositsTx applies to a sale's invoice, as it is issued, every
// deposit held for the sale: its trade-ins' credits (#165) and its
// reservation's deposit (#174).
func applySaleDepositsTx(ctx context.Context, tx pgx.Tx, scope access.Scope, saleID, unitID, invoiceID string) error {
	rows, err := tx.Query(ctx, `SELECT id FROM deposits WHERE subject_kind = 'sale' AND subject_id = $1 ORDER BY created_at, id`, saleID)
	if err != nil {
		return fmt.Errorf("read the sale's deposits: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := applyHeldTx(ctx, tx, scope, id, invoiceID); err != nil {
			return err
		}
	}
	return applyReservationDepositTx(ctx, tx, scope, unitID, invoiceID)
}
