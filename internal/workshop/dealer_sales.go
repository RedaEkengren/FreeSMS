package workshop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/money"
)

// Deals (#175): a car in stock sold to one or more buyers. What was agreed
// is a version of the deal's terms; a change is a new version, and a new
// contract, and both stay. Known defects are rows, and a consumer accepts
// each one on its own (konsumentköplagen 2022:260, 4 kap. 6 §): a general
// "as is" clause does not exempt the dealer, and nothing here pretends it
// does. How the sale is taxed is chosen on the deal, never in the contract's
// text. A deal never carries what the car cost.

// The ways a buyer buys.
const (
	BuyerConsumer = "consumer"
	BuyerBusiness = "business"
)

// The ways a sale is taxed, chosen on the deal.
const (
	TaxMarginScheme = "margin_scheme"
	TaxOrdinary     = "ordinary"
)

// The states of a deal, from its events.
const (
	DealDraft      = "draft"
	DealContracted = "contracted"
	DealCancelled  = "cancelled"
)

// DefectAnswerHows are the ways a buyer answers a defect.
var DefectAnswerHows = []string{"paper", "in_person", "electronic"}

var dealLabels = map[string]string{
	BuyerConsumer: "A consumer", BuyerBusiness: "A business",
	TaxMarginScheme: "Margin scheme", TaxOrdinary: "Ordinary VAT",
	DealDraft: "Being agreed", DealContracted: "Contracted", DealCancelled: "Cancelled",
	"paper": "On paper, scanned", "in_person": "In person, here", "electronic": "Electronically",
}

// DealLabel is a buyer kind, tax treatment, state or way of answering in
// words, as a catalogue key.
func DealLabel(key string) string { return dealLabels[key] }

// SaleLine is one extra sold with the car, priced before VAT.
type SaleLine struct {
	Description    string
	QuantityMilli  int64
	UnitPriceMinor int64
	VATRateBasis   int
}

// NetMinor is the line's amount before VAT.
func (l SaleLine) NetMinor() int64 { return money.Scale(l.UnitPriceMinor, l.QuantityMilli, 1000) }

// GrossMinor is the line's amount with its VAT.
func (l SaleLine) GrossMinor() int64 { return l.NetMinor() + money.VAT(l.NetMinor(), l.VATRateBasis) }

// SaleTerms is what is agreed: who buys, for what, with what else, when and
// where it is handed over, and how the sale is taxed.
type SaleTerms struct {
	Buyers        []string
	BuyerKind     string
	PriceMinor    int64
	TaxTreatment  string
	Warranty      string
	DeliveryOn    *time.Time
	DeliveryPlace string
	OdometerKm    *int64
	PaymentTerms  string
	Note          string
	Lines         []SaleLine
}

// DealBuyer is a buyer as the deal names them.
type DealBuyer struct {
	PartyID string
	Name    string
}

// DefectAnswer is one buyer's standing answer to a defect.
type DefectAnswer struct {
	PartyID  string
	Accepted bool
	How      string
	At       time.Time
}

// Defect is a known defect on a deal, and each buyer's answer to it.
type Defect struct {
	ID          string
	Description string
	PhotoKey    string
	RecordedAt  time.Time
	Answers     []DefectAnswer
}

// AnswerOf is the buyer's standing answer, if any.
func (d Defect) AnswerOf(partyID string) *DefectAnswer {
	for i := range d.Answers {
		if d.Answers[i].PartyID == partyID {
			return &d.Answers[i]
		}
	}
	return nil
}

// Deal is a deal as whoever sells cars reads it: never the car's cost.
type Deal struct {
	ID              string
	Number          int64
	StockUnitID     string
	Car             string
	LeadID          string
	Salesperson     string
	SalespersonName string
	State           string
	CreatedAt       time.Time
	// The terms in force, and their version.
	TermsID string
	Version int
	Terms   SaleTerms
	Buyers  []DealBuyer
	Defects []Defect
	// The newest contract, which terms it was made from, and whether every
	// buyer it names has signed it.
	ContractID      string
	ContractTermsID string
	ContractSigned  bool
	// The terms the deal was last completed on, if it was, and the
	// contract it was completed with.
	ContractedTermsID    string
	ContractedDocumentID string
	// What the newest contract was made from (Basis, as it stood then).
	ContractBasis string
	// The car may be sold under the margin scheme (vat_eligibility).
	MarginSchemeEligible bool
	// The eligibility assessment the margin scheme relied on, when the
	// terms chose it (0205).
	EligibilityID string
}

// TotalMinor is what the buyer pays: the car and the extras, VAT included.
func (d Deal) TotalMinor() int64 {
	t := d.Terms.PriceMinor
	for _, l := range d.Terms.Lines {
		t += l.GrossMinor()
	}
	return t
}

// Basis is what a contract states, as the deal stands: its terms version
// and its known defects. A contract made from another basis is out of date
// -- the terms were revised, or a defect was found -- and a new one is made
// and signed, with the defect accepted, before the deal is complete again.
func (d Deal) Basis() string {
	ids := make([]string, len(d.Defects))
	for i, f := range d.Defects {
		ids[i] = f.ID
	}
	return d.TermsID + ":" + strings.Join(ids, ",")
}

// Completed reports whether the deal's contract was completed, and still
// states everything: the terms as they stand and every known defect.
func (d Deal) Completed() bool {
	return d.State == DealContracted && d.ContractedDocumentID == d.ContractID && d.ContractBasis == d.Basis()
}

// Missing is what stands between the deal and a completed contract, as
// catalogue keys: nothing once it may be completed.
func (d Deal) Missing() []string {
	var out []string
	if d.State == DealCancelled || d.Completed() {
		return nil
	}
	if d.Terms.TaxTreatment == "" {
		out = append(out, "Choose how the sale is taxed.")
	}
	if d.ContractID == "" || d.ContractBasis != d.Basis() {
		out = append(out, "Make the contract from the terms as they stand.")
	} else if !d.ContractSigned {
		out = append(out, "Every buyer signs the contract.")
	}
	if d.Terms.BuyerKind == BuyerConsumer {
		for _, f := range d.Defects {
			for _, b := range d.Buyers {
				if a := f.AnswerOf(b.PartyID); a == nil || !a.Accepted {
					out = append(out, "Every buyer accepts each known defect on its own.")
					return out
				}
			}
		}
	}
	return out
}

