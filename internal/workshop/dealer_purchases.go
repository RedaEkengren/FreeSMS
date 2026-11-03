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
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/money"
)

// Purchases (#161): the dealer buying a car, recorded once -- from whom,
// what kind of seller they are, what was paid and on what evidence -- with
// whether the car may later be sold under the margin scheme (VMB) frozen
// beside it. Whether the scheme is used is the sale's decision (M8): it is
// optional, and ordinary VAT is always allowed.
//
// The tax purchase price is what the scheme reads, and nothing else is: it
// is everything paid to the seller except interest, the part paid to the
// seller's lender included. Reconditioning, transport and the like are the
// unit's costs (#164), kept apart.

// The kinds of seller a car can be bought from, as #161 lists them.
const (
	SellerPrivate             = "private"
	SellerCompanyNoDeduction  = "company_no_deduction"
	SellerCompanyChargedVAT   = "company_charged_vat"
	SellerSwedishDealerMargin = "se_dealer_margin"
	SellerEUDealerMargin      = "eu_dealer_margin"
	SellerEUReverseCharge     = "eu_reverse_charge"
	SellerNonEUImport         = "non_eu_import"
	SellerNewMeansOfTransport = "new_means_of_transport"
)

// SellerTypes is every kind of seller, in the order a form offers them.
var SellerTypes = []string{SellerPrivate, SellerCompanyNoDeduction, SellerCompanyChargedVAT, SellerSwedishDealerMargin,
	SellerEUDealerMargin, SellerEUReverseCharge, SellerNonEUImport, SellerNewMeansOfTransport}

var sellerTypeLabels = map[string]string{
	SellerPrivate:             "A private person",
	SellerCompanyNoDeduction:  "A company that could not deduct VAT on the car",
	SellerCompanyChargedVAT:   "A VAT-registered company that charged VAT",
	SellerSwedishDealerMargin: "A Swedish dealer that sold it under the margin scheme",
	SellerEUDealerMargin:      "A dealer in another EU country that sold it under the margin scheme",
	SellerEUReverseCharge:     "A purchase from another EU country under reverse charge",
	SellerNonEUImport:         "An import from outside the EU",
	SellerNewMeansOfTransport: "A new means of transport (at most 6 months or 6,000 km)",
}

// SellerTypeLabel is a kind of seller in words, a catalogue key.
func SellerTypeLabel(t string) string { return sellerTypeLabels[t] }

// Why a car is not eligible beyond its seller type, as catalogue keys: kept
// on the assessment, and shown with it.
const (
	EligibilityNoEvidence  = "Not eligible until the evidence is added"
	EligibilityEUDealer    = "Not eligible until the accountant has said what evidence a margin-scheme invoice from an EU dealer needs (#221)"
	EligibilityRepurchased = "Not eligible until the accountant has said how a car bought back after a rescinded sale is treated (#221)"
)

// EligibleFor is whether a car bought from a seller of sellerType, into a
// stock cycle opened for reason, with this many evidence documents, may be
// sold under the margin scheme -- and when it may not for a reason beyond
// the seller type, that reason. This is the one place the rule is written.
//
// A seller that charged VAT, a purchase under reverse charge, an import from
// outside the EU and a new means of transport are never eligible: VAT on
// them was or will be deductible. The others are, on evidence. Two cases
// wait for the accountant (#221) and are not eligible until then, which is
// the safe default: ordinary VAT is always allowed, the margin scheme
// wrongly used is not.
func EligibleFor(sellerType, reason string, evidence int) (bool, string) {
	switch sellerType {
	case SellerPrivate, SellerCompanyNoDeduction, SellerSwedishDealerMargin:
	case SellerEUDealerMargin:
		return false, EligibilityEUDealer
	default:
		return false, ""
	}
	switch {
	case reason == StockFromRepurchase:
		return false, EligibilityRepurchased
	case evidence == 0:
		return false, EligibilityNoEvidence
	}
	return true, ""
}

// NewPurchase is a car bought. Paid in kronor, PriceMinor is the tax
// purchase price; paid in another currency, it is worked out from what was
// paid in it and the rate, and PriceMinor is not given.
type NewPurchase struct {
	VehicleID     string
	Reason        string // purchase, trade_in or repurchase
	SellerPartyID string
	// A seller not yet known, made a party in the purchase's own
	// transaction, so a purchase refused leaves no person behind it. Used
	// instead of SellerPartyID.
	NewSeller   *NewParty
	PurchasedOn time.Time
	OdometerKm  *int64
	PriceMinor  int64
	// Another currency: its code, the amount paid in it in minor units,
	// kronor for one unit in millionths, and where the rate came from.
	Currency      string
	OriginalMinor int64
	RateMicro     int64
	RateSource    string
	SellerType    string
	// Documents (#150) that show what kind of seller it was.
	EvidenceDocumentIDs []string
	Note                string
	// The request's own key: replayed, it finds the purchase it made.
	Key string
}

// The most a car may cost, and the most a rate may be: the tax purchase
// price is worked out within int64 (money.Scale) with room to spare.
const (
	maxPurchaseMinor = 100_000_000_000
	maxOriginalMinor = 10_000_000_000
	maxRateMicro     = 100_000_000
)

var (
	errPurchaseReason   = fmt.Errorf("%w: a purchase opens a car's cycle as bought, traded in or bought back", ErrInvalid)
	errPurchaseDate     = fmt.Errorf("%w: say the day it was bought, and not a day still to come", ErrInvalid)
	errPurchasePrice    = fmt.Errorf("%w: say what was paid, from nothing up to a billion kronor", ErrInvalid)
	errPurchaseCurrency = fmt.Errorf("%w: paid in another currency, say which, how much in it, the rate and where the rate came from", ErrInvalid)
	errSellerType       = fmt.Errorf("%w: that is not a kind of seller", ErrInvalid)
	errOdometer         = fmt.Errorf("%w: an odometer reading is a whole number of kilometres, under ten million", ErrInvalid)
	errEvidence         = fmt.Errorf("%w: evidence is a document kept here, named once", ErrInvalid)
	errPurchaseNote     = fmt.Errorf("%w: a note is at most 1000 characters", ErrInvalid)
	errPurchaseKey      = fmt.Errorf("%w: that request was already used to record another purchase", ErrInvalid)
	errEligibilityMoved = fmt.Errorf("%w: the assessment was corrected by somebody else meanwhile; read it again", ErrInvalid)
	errEligibilityFixed = fmt.Errorf("%w: the car's sale has been invoiced, so its VAT eligibility can no longer be corrected", ErrInvalid)
	// ErrEligibilityNoReason is a correction that does not say why.
	ErrEligibilityNoReason = fmt.Errorf("%w: say why the assessment is corrected, in 300 characters or fewer", ErrInvalid)
)