var (
	errDealBuyers     = fmt.Errorf("%w: a deal has one to four buyers, each named once", ErrInvalid)
	errDealKind       = fmt.Errorf("%w: say whether the buyer is a consumer or a business", ErrInvalid)
	errDealPrice      = fmt.Errorf("%w: a price is from nothing up to a billion kronor", ErrInvalid)
	errDealTax        = fmt.Errorf("%w: that is not a way a sale is taxed", ErrInvalid)
	errDealScheme     = fmt.Errorf("%w: this car may not be sold under the margin scheme: its purchase does not make it eligible", ErrInvalid)
	errDealText       = fmt.Errorf("%w: a warranty and a note are at most 1000 characters, a place 300 and payment terms 500", ErrInvalid)
	errDealOdometer   = fmt.Errorf("%w: an odometer reading is a whole number of kilometres, under ten million", ErrInvalid)
	errDealLine       = fmt.Errorf("%w: an extra has a description, a quantity, a price and a VAT rate", ErrInvalid)
	errDealCar        = fmt.Errorf("%w: that car is not in stock to be sold", ErrInvalid)
	errDealOpen       = fmt.Errorf("%w: the car already has a deal being agreed or contracted; cancel that first", ErrInvalid)
	errDealKey        = fmt.Errorf("%w: that request was already used for another deal", ErrInvalid)
	errDealMoved      = fmt.Errorf("%w: the deal was changed by somebody else meanwhile; read it again", ErrInvalid)
	errDealState      = fmt.Errorf("%w: the deal is not where that can be done", ErrInvalid)
	errDealDefect     = fmt.Errorf("%w: describe the defect in 1000 characters or fewer", ErrInvalid)
	errDealAnswer     = fmt.Errorf("%w: a defect is answered by one of the deal's buyers, on paper with the scan, in person, or electronically", ErrInvalid)
	errDealTemplate   = fmt.Errorf("%w: that is not a sales contract template", ErrInvalid)
	errDealComplete   = fmt.Errorf("%w: the contract is complete and states everything; change the terms or state a defect first", ErrInvalid)
	errDealIncomplete = fmt.Errorf("%w: the contract cannot be completed yet", ErrInvalid)
	// ErrDealNoReason is a deal cancelled without saying why.
	ErrDealNoReason = fmt.Errorf("%w: say why the deal is cancelled, in 300 characters or fewer", ErrInvalid)
)

// checkTerms is terms as they may be recorded.
func checkTerms(t *SaleTerms) error {
	if len(t.Buyers) == 0 || len(t.Buyers) > 4 {
		return errDealBuyers
	}
	seen := map[string]bool{}
	for _, b := range t.Buyers {
		if !looksLikeUUID(b) || seen[strings.ToLower(b)] {
			return errDealBuyers
		}
		seen[strings.ToLower(b)] = true
	}
	if t.BuyerKind != BuyerConsumer && t.BuyerKind != BuyerBusiness {
		return errDealKind
	}
	if t.PriceMinor < 0 || t.PriceMinor > 100_000_000_000 {
		return errDealPrice
	}
	if t.TaxTreatment != "" && t.TaxTreatment != TaxMarginScheme && t.TaxTreatment != TaxOrdinary {
		return errDealTax
	}
	t.Warranty, t.DeliveryPlace = strings.TrimSpace(t.Warranty), strings.TrimSpace(t.DeliveryPlace)
	t.PaymentTerms, t.Note = strings.TrimSpace(t.PaymentTerms), strings.TrimSpace(t.Note)
	if utf8.RuneCountInString(t.Warranty) > 1000 || utf8.RuneCountInString(t.Note) > 1000 ||
		utf8.RuneCountInString(t.DeliveryPlace) > 300 || utf8.RuneCountInString(t.PaymentTerms) > 500 {
		return errDealText
	}
	if t.OdometerKm != nil && (*t.OdometerKm < 0 || *t.OdometerKm > 9_999_999) {
		return errDealOdometer
	}
	if len(t.Lines) > 50 {
		return errDealLine
	}
	for i := range t.Lines {
		l := &t.Lines[i]
		l.Description = strings.TrimSpace(l.Description)
		if l.Description == "" || utf8.RuneCountInString(l.Description) > 300 || l.QuantityMilli < 1 || l.QuantityMilli > 1_000_000_000 ||
			l.UnitPriceMinor < 0 || l.UnitPriceMinor > 10_000_000_000 || l.VATRateBasis < 0 || l.VATRateBasis > 10000 {
			return errDealLine
		}
	}
	return nil
}

// eligibleForSchemeTx reports whether the unit's purchase makes the car
// eligible for the margin scheme. A unit with no purchase -- consigned,
// from the fleet -- is not.
func eligibleForSchemeTx(ctx context.Context, tx pgx.Tx, unitID string) (bool, error) {
	var purchase string
	err := tx.QueryRow(ctx, `SELECT id FROM vehicle_purchases WHERE stock_unit_id = $1`, unitID).Scan(&purchase)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the purchase: %w", err)
	}
	e, err := eligibilityTx(ctx, tx, purchase)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return e.Eligible, err
}

// insertTermsTx records a version of a deal's terms, its buyers and its
// lines, the buyers each a party the caller may see.
func insertTermsTx(ctx context.Context, tx pgx.Tx, scope access.Scope, saleID, unitID string, version int, t SaleTerms) error {
	if t.TaxTreatment == TaxMarginScheme {
		ok, err := eligibleForSchemeTx(ctx, tx, unitID)
		if err != nil {
			return err
		}
		if !ok {
			return errDealScheme
		}
	}
	for _, b := range t.Buyers {
		if err := partySeenTx(ctx, tx, scope, b); err != nil {
			return err
		}
	}
	var on any
	if t.DeliveryOn != nil {
		on = t.DeliveryOn.Format(time.DateOnly)
	}
	var terms string
	if err := tx.QueryRow(ctx, `
		INSERT INTO vehicle_sale_terms (shop_id, sale_id, version, buyer_kind, price_minor, sale_tax_treatment, warranty,
		                                delivery_on, delivery_place, odometer_km, payment_terms, note, created_by)
		VALUES ($1, $2, $3, $4, $5, nullif($6, ''), nullif($7, ''), $8::date, nullif($9, ''), $10, nullif($11, ''), nullif($12, ''),
		        NULLIF($13, '')::uuid)
		RETURNING id`,
		scope.ShopID, saleID, version, t.BuyerKind, t.PriceMinor, t.TaxTreatment, t.Warranty, on, t.DeliveryPlace, t.OdometerKm,
		t.PaymentTerms, t.Note, scope.UserID).Scan(&terms); err != nil {
		if isUniqueViolation(err, "vehicle_sale_terms_sale_id_version_key") {
			return errDealMoved
		}
		return fmt.Errorf("record the terms: %w", err)
	}
	for i, b := range t.Buyers {
		if _, err := tx.Exec(ctx, `INSERT INTO vehicle_sale_buyers (shop_id, terms_id, party_id, position) VALUES ($1, $2, party_now($3), $4)`,
			scope.ShopID, terms, b, i+1); err != nil {
			if isUniqueViolation(err, "vehicle_sale_buyers_terms_id_party_id_key") {
				return errDealBuyers
			}
			return fmt.Errorf("record the buyers: %w", err)
		}
	}
	for i, l := range t.Lines {
		if _, err := tx.Exec(ctx, `
			INSERT INTO vehicle_sale_lines (shop_id, terms_id, position, description, quantity_milli, unit_price_minor, vat_rate_basis)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`, scope.ShopID, terms, i+1, l.Description, l.QuantityMilli, l.UnitPriceMinor,
			l.VATRateBasis); err != nil {
			return fmt.Errorf("record the extras: %w", err)
		}
	}
	return nil
}