// taxPurchasePrice is what n says was paid, in kronor.
func (n NewPurchase) taxPurchasePrice() (int64, error) {
	n.Currency = strings.ToUpper(strings.TrimSpace(n.Currency))
	if n.Currency == "" || n.Currency == "SEK" {
		if n.OriginalMinor != 0 || n.RateMicro != 0 || strings.TrimSpace(n.RateSource) != "" {
			return 0, errPurchaseCurrency
		}
		if n.PriceMinor < 0 || n.PriceMinor > maxPurchaseMinor {
			return 0, errPurchasePrice
		}
		return n.PriceMinor, nil
	}
	if len(n.Currency) != 3 || strings.Trim(n.Currency, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" ||
		n.OriginalMinor < 0 || n.OriginalMinor > maxOriginalMinor || n.RateMicro < 1 || n.RateMicro > maxRateMicro ||
		strings.TrimSpace(n.RateSource) == "" || utf8.RuneCountInString(n.RateSource) > 200 || n.PriceMinor != 0 {
		return 0, errPurchaseCurrency
	}
	return money.Scale(n.OriginalMinor, n.RateMicro, 1_000_000), nil
}

// checkEvidence is a list of document ids, each named once.
func checkEvidence(ids []string) error {
	seen := map[string]bool{}
	for _, id := range ids {
		if !looksLikeUUID(id) || seen[strings.ToLower(id)] {
			return errEvidence
		}
		seen[strings.ToLower(id)] = true
	}
	return nil
}

// evidenceTx refuses evidence that is not a document of this shop.
func evidenceTx(ctx context.Context, tx pgx.Tx, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM document_records WHERE id = ANY($1::uuid[])`, ids).Scan(&n); err != nil {
		return fmt.Errorf("read evidence: %w", err)
	}
	if n != len(ids) {
		return errEvidence
	}
	return nil
}

// RecordPurchase records a car bought and opens its cycle in stock, in one
// transaction: the purchase, the odometer reading taken then, the seller as
// a seller, and the first assessment of whether the car may be sold under
// the margin scheme. For whoever buys cars, in a shop that sells them.
// Replayed with the same key, it returns the purchase it made.
func RecordPurchase(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, n NewPurchase) (Purchase, error) {
	if !scope.Role.BuysVehicles() {
		return Purchase{}, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return Purchase{}, err
	}
	switch n.Reason {
	case StockFromPurchase, StockFromTradeIn, StockFromRepurchase:
	default:
		return Purchase{}, errPurchaseReason
	}
	var sellerNote string
	if n.NewSeller != nil {
		seller := *n.NewSeller
		seller.Role = PartySeller
		var err error
		if seller, sellerNote, err = checkNewParty(seller); err != nil {
			return Purchase{}, err
		}
		n.NewSeller, n.SellerPartyID = &seller, ""
	} else if !looksLikeUUID(n.SellerPartyID) {
		return Purchase{}, ErrNotFound
	}
	if !looksLikeUUID(n.VehicleID) {
		return Purchase{}, ErrNotFound
	}
	if n.PurchasedOn.IsZero() || n.PurchasedOn.After(time.Now().Add(24*time.Hour)) {
		return Purchase{}, errPurchaseDate
	}
	price, err := n.taxPurchasePrice()
	if err != nil {
		return Purchase{}, err
	}
	if !slices.Contains(SellerTypes, n.SellerType) {
		return Purchase{}, errSellerType
	}
	if n.OdometerKm != nil && (*n.OdometerKm < 0 || *n.OdometerKm >= 10_000_000) {
		return Purchase{}, errOdometer
	}
	if err := checkEvidence(n.EvidenceDocumentIDs); err != nil {
		return Purchase{}, err
	}
	n.Note = strings.TrimSpace(n.Note)
	if utf8.RuneCountInString(n.Note) > 1000 {
		return Purchase{}, errPurchaseNote
	}
	n.Key = strings.TrimSpace(n.Key)
	var out Purchase
	err = database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if n.NewSeller == nil {
			if err := partySeenTx(ctx, tx, scope, n.SellerPartyID); err != nil {
				return err
			}
		}
		unit, err := openStockUnitTx(ctx, tx, scope, n.VehicleID, n.Reason, n.Key)
		if err != nil {
			return err
		}
		// The same request again: the unit it opened has its purchase. A
		// seller made with it is not made twice.
		var existing string
		err = tx.QueryRow(ctx, `SELECT id FROM vehicle_purchases WHERE stock_unit_id = $1`, unit.ID).Scan(&existing)
		if err == nil {
			out, err = purchaseTx(ctx, tx, `p.id = $1`, existing)
			if err == nil && ((n.NewSeller == nil && out.SellerPartyID != n.SellerPartyID) || out.TaxPurchasePriceMinor != price) {
				return errPurchaseKey
			}
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read purchase: %w", err)
		}
		if n.NewSeller != nil {
			if n.SellerPartyID, err = createPartyTx(ctx, tx, scope, *n.NewSeller, sellerNote); err != nil {
				return err
			}
		}
		if err := evidenceTx(ctx, tx, n.EvidenceDocumentIDs); err != nil {
			return err
		}
		var reading *string
		if n.OdometerKm != nil {
			var id string
			if err := tx.QueryRow(ctx, `
				INSERT INTO odometer_readings (shop_id, vehicle_id, km, source, read_at, decreasing)
				VALUES ($1, $2, $3, 'purchase', ($4::date + time '12:00') AT TIME ZONE (SELECT timezone FROM shops WHERE id = $1),
				        EXISTS (SELECT 1 FROM odometer_readings WHERE vehicle_id = $2 AND km > $3))
				RETURNING id`, scope.ShopID, n.VehicleID, *n.OdometerKm, n.PurchasedOn.Format(time.DateOnly)).Scan(&id); err != nil {
				return fmt.Errorf("record the odometer: %w", err)
			}
			reading = &id
		}
		currency := strings.ToUpper(strings.TrimSpace(n.Currency))
		if currency == "SEK" {
			currency = ""
		}
		var id string
		if err := tx.QueryRow(ctx, `
			INSERT INTO vehicle_purchases (shop_id, stock_unit_id, seller_party_id, purchased_on, odometer_reading_id,
			                               tax_purchase_price_minor, currency, original_amount_minor, rate_micro, rate_source,
			                               note, created_by)
			VALUES ($1, $2, party_now($3), $4, $5, $6, nullif($7, ''), CASE WHEN $7 <> '' THEN $8::bigint END,
			        CASE WHEN $7 <> '' THEN $9::bigint END, nullif(btrim($10), ''), nullif($11, ''), NULLIF($12, '')::uuid)
			RETURNING id`,
			scope.ShopID, unit.ID, n.SellerPartyID, n.PurchasedOn.Format(time.DateOnly), reading, price,
			currency, n.OriginalMinor, n.RateMicro, n.RateSource, n.Note, scope.UserID).Scan(&id); err != nil {
			return fmt.Errorf("record the purchase: %w", err)
		}
		// Its ownership is to be filed: prepared now, so nothing is
		// forgotten (#180).
		if _, err := prepareFilingTx(ctx, tx, scope, "purchase", id, n.VehicleID, n.SellerPartyID, "", n.PurchasedOn); err != nil {
			return err
		}
		// The seller is a seller from now, if not one already.
		if _, err := tx.Exec(ctx, `
			INSERT INTO party_roles (shop_id, party_id, role, started_by)
			SELECT $1, party_now($2), 'seller', NULLIF($3, '')::uuid
			WHERE NOT EXISTS (SELECT 1 FROM party_roles WHERE party_id = party_now($2) AND role = 'seller' AND valid_to IS NULL)`,
			scope.ShopID, n.SellerPartyID, scope.UserID); err != nil {
			return fmt.Errorf("make the seller a seller: %w", err)
		}
		if err := assessTx(ctx, tx, scope, id, "", n.SellerType, n.Reason, n.EvidenceDocumentIDs, ""); err != nil {
			return err
		}
		// Whether the reading fits those before it is the system's to say,
		// as a check before money leaves (#162).
		if reading != nil {
			if err := odometerCheckTx(ctx, tx, scope, unit.ID, *reading); err != nil {
				return err
			}
		}
		if err := auditTx(ctx, tx, scope, id, "purchase.recorded", "", "", "",
			map[string]any{"stock_unit_id": unit.ID, "vehicle_id": n.VehicleID}); err != nil {
			return err
		}
		out, err = purchaseTx(ctx, tx, `p.id = $1`, id)
		return err
	})
	return out, err
}

// assessTx writes an assessment of a purchase's eligibility: the first, or
// one superseding the assessment before it.
func assessTx(ctx context.Context, tx pgx.Tx, scope access.Scope, purchaseID, supersedes, sellerType, reason string, evidence []string, why string) error {
	eligible, note := EligibleFor(sellerType, reason, len(evidence))
	if evidence == nil {
		evidence = []string{}
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO vat_eligibility (shop_id, purchase_id, seller_type, eligible, evidence_document_ids, note,
		                             supersedes_id, reason, recorded_by)
		VALUES ($1, $2, $3, $4, $5::uuid[], nullif($6, ''), NULLIF($7, '')::uuid, nullif($8, ''), NULLIF($9, '')::uuid)`,
		scope.ShopID, purchaseID, sellerType, eligible, evidence, note, supersedes, why, scope.UserID)
	var pg *pgconn.PgError
	switch {
	case isUniqueViolation(err, "vat_eligibility_supersedes_id_key"):
		return errEligibilityMoved
	case errors.As(err, &pg) && pg.Code == "23514" && strings.Contains(pg.Message, "invoiced"):
		return errEligibilityFixed
	case err != nil:
		return fmt.Errorf("record the VAT eligibility: %w", err)
	}
	return nil
}

// VATEligibility is a purchase's assessment in force: what kind of seller
// sold the car, whether it may be sold under the margin scheme, and on what
// evidence. It holds no price.
type VATEligibility struct {
	ID                  string
	PurchaseID          string
	SellerType          string
	Eligible            bool
	EvidenceDocumentIDs []string
	// Why not, beyond the seller type: a catalogue key, or "".
	Note string
	// Why it was corrected, when it was.
	Reason     string
	RecordedAt time.Time
	RecordedBy string
}

// SellerTypeLabel is the kind of seller in words, a catalogue key.
func (e VATEligibility) SellerTypeLabel() string { return SellerTypeLabel(e.SellerType) }

// Purchase is a car bought, as whoever may see what it cost reads it.
type Purchase struct {
	ID            string
	StockUnitID   string
	VehicleID     string
	SellerPartyID string
	PurchasedOn   time.Time
	OdometerKm    *int64
	// In kronor, minor units.
	TaxPurchasePriceMinor int64
	// Paid in another currency: which, how much in it, the rate (kronor
	// for one unit, in millionths) and where it came from. "" in kronor.
	Currency      string
	OriginalMinor int64
	RateMicro     int64
	RateSource    string
	Note          string
	CreatedAt     time.Time
	Eligibility   VATEligibility
}