// lockDealTx takes a deal's lock: every change to it is one at a time.
func lockDealTx(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('sale:' || $1::uuid::text, 0))`, id)
	return err
}

// CreateDeal opens a deal on a car in stock with its first terms, for the
// buyers named, from a lead or not. A car has one deal being agreed or
// contracted at a time; the same request sent again finds the deal it
// made. The salesperson is whoever opens it. For whoever sells cars, in a
// shop that sells them.
// NewDeal is a deal to open: the car, the lead it came from if any, the
// first terms, and the request's own key. NewBuyer, given, is a buyer who
// is not a party yet -- a private person by name and phone or email --
// made in the deal's own transaction and named first, so a deal sent twice
// or refused makes no stray person.
type NewDeal struct {
	UnitID   string
	LeadID   string
	Terms    SaleTerms
	NewBuyer *NewParty
	Key      string
}

func CreateDeal(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, n NewDeal) (Deal, error) {
	unitID, leadID, t, key := n.UnitID, n.LeadID, n.Terms, n.Key
	var newBuyer *NewParty
	if n.NewBuyer != nil {
		nb := *n.NewBuyer
		nb.Kind, nb.Role, nb.Name = "private", PartyBuyer, strings.TrimSpace(nb.Name)
		if nb.Name == "" || utf8.RuneCountInString(nb.Name) > 200 || utf8.RuneCountInString(nb.Phone) > 40 ||
			utf8.RuneCountInString(nb.Email) > 200 {
			return Deal{}, errDealBuyers
		}
		newBuyer = &nb
		// A stand-in, checked as an id, replaced by the party once made.
		t.Buyers = append([]string{"00000000-0000-0000-0000-000000000000"}, t.Buyers...)
	}
	if !scope.Role.SellsVehicles() {
		return Deal{}, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return Deal{}, err
	}
	if !looksLikeUUID(unitID) || (leadID != "" && !looksLikeUUID(leadID)) {
		return Deal{}, ErrNotFound
	}
	if err := checkTerms(&t); err != nil {
		return Deal{}, err
	}
	key = strings.TrimSpace(key)
	if len(key) > 100 {
		return Deal{}, errDealKey
	}
	var out Deal
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		// The request first, so the same key sent at once for two cars is
		// looked up after the first has committed; then the car's deals one
		// at a time. Always in that order.
		if key != "" {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('salekey:' || $1, 0))`, key); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('saleunit:' || $1::uuid::text, 0))`, unitID); err != nil {
			return err
		}
		if key != "" {
			var id, unit string
			err := tx.QueryRow(ctx, `SELECT id, stock_unit_id FROM vehicle_sales WHERE request_key = $1`, key).Scan(&id, &unit)
			if err == nil {
				if unit != unitID {
					return errDealKey
				}
				out, err = dealTx(ctx, tx, scope, id)
				return err
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("read the request: %w", err)
			}
		}
		var status StockStatus
		var ended bool
		err := tx.QueryRow(ctx, `SELECT status, closed_at IS NOT NULL FROM stock_units WHERE id = $1`, unitID).Scan(&status, &ended)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read the car: %w", err)
		}
		if ended || status == StockSold || status == StockDelivered || status == StockInPreparation {
			return errDealCar
		}
		var open bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM vehicle_sales s WHERE s.stock_unit_id = $1
			               AND NOT EXISTS (SELECT 1 FROM vehicle_sale_events e WHERE e.sale_id = s.id AND e.kind = 'cancelled'))`,
			unitID).Scan(&open); err != nil {
			return fmt.Errorf("read the car's deals: %w", err)
		}
		if open {
			return errDealOpen
		}
		if leadID != "" {
			if _, err := leadTx(ctx, tx, scope, leadID); err != nil {
				return err
			}
		}
		if newBuyer != nil {
			if err := mayRecordPartyRole(scope.Role, scope, newBuyer.Role); err != nil {
				return err
			}
			if t.Buyers[0], err = createPartyTx(ctx, tx, scope, *newBuyer, ""); err != nil {
				return err
			}
		}
		var id string
		if err := tx.QueryRow(ctx, `
			INSERT INTO vehicle_sales (shop_id, stock_unit_id, lead_id, salesperson_id, request_key, created_by)
			VALUES ($1, $2, NULLIF($3, '')::uuid, $4, nullif($5, ''), NULLIF($6, '')::uuid) RETURNING id`,
			scope.ShopID, unitID, leadID, scope.UserID, key, scope.UserID).Scan(&id); err != nil {
			return fmt.Errorf("open the deal: %w", err)
		}
		if err := insertTermsTx(ctx, tx, scope, id, unitID, 1, t); err != nil {
			return err
		}
		if leadID != "" {
			var st string
			if err := tx.QueryRow(ctx, `SELECT coalesce(status, 'new') FROM lead_now WHERE lead_id = $1`, leadID).Scan(&st); err != nil {
				return err
			}
			if st != LeadOfferMade && st != LeadWon && st != LeadLost {
				if err := leadEventTx(ctx, tx, scope, leadID, "status", map[string]any{"status": LeadOfferMade}); err != nil {
					return err
				}
			}
		}
		out, err = dealTx(ctx, tx, scope, id)
		return err
	})
	return out, err
}

// ReviseDeal records the next version of a deal's terms, made from version
// fromVersion: the one the caller read, or the change is refused as made
// over somebody else's. Revised after the contract was signed, the deal
// needs a new contract made and signed from the new terms; the old one
// stays.
func ReviseDeal(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, saleID string, fromVersion int, t SaleTerms) (Deal, error) {
	if !scope.Role.SellsVehicles() {
		return Deal{}, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return Deal{}, err
	}
	if !looksLikeUUID(saleID) {
		return Deal{}, ErrNotFound
	}
	if err := checkTerms(&t); err != nil {
		return Deal{}, err
	}
	var out Deal
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockDealTx(ctx, tx, saleID); err != nil {
			return err
		}
		d, err := dealTx(ctx, tx, scope, saleID)
		if err != nil {
			return err
		}
		if d.State == DealCancelled {
			return errDealState
		}
		if d.Version != fromVersion {
			return errDealMoved
		}
		if err := insertTermsTx(ctx, tx, scope, saleID, d.StockUnitID, d.Version+1, t); err != nil {
			return err
		}
		out, err = dealTx(ctx, tx, scope, saleID)
		return err
	})
	return out, err
}