const purchaseColumns = `
	SELECT p.id, p.stock_unit_id, u.vehicle_id, p.seller_party_id, p.purchased_on, o.km, p.tax_purchase_price_minor,
	       coalesce(p.currency, ''), coalesce(p.original_amount_minor, 0), coalesce(p.rate_micro, 0),
	       coalesce(p.rate_source, ''), coalesce(p.note, ''), p.created_at
	FROM vehicle_purchases p
	JOIN stock_units u ON u.id = p.stock_unit_id
	LEFT JOIN odometer_readings o ON o.id = p.odometer_reading_id`

func purchaseTx(ctx context.Context, tx pgx.Tx, where string, arg any) (Purchase, error) {
	var p Purchase
	err := tx.QueryRow(ctx, purchaseColumns+` WHERE `+where, arg).Scan(&p.ID, &p.StockUnitID, &p.VehicleID, &p.SellerPartyID,
		&p.PurchasedOn, &p.OdometerKm, &p.TaxPurchasePriceMinor, &p.Currency, &p.OriginalMinor, &p.RateMicro, &p.RateSource,
		&p.Note, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Purchase{}, ErrNotFound
	}
	if err != nil {
		return Purchase{}, fmt.Errorf("read purchase: %w", err)
	}
	p.Eligibility, err = eligibilityTx(ctx, tx, p.ID)
	return p, err
}

// eligibilityTx is the assessment in force: the one nothing supersedes.
func eligibilityTx(ctx context.Context, tx pgx.Tx, purchaseID string) (VATEligibility, error) {
	var e VATEligibility
	err := tx.QueryRow(ctx, `
		SELECT e.id, e.purchase_id, e.seller_type, e.eligible, e.evidence_document_ids::text[], coalesce(e.note, ''),
		       coalesce(e.reason, ''), e.recorded_at, coalesce(pe.display_name, '')
		FROM vat_eligibility e
		LEFT JOIN users us ON us.id = e.recorded_by LEFT JOIN people pe ON pe.id = us.person_id
		WHERE e.purchase_id = $1 AND NOT EXISTS (SELECT 1 FROM vat_eligibility n WHERE n.supersedes_id = e.id)`,
		purchaseID).Scan(&e.ID, &e.PurchaseID, &e.SellerType, &e.Eligible, &e.EvidenceDocumentIDs, &e.Note, &e.Reason,
		&e.RecordedAt, &e.RecordedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return VATEligibility{}, ErrNotFound
	}
	if err != nil {
		return VATEligibility{}, fmt.Errorf("read VAT eligibility: %w", err)
	}
	return e, nil
}

// PurchaseOfUnit reads the purchase that opened a stock unit, price and
// all, for whoever may see what a car cost (#145): never a salesperson,
// however they ask. Reading is Core's, so a shop that stops selling cars
// keeps its purchases readable. A unit with no purchase -- consigned, moved
// from the fleet, imported with no evidence -- has none: ErrNotFound.
func PurchaseOfUnit(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, unitID string) (Purchase, error) {
	if !scope.Role.SeesPurchasePrice() {
		return Purchase{}, access.ErrForbidden
	}
	if !looksLikeUUID(unitID) {
		return Purchase{}, ErrNotFound
	}
	var p Purchase
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		p, err = purchaseTx(ctx, tx, `p.stock_unit_id = $1`, unitID)
		return err
	})
	return p, err
}

// VATEligibilityOfUnit reads whether a stock unit's car may be sold under
// the margin scheme, for whoever buys or sells cars: the salesperson needs
// it to offer the scheme at the sale, and it carries no price. A unit with
// no purchase has no assessment, and so may not be: ErrNotFound. Reading is
// Core's.
func VATEligibilityOfUnit(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, unitID string) (VATEligibility, error) {
	if !scope.Role.BuysVehicles() && !scope.Role.SellsVehicles() {
		return VATEligibility{}, access.ErrForbidden
	}
	if !looksLikeUUID(unitID) {
		return VATEligibility{}, ErrNotFound
	}
	var e VATEligibility
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var purchase string
		err := tx.QueryRow(ctx, `SELECT id FROM vehicle_purchases WHERE stock_unit_id = $1`, unitID).Scan(&purchase)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read purchase: %w", err)
		}
		e, err = eligibilityTx(ctx, tx, purchase)
		return err
	})
	return e, err
}

// CorrectVATEligibility assesses a purchase again: the seller turned out to
// be a business, the evidence has come. The assessment before stays,
// superseded, and the correction says why and is an audit event. Refused
// once the car's sale has an invoice: the VAT on it was worked out from
// what was assessed then. For whoever buys cars, in a shop that sells them.
func CorrectVATEligibility(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, purchaseID, sellerType string, evidence []string, reason string) (VATEligibility, error) {
	if !scope.Role.BuysVehicles() {
		return VATEligibility{}, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return VATEligibility{}, err
	}
	if !looksLikeUUID(purchaseID) {
		return VATEligibility{}, ErrNotFound
	}
	if !slices.Contains(SellerTypes, sellerType) {
		return VATEligibility{}, errSellerType
	}
	if err := checkEvidence(evidence); err != nil {
		return VATEligibility{}, err
	}
	reason, err := checkReason(reason, ErrEligibilityNoReason)
	if err != nil {
		return VATEligibility{}, err
	}
	var out VATEligibility
	err = database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var cycle string
		err := tx.QueryRow(ctx, `
			SELECT u.reason FROM vehicle_purchases p JOIN stock_units u ON u.id = p.stock_unit_id WHERE p.id = $1`,
			purchaseID).Scan(&cycle)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read purchase: %w", err)
		}
		before, err := eligibilityTx(ctx, tx, purchaseID)
		if err != nil {
			return err
		}
		if err := evidenceTx(ctx, tx, evidence); err != nil {
			return err
		}
		if err := assessTx(ctx, tx, scope, purchaseID, before.ID, sellerType, cycle, evidence, reason); err != nil {
			return err
		}
		if out, err = eligibilityTx(ctx, tx, purchaseID); err != nil {
			return err
		}
		return auditTx(ctx, tx, scope, purchaseID, "vat_eligibility.corrected",
			eligibilityState(before), eligibilityState(out), reason,
			map[string]any{"from_seller_type": before.SellerType, "to_seller_type": out.SellerType})
	})
	return out, err
}

// eligibilityState is an assessment as an audit event's from or to.
func eligibilityState(e VATEligibility) string {
	if e.Eligible {
		return "eligible"
	}
	return "not_eligible"
}