// AddDefect records a known defect on a deal, as the dealer states it to
// the buyer. A consumer then accepts it on its own (AnswerDefect) before
// the contract can be completed; one added after the contract was signed
// is answered again before the car is delivered (#179).
func AddDefect(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, saleID, description, photoKey string) (string, error) {
	if !scope.Role.SellsVehicles() {
		return "", access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return "", err
	}
	if !looksLikeUUID(saleID) {
		return "", ErrNotFound
	}
	description, photoKey = strings.TrimSpace(description), strings.TrimSpace(photoKey)
	if description == "" || utf8.RuneCountInString(description) > 1000 || len(photoKey) > 300 {
		return "", errDealDefect
	}
	var id string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockDealTx(ctx, tx, saleID); err != nil {
			return err
		}
		d, err := dealTx(ctx, tx, scope, saleID)
		if err != nil {
			return err
		}
		if d.State == DealCancelled {
			return errDealState
		}
		return tx.QueryRow(ctx, `INSERT INTO vehicle_sale_defects (shop_id, sale_id, description, photo_key, recorded_by)
			VALUES ($1, $2, $3, nullif($4, ''), NULLIF($5, '')::uuid) RETURNING id`,
			scope.ShopID, saleID, description, photoKey, scope.UserID).Scan(&id)
	})
	return id, err
}

// AnswerDefect records one buyer's answer to one defect: accepted or
// refused, how, and on paper the scan. Answering again is another row; the
// newest stands. A business buyer's answer is optional, and recorded.
func AnswerDefect(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, defectID, partyID string, accepted bool, how, evidenceKey string) error {
	if !scope.Role.SellsVehicles() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return err
	}
	if !looksLikeUUID(defectID) || !looksLikeUUID(partyID) {
		return ErrNotFound
	}
	evidenceKey = strings.TrimSpace(evidenceKey)
	if !slices.Contains(DefectAnswerHows, how) || (how == "paper" && evidenceKey == "") || len(evidenceKey) > 300 {
		return errDealAnswer
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var saleID string
		err := tx.QueryRow(ctx, `SELECT sale_id FROM vehicle_sale_defects WHERE id = $1`, defectID).Scan(&saleID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := lockDealTx(ctx, tx, saleID); err != nil {
			return err
		}
		d, err := dealTx(ctx, tx, scope, saleID)
		if err != nil {
			return err
		}
		if d.State == DealCancelled {
			return errDealState
		}
		buyer := false
		for _, b := range d.Buyers {
			buyer = buyer || b.PartyID == partyID
		}
		if !buyer {
			return errDealAnswer
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO vehicle_sale_defect_answers (shop_id, defect_id, party_id, accepted, how, evidence_key, recorded_by)
			VALUES ($1, $2, $3, $4, $5, nullif($6, ''), NULLIF($7, '')::uuid)`,
			scope.ShopID, defectID, partyID, accepted, how, evidenceKey, scope.UserID)
		return err
	})
}

// CompleteDeal completes the contract: the terms as they stand, with how
// the sale is taxed chosen, in a contract every buyer has signed, and for
// a consumer every known defect accepted by every buyer on its own. The
// car is sold from then (its stock unit, and its sale row), and the lead
// the deal came from is won. Completed again, nothing changes.
func CompleteDeal(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, saleID string) (Deal, error) {
	if !scope.Role.SellsVehicles() {
		return Deal{}, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return Deal{}, err
	}
	if !looksLikeUUID(saleID) {
		return Deal{}, ErrNotFound
	}
	var out Deal
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockDealTx(ctx, tx, saleID); err != nil {
			return err
		}
		d, err := dealTx(ctx, tx, scope, saleID)
		if err != nil {
			return err
		}
		if d.Completed() {
			out = d
			return nil
		}
		if d.State == DealCancelled {
			return errDealState
		}
		if len(d.Missing()) > 0 {
			return errDealIncomplete
		}
		if _, err := tx.Exec(ctx, `INSERT INTO vehicle_sale_events (shop_id, sale_id, kind, terms_id, document_id, by_user)
			VALUES ($1, $2, 'contracted', $3, $4, NULLIF($5, '')::uuid)`, scope.ShopID, saleID, d.TermsID, d.ContractID, scope.UserID); err != nil {
			return fmt.Errorf("record the contract: %w", err)
		}
		var status StockStatus
		if err := tx.QueryRow(ctx, `SELECT status FROM stock_units WHERE id = $1`, d.StockUnitID).Scan(&status); err != nil {
			return err
		}
		// Sold already, or delivered since -- a deal revised after the
		// car left, completed again -- the car stays where it is.
		if status != StockSold && status != StockDelivered {
			if err := moveStockUnitTx(ctx, tx, scope, d.StockUnitID, StockSold, ""); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE stock_units SET vehicle_sale_id = $2 WHERE id = $1`, d.StockUnitID, saleID); err != nil {
			return fmt.Errorf("tie the car to its sale: %w", err)
		}
		var leadWon bool
		if d.LeadID != "" {
			if err := tx.QueryRow(ctx, `SELECT coalesce(status, '') = 'won' FROM lead_now WHERE lead_id = $1`, d.LeadID).Scan(&leadWon); err != nil {
				return err
			}
		}
		if d.LeadID != "" && !leadWon {
			if err := leadEventTx(ctx, tx, scope, d.LeadID, "status", map[string]any{"status": LeadWon}); err != nil {
				return err
			}
		}
		out, err = dealTx(ctx, tx, scope, saleID)
		return err
	})
	return out, err
}

// CancelDeal calls a deal off before the car is delivered, with why. A
// contracted deal's sale falls through: the car is for sale again, and the
// signed contract stays.
func CancelDeal(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, saleID, reason string) error {
	if !scope.Role.SellsVehicles() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return err
	}
	if !looksLikeUUID(saleID) {
		return ErrNotFound
	}
	reason, err := checkReason(reason, ErrDealNoReason)
	if err != nil {
		return err
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockDealTx(ctx, tx, saleID); err != nil {
			return err
		}
		d, err := dealTx(ctx, tx, scope, saleID)
		if err != nil {
			return err
		}
		if d.State == DealCancelled {
			return nil
		}
		var status StockStatus
		if err := tx.QueryRow(ctx, `SELECT status FROM stock_units WHERE id = $1`, d.StockUnitID).Scan(&status); err != nil {
			return err
		}
		if status == StockDelivered {
			return errDealState
		}
		if _, err := tx.Exec(ctx, `INSERT INTO vehicle_sale_events (shop_id, sale_id, kind, reason, by_user)
			VALUES ($1, $2, 'cancelled', $3, NULLIF($4, '')::uuid)`, scope.ShopID, saleID, reason, scope.UserID); err != nil {
			return fmt.Errorf("cancel the deal: %w", err)
		}
		// The car no longer points at a sale that did not happen.
		if _, err := tx.Exec(ctx, `UPDATE stock_units SET vehicle_sale_id = NULL WHERE id = $1 AND vehicle_sale_id = $2`,
			d.StockUnitID, saleID); err != nil {
			return fmt.Errorf("let go of the car: %w", err)
		}
		if d.State == DealContracted && status == StockSold {
			return moveStockUnitTx(ctx, tx, scope, d.StockUnitID, StockForSale, reason)
		}
		return nil
	})
}

// DealByID reads a deal for whoever sells cars. Reading is Core's: a shop
// that stops selling cars keeps its deals.
func DealByID(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string) (Deal, error) {
	if !scope.Role.SellsVehicles() {
		return Deal{}, access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return Deal{}, ErrNotFound
	}
	var out Deal
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = dealTx(ctx, tx, scope, id)
		return err
	})
	return out, err
}

// Deals lists the deals the caller may see, newest first, as a list shows
// them -- the car, where it stands, the buyers, the price and extras, not
// the defects or the contract; cancelled ones only with all. For whoever
// sells cars. Reading is Core's.
func Deals(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, all bool) ([]Deal, error) {
	if !scope.Role.SellsVehicles() {
		return nil, access.ErrForbidden
	}
	var out []Deal
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		// One statement for the list, however long: what a list shows of
		// each deal -- its car, where it stands, its buyers, its price and
		// extras -- and nothing it does not.
		rows, err := tx.Query(ctx, `
			SELECT s.id, s.number, s.stock_unit_id, `+carNameSQL+`, coalesce(s.lead_id::text, ''), s.salesperson_id,
			       coalesce(sp.display_name, ''), s.created_at,
			       coalesce((SELECT e.kind FROM vehicle_sale_events e WHERE e.sale_id = s.id ORDER BY e.seq DESC LIMIT 1), 'draft'),
			       t.id, t.version, t.buyer_kind, t.price_minor, coalesce(t.sale_tax_treatment, ''),
			       ARRAY(SELECT b.party_id::text FROM vehicle_sale_buyers b WHERE b.terms_id = t.id ORDER BY b.position),
			       ARRAY(SELECT coalesce(CASE WHEN pa.kind = 'company' THEN coalesce(c.company_name, pa.company_name) END,
			                             cp.display_name, pp.display_name, '')
			             FROM vehicle_sale_buyers b JOIN parties pa ON pa.id = b.party_id
			             LEFT JOIN customers c ON c.id = pa.customer_id LEFT JOIN people cp ON cp.id = c.person_id
			             LEFT JOIN people pp ON pp.id = pa.person_id
			             WHERE b.terms_id = t.id ORDER BY b.position),
			       ARRAY(SELECT l.description FROM vehicle_sale_lines l WHERE l.terms_id = t.id ORDER BY l.position),
			       ARRAY(SELECT l.quantity_milli FROM vehicle_sale_lines l WHERE l.terms_id = t.id ORDER BY l.position),
			       ARRAY(SELECT l.unit_price_minor FROM vehicle_sale_lines l WHERE l.terms_id = t.id ORDER BY l.position),
			       ARRAY(SELECT l.vat_rate_basis FROM vehicle_sale_lines l WHERE l.terms_id = t.id ORDER BY l.position)
			FROM vehicle_sales s
			JOIN stock_units u ON u.id = s.stock_unit_id JOIN vehicles v ON v.id = u.vehicle_id
			LEFT JOIN vehicle_registrations reg ON reg.vehicle_id = v.id AND reg.valid_to IS NULL
			JOIN users su ON su.id = s.salesperson_id LEFT JOIN people sp ON sp.id = su.person_id
			JOIN LATERAL (SELECT * FROM vehicle_sale_terms t WHERE t.sale_id = s.id ORDER BY t.version DESC LIMIT 1) t ON true
			WHERE ($1 OR NOT EXISTS (SELECT 1 FROM vehicle_sale_events e WHERE e.sale_id = s.id AND e.kind = 'cancelled'))
			  AND ($2 = '' OR s.salesperson_id::text = $2)
			ORDER BY s.created_at DESC LIMIT 200`, all, dealSeer(scope))
		if err != nil {
			return fmt.Errorf("list deals: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var d Deal
			var names, descs []string
			var qtys, prices []int64
			var rates []int32
			if err := rows.Scan(&d.ID, &d.Number, &d.StockUnitID, &d.Car, &d.LeadID, &d.Salesperson, &d.SalespersonName, &d.CreatedAt,
				&d.State, &d.TermsID, &d.Version, &d.Terms.BuyerKind, &d.Terms.PriceMinor, &d.Terms.TaxTreatment, &d.Terms.Buyers,
				&names, &descs, &qtys, &prices, &rates); err != nil {
				return err
			}
			for i, p := range d.Terms.Buyers {
				d.Buyers = append(d.Buyers, DealBuyer{PartyID: p, Name: names[i]})
			}
			for i := range descs {
				d.Terms.Lines = append(d.Terms.Lines, SaleLine{Description: descs[i], QuantityMilli: qtys[i], UnitPriceMinor: prices[i],
					VATRateBasis: int(rates[i])})
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

// dealSeer is the salesperson whose deals the caller may see, or empty for
// all: a salesperson their own, as with leads; whoever runs the shop, and a
// buyer reading what a sale is taxed at, every deal.
func dealSeer(scope access.Scope) string {
	if scope.Role.RunsTheShop() || !scope.Role.SellsVehicles() {
		return ""
	}
	return scope.UserID
}

// dealTx reads a deal the caller may see, with its terms in force, buyers, extras, defects and
// newest contract.
func dealTx(ctx context.Context, tx pgx.Tx, scope access.Scope, id string) (Deal, error) {
	var d Deal
	var on *time.Time
	err := tx.QueryRow(ctx, `
		SELECT s.id, s.number, s.stock_unit_id,
		       `+carNameSQL+`,
		       coalesce(s.lead_id::text, ''), s.salesperson_id, coalesce(sp.display_name, ''), s.created_at,
		       coalesce((SELECT e.kind FROM vehicle_sale_events e WHERE e.sale_id = s.id ORDER BY e.seq DESC LIMIT 1), 'draft'),
		       t.id, t.version, t.buyer_kind, t.price_minor, coalesce(t.sale_tax_treatment, ''), coalesce(t.warranty, ''),
		       t.delivery_on, coalesce(t.delivery_place, ''), t.odometer_km, coalesce(t.payment_terms, ''), coalesce(t.note, ''),
		       coalesce(t.eligibility_id::text, '')
		FROM vehicle_sales s
		JOIN stock_units u ON u.id = s.stock_unit_id JOIN vehicles v ON v.id = u.vehicle_id
		LEFT JOIN vehicle_registrations reg ON reg.vehicle_id = v.id AND reg.valid_to IS NULL
		JOIN users su ON su.id = s.salesperson_id LEFT JOIN people sp ON sp.id = su.person_id
		JOIN LATERAL (SELECT * FROM vehicle_sale_terms t WHERE t.sale_id = s.id ORDER BY t.version DESC LIMIT 1) t ON true
		WHERE s.id = $1`, id).Scan(&d.ID, &d.Number, &d.StockUnitID, &d.Car, &d.LeadID, &d.Salesperson, &d.SalespersonName,
		&d.CreatedAt, &d.State, &d.TermsID, &d.Version, &d.Terms.BuyerKind, &d.Terms.PriceMinor, &d.Terms.TaxTreatment,
		&d.Terms.Warranty, &on, &d.Terms.DeliveryPlace, &d.Terms.OdometerKm, &d.Terms.PaymentTerms, &d.Terms.Note, &d.EligibilityID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Deal{}, ErrNotFound
	}
	if err != nil {
		return Deal{}, fmt.Errorf("read deal: %w", err)
	}
	if seer := dealSeer(scope); seer != "" && d.Salesperson != seer {
		return Deal{}, ErrNotFound
	}
	d.Terms.DeliveryOn = on
	rows, err := tx.Query(ctx, `
		SELECT b.party_id,
		       coalesce(CASE WHEN pa.kind = 'company' THEN coalesce(c.company_name, pa.company_name) END, cp.display_name, pp.display_name, '')
		FROM vehicle_sale_buyers b JOIN parties pa ON pa.id = b.party_id
		LEFT JOIN customers c ON c.id = pa.customer_id LEFT JOIN people cp ON cp.id = c.person_id LEFT JOIN people pp ON pp.id = pa.person_id
		WHERE b.terms_id = $1 ORDER BY b.position`, d.TermsID)
	if err != nil {
		return Deal{}, err
	}
	for rows.Next() {
		var b DealBuyer
		if err := rows.Scan(&b.PartyID, &b.Name); err != nil {
			rows.Close()
			return Deal{}, err
		}
		d.Buyers = append(d.Buyers, b)
		d.Terms.Buyers = append(d.Terms.Buyers, b.PartyID)
	}
	rows.Close()
	rows, err = tx.Query(ctx, `SELECT description, quantity_milli, unit_price_minor, vat_rate_basis FROM vehicle_sale_lines
		WHERE terms_id = $1 ORDER BY position`, d.TermsID)
	if err != nil {
		return Deal{}, err
	}
	for rows.Next() {
		var l SaleLine
		if err := rows.Scan(&l.Description, &l.QuantityMilli, &l.UnitPriceMinor, &l.VATRateBasis); err != nil {
			rows.Close()
			return Deal{}, err
		}
		d.Terms.Lines = append(d.Terms.Lines, l)
	}
	rows.Close()
	rows, err = tx.Query(ctx, `SELECT id, description, coalesce(photo_key, ''), recorded_at FROM vehicle_sale_defects
		WHERE sale_id = $1 ORDER BY recorded_at, id`, id)
	if err != nil {
		return Deal{}, err
	}
	for rows.Next() {
		var f Defect
		if err := rows.Scan(&f.ID, &f.Description, &f.PhotoKey, &f.RecordedAt); err != nil {
			rows.Close()
			return Deal{}, err
		}
		d.Defects = append(d.Defects, f)
	}
	rows.Close()
	for i := range d.Defects {
		rows, err := tx.Query(ctx, `
			SELECT DISTINCT ON (party_id) party_id, accepted, how, at FROM vehicle_sale_defect_answers
			WHERE defect_id = $1 ORDER BY party_id, seq DESC`, d.Defects[i].ID)
		if err != nil {
			return Deal{}, err
		}
		for rows.Next() {
			var a DefectAnswer
			if err := rows.Scan(&a.PartyID, &a.Accepted, &a.How, &a.At); err != nil {
				rows.Close()
				return Deal{}, err
			}
			d.Defects[i].Answers = append(d.Defects[i].Answers, a)
		}
		rows.Close()
	}
	// The newest contract, which terms it was made from (its data says),
	// and whether every party it names has signed.
	err = tx.QueryRow(ctx, `
		SELECT r.id, coalesce(r.data->>'terms_id', ''),
		       -- A contract made before contracts said what they were made
		       -- from: its terms and the defects known when it was made.
		       coalesce(r.data->>'basis', (r.data->>'terms_id') || ':' ||
		                coalesce((SELECT string_agg(f.id::text, ',' ORDER BY f.recorded_at, f.id) FROM vehicle_sale_defects f
		                          WHERE f.sale_id = $1 AND f.recorded_at <= r.created_at), ''), ''),
		       NOT EXISTS (SELECT 1 FROM document_record_parties dp WHERE dp.document_id = r.id
		                   AND NOT EXISTS (SELECT 1 FROM document_signatures sg WHERE sg.document_party_id = dp.id))
		FROM document_records r
		WHERE r.kind = 'sales_contract' AND r.subject_kind = 'sale' AND r.subject_id = $1
		  AND NOT EXISTS (SELECT 1 FROM document_records n WHERE n.supersedes_id = r.id)
		ORDER BY r.created_at DESC LIMIT 1`, id).Scan(&d.ContractID, &d.ContractTermsID, &d.ContractBasis, &d.ContractSigned)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Deal{}, fmt.Errorf("read the contract: %w", err)
	}
	if err := tx.QueryRow(ctx, `
		SELECT coalesce(e.terms_id::text, ''), coalesce(e.document_id::text, '')
		FROM (SELECT 1) one LEFT JOIN LATERAL (SELECT terms_id, document_id FROM vehicle_sale_events
		                                       WHERE sale_id = $1 AND kind = 'contracted' ORDER BY seq DESC LIMIT 1) e ON true`,
		id).Scan(&d.ContractedTermsID, &d.ContractedDocumentID); err != nil {
		return Deal{}, err
	}
	if d.MarginSchemeEligible, err = eligibleForSchemeTx(ctx, tx, d.StockUnitID); err != nil {
		return Deal{}, err
	}
	return d, nil
}

// asIsPhrases are what reads as a general "as is" waiver in a contract;
// for a consumer one exempts the dealer from nothing (konsumentköplagen
// 4 kap. 6 §).
var asIsPhrases = []string{"befintligt skick", "som den är", "som den står", "som den står och går", "as is", "as seen",
	"in its current condition"}

// SalesContractWarnings is what a contract's text should not say to a
// consumer: a general "as is" clause presented as a waiver. Catalogue
// sentences, each with the phrase found; nothing for a business buyer.
func SalesContractWarnings(body, buyerKind string) []string {
	if buyerKind != BuyerConsumer {
		return nil
	}
	lower := strings.ToLower(body)
	isLetter := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }
	type found struct {
		phrase   string
		from, to int
	}
	var all []found
	for _, p := range asIsPhrases {
		for start := 0; ; {
			i := strings.Index(lower[start:], p)
			if i < 0 {
				break
			}
			from, to := start+i, start+i+len(p)
			start = from + 1
			// A whole phrase: no letter just before it or just after.
			if before, _ := utf8.DecodeLastRuneInString(lower[:from]); from > 0 && isLetter(before) {
				continue
			}
			if after, _ := utf8.DecodeRuneInString(lower[to:]); to < len(lower) && isLetter(after) {
				continue
			}
			all = append(all, found{p, from, to})
		}
	}
	// A phrase inside a longer one found at the same place is that one.
	var out []string
	for _, f := range all {
		inside := false
		for _, g := range all {
			if g != f && g.from <= f.from && f.to <= g.to && g.to-g.from > f.to-f.from {
				inside = true
			}
		}
		if !inside && !slices.Contains(out, f.phrase) {
			out = append(out, f.phrase)
		}
	}
	return out
}

// SalesContractTemplateName is the shipped contract's name.
const SalesContractTemplateName = "Sales contract (draft, needs legal review)"

// shippedSalesContracts are the shipped contract's text by language: it
// states everything promised, every known defect on its own, and for a
// consumer the dispute body. A draft until the legal review (#221) has
// read it.
var shippedSalesContracts = map[string]string{
	"en": `SALES CONTRACT {number}
Seller: {shop}, {shop_org}
Buyer: {buyers}

The seller sells and the buyer buys the car below.
Car: {car}, chassis number {vin}
Odometer at contract: {odometer}

Price of the car: {price}
Sold with it:
{lines}
To pay in all: {total}
Payment: {payment_terms}

Delivery: {delivery}
Warranty: {warranty}

KNOWN DEFECTS
The seller has told the buyer of these defects, and the buyer has accepted each of them on its own:
{defects}

The buyer's rights for a defect not stated here are as the law gives them. {dispute}

{note}`,
	"sv": `KÖPEAVTAL {number}
Säljare: {shop}, {shop_org}
Köpare: {buyers}

Säljaren säljer och köparen köper bilen nedan.
Bil: {car}, chassinummer {vin}
Mätarställning vid avtalet: {odometer}

Pris för bilen: {price}
Säljs tillsammans med bilen:
{lines}
Att betala totalt: {total}
Betalning: {payment_terms}

Leverans: {delivery}
Garanti: {warranty}

KÄNDA FEL
Säljaren har upplyst köparen om dessa fel, och köparen har godtagit vart och ett av dem för sig:
{defects}

Köparens rätt vid fel som inte anges här följer av lag. {dispute}

{note}`,
}

var shippedSalesContractNames = map[string]string{"sv": "Köpeavtal (utkast, behöver juridisk granskning)"}

// The dispute clause by buyer and language: a consumer may take a dispute
// to Allmänna reklamationsnämnden (ARN).
var disputeClauses = map[string]map[string]string{
	"en": {BuyerConsumer: "A consumer may take a dispute to the National Board for Consumer Disputes (Allmänna reklamationsnämnden, ARN), Box 174, 101 23 Stockholm, www.arn.se.",
		BuyerBusiness: "The Sale of Goods Act (köplagen) applies."},
	"sv": {BuyerConsumer: "En konsument kan vända sig till Allmänna reklamationsnämnden (ARN), Box 174, 101 23 Stockholm, www.arn.se, vid en tvist.",
		BuyerBusiness: "Köplagen gäller."},
}

// ShippedSalesContract is the shipped contract's text in a language.
func ShippedSalesContract(locale string) string { return shippedSalesContracts[locale] }

func ensureSalesContractTemplateTx(ctx context.Context, tx pgx.Tx, shopID string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('sales_templates:' || $1::text, 0))`, shopID); err != nil {
		return fmt.Errorf("lock templates: %w", err)
	}
	var some bool
	var locale string
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM document_templates WHERE kind = 'sales_contract'), (SELECT locale FROM shops)`).
		Scan(&some, &locale); err != nil || some {
		return err
	}
	text, ok := shippedSalesContracts[locale]
	if !ok {
		text = shippedSalesContracts["en"]
	}
	name := SalesContractTemplateName
	if n, ok := shippedSalesContractNames[locale]; ok {
		name = n
	}
	var id string
	if err := tx.QueryRow(ctx, `INSERT INTO document_templates (shop_id, kind, name) VALUES ($1, 'sales_contract', $2) RETURNING id`,
		shopID, name).Scan(&id); err != nil {
		return fmt.Errorf("make the shipped template: %w", err)
	}
	_, err := tx.Exec(ctx, `INSERT INTO document_template_versions (shop_id, template_id, version, body) VALUES ($1, $2, 1, $3)`, shopID, id, text)
	return err
}

// SalesContractTemplates lists the sales contract templates, making the
// shipped one the first time. For whoever sells cars.
func SalesContractTemplates(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]DocumentTemplate, error) {
	if !scope.Role.SellsVehicles() {
		return nil, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return nil, err
	}
	if err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		return ensureSalesContractTemplateTx(ctx, tx, scope.ShopID)
	}); err != nil {
		return nil, err
	}
	return DocumentTemplates(ctx, pool, scope, DocSalesContract)
}

// ProduceSalesContract freezes the contract for a deal's terms as they
// stand from a template version -- the shipped one when none is given -- with its buyers as parties, in one
// transaction with the deal locked. replacing is the contract the caller
// saw, or empty for none: made again, the new one supersedes it; when the
// deal's contract has moved on meanwhile, nothing is made
// (ErrMadeMeanwhile). It
// returns the "as is" phrases the text holds for a consumer, which the
// page warns of. The contract never names what the car cost, nor how the
// sale is taxed: that is the deal's (#176) and the invoice's (#177).
func ProduceSalesContract(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, saleID, templateVersionID, replacing string) (string, []string, error) {
	if !scope.Role.SellsVehicles() {
		return "", nil, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return "", nil, err
	}
	if err := mayDocument(scope.Role, scope, DocSalesContract, true); err != nil {
		return "", nil, err
	}
	if !looksLikeUUID(saleID) || (templateVersionID != "" && !looksLikeUUID(templateVersionID)) || (replacing != "" && !looksLikeUUID(replacing)) {
		return "", nil, ErrNotFound
	}
	var doc string
	var warnings []string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockDealTx(ctx, tx, saleID); err != nil {
			return err
		}
		d, err := dealTx(ctx, tx, scope, saleID)
		if err != nil {
			return err
		}
		if d.State == DealCancelled {
			return errDealState
		}
		if d.ContractID != replacing {
			return ErrMadeMeanwhile
		}
		if d.Completed() {
			return errDealComplete
		}
		// None chosen: the shipped contract, made here the first time --
		// in a write, never when a page is read.
		if templateVersionID == "" {
			if err := ensureSalesContractTemplateTx(ctx, tx, scope.ShopID); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `
				SELECT v.id FROM document_templates t JOIN document_template_versions v ON v.template_id = t.id
				WHERE t.kind = 'sales_contract' ORDER BY t.created_at, t.id, v.version DESC LIMIT 1`).Scan(&templateVersionID); err != nil {
				return fmt.Errorf("read the shipped contract: %w", err)
			}
		}
		var body, kind string
		if err := tx.QueryRow(ctx, `SELECT v.body, t.kind FROM document_template_versions v JOIN document_templates t ON t.id = v.template_id
			WHERE v.id = $1`, templateVersionID).Scan(&body, &kind); err != nil || kind != DocSalesContract {
			return errDealTemplate
		}
		warnings = SalesContractWarnings(body, d.Terms.BuyerKind)
		var shop, org, locale, vin string
		if err := tx.QueryRow(ctx, `SELECT name, coalesce(org_number, ''), locale FROM shops`).Scan(&shop, &org, &locale); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT coalesce(v.vin, '') FROM stock_units u JOIN vehicles v ON v.id = u.vehicle_id WHERE u.id = $1`,
			d.StockUnitID).Scan(&vin); err != nil {
			return err
		}
		clauses, ok := disputeClauses[locale]
		if !ok {
			clauses = disputeClauses["en"]
		}
		dash := func(s string) string {
			if strings.TrimSpace(s) == "" {
				return "-"
			}
			return s
		}
		names := make([]string, len(d.Buyers))
		parties := make([]DocumentPartyRole, len(d.Buyers))
		for i, b := range d.Buyers {
			names[i], parties[i] = b.Name, DocumentPartyRole{PartyID: b.PartyID, Role: "buyer"}
		}
		var lines, defects []string
		for _, l := range d.Terms.Lines {
			lines = append(lines, fmt.Sprintf("- %s, %s x %s: %s", l.Description, strconv.FormatFloat(float64(l.QuantityMilli)/1000, 'f', -1, 64),
				money.Format(l.UnitPriceMinor), money.Format(l.GrossMinor())))
		}
		for i, f := range d.Defects {
			defects = append(defects, fmt.Sprintf("%d. %s", i+1, f.Description))
		}
		odometer, delivery := "-", "-"
		if d.Terms.OdometerKm != nil {
			odometer = fmt.Sprintf("%d km", *d.Terms.OdometerKm)
		}
		if d.Terms.DeliveryOn != nil {
			delivery = strings.TrimSpace(d.Terms.DeliveryOn.Format(time.DateOnly) + " " + d.Terms.DeliveryPlace)
		} else if d.Terms.DeliveryPlace != "" {
			delivery = d.Terms.DeliveryPlace
		}
		doc, err = produceDocumentTx(ctx, tx, scope, NewDocumentRecord{Kind: DocSalesContract, SubjectKind: "sale", SubjectID: saleID,
			TemplateVersionID: templateVersionID, Supersedes: d.ContractID, Parties: parties,
			Title: fmt.Sprintf("%d %s", d.Number, d.Car),
			Data: map[string]string{"number": strconv.FormatInt(d.Number, 10), "shop": shop, "shop_org": dash(org),
				"buyers": strings.Join(names, ", "), "car": d.Car, "vin": dash(vin), "odometer": odometer,
				"price": money.Format(d.Terms.PriceMinor), "lines": dash(strings.Join(lines, "\n")), "total": money.Format(d.TotalMinor()),
				"payment_terms": dash(d.Terms.PaymentTerms), "delivery": delivery, "warranty": dash(d.Terms.Warranty),
				"defects": dash(strings.Join(defects, "\n")), "dispute": clauses[d.Terms.BuyerKind], "note": d.Terms.Note,
				"terms_id": d.TermsID, "basis": d.Basis()}})
		return err
	})
	return doc, warnings, err
}

// SaleTax is how a deal's car is taxed as its terms stand: under the
// margin scheme, the car's tax margin, the VAT in it and the base, against
// the tax purchase price the eligibility it relied on belongs to; under
// ordinary VAT, the VAT in its price and the price without it. The extras
// are taxed as ordinary lines on the invoice (#177). Never shown to the
// buyer: the margin is the dealer's.
type SaleTax struct {
	Treatment     string
	EligibilityID string
	MarginMinor   int64
	VATMinor      int64
	BaseMinor     int64
}

// saleVATRate is the rate a car is sold at in Sweden.
const saleVATRate = 2500

// SaleTaxOfDeal works out how the deal's car is taxed, one car alone
// (money.MarginScheme): for whoever may see what a car cost (#145), never a
// salesperson. Not chosen yet, it is nothing. Reading is Core's.
//
// A default pending the accountant (gate G2, #221): the rate is 25 per
// cent, and how the result is posted -- accounts, the VAT return's boxes --
// is not decided here.
func SaleTaxOfDeal(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, saleID string) (SaleTax, error) {
	if !scope.Role.SeesPurchasePrice() {
		return SaleTax{}, access.ErrForbidden
	}
	if !looksLikeUUID(saleID) {
		return SaleTax{}, ErrNotFound
	}
	var out SaleTax
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		d, err := dealTx(ctx, tx, scope, saleID)
		if err != nil {
			return err
		}
		out = SaleTax{Treatment: d.Terms.TaxTreatment, EligibilityID: d.EligibilityID}
		switch d.Terms.TaxTreatment {
		case TaxMarginScheme:
			// Terms written before the eligibility relied on was recorded
			// on them (0205) are read with the one that stands.
			if out.EligibilityID == "" {
				if err := tx.QueryRow(ctx, `
					SELECT e.id FROM vehicle_purchases p JOIN vat_eligibility e ON e.purchase_id = p.id
					WHERE p.stock_unit_id = $1 AND NOT EXISTS (SELECT 1 FROM vat_eligibility n WHERE n.supersedes_id = e.id)`,
					d.StockUnitID).Scan(&out.EligibilityID); err != nil {
					return fmt.Errorf("read the eligibility: %w", err)
				}
			}
			var purchase int64
			if err := tx.QueryRow(ctx, `SELECT p.tax_purchase_price_minor FROM vat_eligibility e JOIN vehicle_purchases p ON p.id = e.purchase_id
				WHERE e.id = $1`, out.EligibilityID).Scan(&purchase); err != nil {
				return fmt.Errorf("read the purchase price: %w", err)
			}
			r, err := money.MarginScheme(d.Terms.PriceMinor, purchase, saleVATRate)
			if err != nil {
				return fmt.Errorf("%w: %v", ErrInvalid, err)
			}
			out.MarginMinor, out.VATMinor, out.BaseMinor = r.MarginMinor, r.VATMinor, r.BaseMinor
		case TaxOrdinary:
			out.VATMinor = money.VATInGross(d.Terms.PriceMinor, saleVATRate)
			out.BaseMinor = d.Terms.PriceMinor - out.VATMinor
		}
		return nil
	})
	return out, err
}
