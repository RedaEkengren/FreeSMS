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

// Purchase calculations (#163): before bidding, the most the buyer may pay
// for a car, from what it should sell for, what it will cost to make ready
// and to keep, and the margin the dealer wants -- under ordinary VAT, and
// under the margin scheme when the seller would make the car eligible.
// Every calculation saved is a numbered version, never changed; the version
// a bid was made on is recorded. All of it is what a car costs, so it is
// for whoever may see that (#145).

// The kinds of cost a stock car has, here expected and in #164 spent.
const (
	CostReconditioning = "reconditioning"
	CostWorkshop       = "workshop"
	CostParts          = "parts"
	CostTransport      = "transport"
	CostInspection     = "inspection"
	CostRegistration   = "registration"
	CostAdvertising    = "advertising"
	CostOther          = "other"
)

// CostKinds is every kind of cost, in the order a form offers them.
var CostKinds = []string{CostReconditioning, CostWorkshop, CostParts, CostTransport, CostInspection, CostRegistration,
	CostAdvertising, CostOther}

var costKindLabels = map[string]string{
	CostReconditioning: "Reconditioning",
	CostWorkshop:       "Workshop work",
	CostParts:          "Parts",
	CostTransport:      "Transport",
	CostInspection:     "Inspection",
	CostRegistration:   "Registration and fees",
	CostAdvertising:    "Advertising",
	CostOther:          "Other",
}

// CostKindLabel is a kind of cost in words, a catalogue key.
func CostKindLabel(kind string) string { return costKindLabels[kind] }

// VAT on a car sold, in basis points.
const carVATBasis = 2500

// CalculationCost is one cost expected, net of VAT: the dealer deducts the
// VAT on what it buys to make a car ready.
type CalculationCost struct {
	Kind     string
	NetMinor int64
}

// KindLabel is the kind in words, a catalogue key.
func (c CalculationCost) KindLabel() string { return CostKindLabel(c.Kind) }

// CalculationInput is what a buyer puts into a calculation.
type CalculationInput struct {
	// Nil while nobody can say yet: then there is no bid to show.
	ExpectedSaleGrossMinor *int64
	Costs                  []CalculationCost
	ExpectedStockDays      int
	HoldingPerDayMinor     int64
	// The margin wanted: an amount, or basis points of the sale price;
	// one or neither. Below nothing is a loss taken on purpose.
	TargetMarginMinor *int64
	TargetMarginBasis *int
	// The kind of seller expected (#161), or "" when not known: the margin
	// scheme's bid is shown only for one that would make the car eligible.
	SellerType string
	Note       string
}

// Bid is the most that may be paid under one way of taxing the sale, and
// what the sale would carry in VAT at that price.
type Bid struct {
	MaxBidMinor  int64
	SaleVATMinor int64
}

// Possible reports whether any price leaves the margin wanted.
func (b Bid) Possible() bool { return b.MaxBidMinor >= 0 }

// CalculationResult is what a calculation comes to.
type CalculationResult struct {
	CostsMinor        int64
	HoldingMinor      int64
	TargetMarginMinor int64
	// The margin wanted is below nothing: a car sold at a loss on purpose.
	LossLeader bool
	// No sale price expected yet, so no bid.
	NoSalePrice bool
	Ordinary    Bid
	// Nil when the seller would not make the car eligible, or is not known.
	MarginScheme *Bid
}

var (
	errCalcSale    = fmt.Errorf("%w: the sale price expected is from nothing up to a billion kronor", ErrInvalid)
	errCalcCost    = fmt.Errorf("%w: a cost is a kind of cost and an amount from nothing up to a hundred million kronor", ErrInvalid)
	errCalcCosts   = fmt.Errorf("%w: a calculation has at most 50 costs", ErrInvalid)
	errCalcDays    = fmt.Errorf("%w: the days in stock are from none to ten years, and the cost of a day up to a million kronor", ErrInvalid)
	errCalcMargin  = fmt.Errorf("%w: the margin wanted is an amount or a share of the sale price, not both, and within the sale price either way", ErrInvalid)
	errCalcNote    = fmt.Errorf("%w: a note is at most 1000 characters", ErrInvalid)
	errCalcAgain   = fmt.Errorf("%w: a bid has already been made on this calculation", ErrInvalid)
	errCalcLater   = fmt.Errorf("%w: a later version of this calculation was saved meanwhile; read it again", ErrInvalid)
	errCalcBid     = fmt.Errorf("%w: a bid is from nothing up to a billion kronor", ErrInvalid)
	errCalcVehicle = fmt.Errorf("%w: that calculation is for another car", ErrInvalid)
)

func (in CalculationInput) check() error {
	if in.ExpectedSaleGrossMinor != nil && (*in.ExpectedSaleGrossMinor < 0 || *in.ExpectedSaleGrossMinor > maxPurchaseMinor) {
		return errCalcSale
	}
	if len(in.Costs) > 50 {
		return errCalcCosts
	}
	for _, c := range in.Costs {
		if !slices.Contains(CostKinds, c.Kind) || c.NetMinor < 0 || c.NetMinor > 10_000_000_000 {
			return errCalcCost
		}
	}
	if in.ExpectedStockDays < 0 || in.ExpectedStockDays > 3650 || in.HoldingPerDayMinor < 0 || in.HoldingPerDayMinor > 100_000_000 {
		return errCalcDays
	}
	switch {
	case in.TargetMarginMinor != nil && in.TargetMarginBasis != nil,
		in.TargetMarginMinor != nil && (*in.TargetMarginMinor < -maxPurchaseMinor || *in.TargetMarginMinor > maxPurchaseMinor),
		in.TargetMarginBasis != nil && (*in.TargetMarginBasis < -10000 || *in.TargetMarginBasis > 10000):
		return errCalcMargin
	}
	if in.SellerType != "" && !slices.Contains(SellerTypes, in.SellerType) {
		return errSellerType
	}
	if utf8.RuneCountInString(strings.TrimSpace(in.Note)) > 1000 {
		return errCalcNote
	}
	return nil
}

// Result works the calculation out. Every amount is minor units through
// internal/money, rounded as the package rounds; no float is anywhere near
// it.
//
// The business margin of a car is what it sells for, less the VAT in that,
// less what was paid for it, its costs and the cost of keeping it. Under
// ordinary VAT the VAT is the sale price's own, so the most to pay is the
// sale price without VAT less the rest: a price before any VAT the seller
// charges, which the dealer deducts. Under the margin scheme the VAT is on
// the margin alone (money.MarginSchemeVAT), so every krona off the price
// saves VAT as well: the most to pay is the sale price less one and a
// quarter times the costs and the margin wanted, at 25 per cent -- or, when
// those come to less than nothing, the sale price less them, with no VAT.
func (in CalculationInput) Result() CalculationResult {
	var r CalculationResult
	for _, c := range in.Costs {
		r.CostsMinor += c.NetMinor
	}
	r.HoldingMinor = int64(in.ExpectedStockDays) * in.HoldingPerDayMinor
	if in.ExpectedSaleGrossMinor == nil {
		r.NoSalePrice = true
		if in.TargetMarginMinor != nil {
			r.TargetMarginMinor = *in.TargetMarginMinor
		}
		r.LossLeader = r.TargetMarginMinor < 0 || (in.TargetMarginBasis != nil && *in.TargetMarginBasis < 0)
		return r
	}
	sale := *in.ExpectedSaleGrossMinor
	switch {
	case in.TargetMarginMinor != nil:
		r.TargetMarginMinor = *in.TargetMarginMinor
	case in.TargetMarginBasis != nil:
		r.TargetMarginMinor = money.Share(sale, *in.TargetMarginBasis)
	}
	r.LossLeader = r.TargetMarginMinor < 0
	keep := r.CostsMinor + r.HoldingMinor + r.TargetMarginMinor

	saleVAT := money.Scale(sale, carVATBasis, 10000+carVATBasis)
	r.Ordinary = Bid{MaxBidMinor: sale - saleVAT - keep, SaleVATMinor: saleVAT}

	if eligible, _ := EligibleFor(in.SellerType, StockFromPurchase, 1); eligible {
		b := marginSchemeBid(sale, keep)
		r.MarginScheme = &b
	}
	return r
}

// marginSchemeBid is the most that may be paid for a car to sell at sale
// under the margin scheme and leave keep -- costs, holding and the margin
// wanted -- after its VAT. Worked out, then made exact against
// money.MarginSchemeVAT itself, so that the bid shown is the highest whole
// öre whose margin is at least the margin wanted.
func marginSchemeBid(sale, keep int64) Bid {
	left := func(p int64) int64 { return sale - money.MarginSchemeVAT(sale, p, carVATBasis) - p }
	var p int64
	if keep >= 0 {
		p = sale - money.Scale(keep, 10000+carVATBasis, 10000)
	} else {
		p = sale - keep
	}
	for i := 0; i < 4 && left(p) < keep; i++ {
		p--
	}
	for i := 0; i < 4 && left(p+1) >= keep; i++ {
		p++
	}
	return Bid{MaxBidMinor: p, SaleVATMinor: money.MarginSchemeVAT(sale, p, carVATBasis)}
}

// Calculation is one version of a calculation, as it was saved.
type Calculation struct {
	ID            string
	CalculationID string
	Version       int
	VehicleID     string
	StockUnitID   string
	Input         CalculationInput
	CreatedAt     time.Time
	CreatedBy     string
	// The version a bid was made on, and the bid; "" and 0 when none yet.
	AcceptedVersionID string
	AcceptedBidMinor  int64
	AcceptedAt        *time.Time
	// The newest version of the calculation, for knowing this is not it.
	LatestVersion int
}

// Result is what the version comes to.
func (c Calculation) Result() CalculationResult { return c.Input.Result() }

// Accepted reports whether a bid was made on this version.
func (c Calculation) Accepted() bool { return c.AcceptedVersionID == c.ID }

// SaveCalculation saves a calculation for a car: a new one, or, given the
// calculation's id and the version it was edited from, its next version.
// A version is never changed, so an edit made from a version that is no
// longer the newest is refused rather than lost. For whoever may see what a
// car costs, in a shop that sells cars.
func SaveCalculation(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, vehicleID, calculationID string, fromVersion int, in CalculationInput) (Calculation, error) {
	if !scope.Role.SeesPurchasePrice() {
		return Calculation{}, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return Calculation{}, err
	}
	if !looksLikeUUID(vehicleID) || (calculationID != "" && !looksLikeUUID(calculationID)) {
		return Calculation{}, ErrNotFound
	}
	if err := in.check(); err != nil {
		return Calculation{}, err
	}
	in.Note = strings.TrimSpace(in.Note)
	var out Calculation
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM vehicles WHERE id = $1 AND merged_into IS NULL)`, vehicleID).Scan(&exists); err != nil {
			return fmt.Errorf("read vehicle: %w", err)
		}
		if !exists {
			return ErrNotFound
		}
		version := 1
		if calculationID != "" {
			// One version at a time per calculation: two saves from the same
			// version, the second waits and then finds it is not the newest.
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('calculation:' || $1::text, 0))`, calculationID); err != nil {
				return err
			}
			var latest int
			var vehicle string
			err := tx.QueryRow(ctx, `
				SELECT max(version), min(vehicle_id::text) FROM purchase_calculations WHERE calculation_id = $1 HAVING count(*) > 0`,
				calculationID).Scan(&latest, &vehicle)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return fmt.Errorf("read calculation: %w", err)
			}
			if !strings.EqualFold(vehicle, vehicleID) {
				return errCalcVehicle
			}
			if latest != fromVersion {
				return errCalcLater
			}
			version = latest + 1
		}
		var id string
		if err := tx.QueryRow(ctx, `
			WITH n AS (SELECT gen_random_uuid() AS id)
			INSERT INTO purchase_calculations (id, shop_id, calculation_id, version, vehicle_id, stock_unit_id,
			       expected_sale_gross_minor, expected_stock_days, holding_cost_per_day_minor, target_margin_minor,
			       target_margin_basis, seller_type, note, created_by)
			SELECT n.id, $1, coalesce(NULLIF($2, '')::uuid, n.id), $3, $4,
			       (SELECT u.id FROM stock_units u WHERE u.vehicle_id = $4 AND u.closed_at IS NULL),
			       $5, $6, $7, $8, $9, nullif($10, ''), nullif($11, ''), NULLIF($12, '')::uuid
			FROM n RETURNING id`,
			scope.ShopID, calculationID, version, vehicleID, in.ExpectedSaleGrossMinor, in.ExpectedStockDays,
			in.HoldingPerDayMinor, in.TargetMarginMinor, in.TargetMarginBasis, in.SellerType, in.Note, scope.UserID).Scan(&id); err != nil {
			return fmt.Errorf("save the calculation: %w", err)
		}
		for i, c := range in.Costs {
			if _, err := tx.Exec(ctx, `
				INSERT INTO purchase_calculation_costs (shop_id, calculation_version_id, position, kind, net_minor)
				VALUES ($1, $2, $3, $4, $5)`, scope.ShopID, id, i+1, c.Kind, c.NetMinor); err != nil {
				return fmt.Errorf("save a cost: %w", err)
			}
		}
		var err error
		out, err = calculationTx(ctx, tx, id)
		return err
	})
	return out, err
}

func calculationTx(ctx context.Context, tx pgx.Tx, id string) (Calculation, error) {
	var c Calculation
	var bid *int64
	var accepted *string
	err := tx.QueryRow(ctx, `
		SELECT c.id, c.calculation_id, c.version, c.vehicle_id, coalesce(c.stock_unit_id::text, ''),
		       c.expected_sale_gross_minor, c.expected_stock_days, c.holding_cost_per_day_minor, c.target_margin_minor,
		       c.target_margin_basis, coalesce(c.seller_type, ''), coalesce(c.note, ''), c.created_at, coalesce(pe.display_name, ''),
		       a.calculation_version_id::text, a.bid_minor, a.accepted_at,
		       (SELECT max(l.version) FROM purchase_calculations l WHERE l.calculation_id = c.calculation_id)
		FROM purchase_calculations c
		LEFT JOIN purchase_calculation_acceptances a ON a.calculation_id = c.calculation_id
		LEFT JOIN users u ON u.id = c.created_by LEFT JOIN people pe ON pe.id = u.person_id
		WHERE c.id = $1`, id).Scan(&c.ID, &c.CalculationID, &c.Version, &c.VehicleID, &c.StockUnitID,
		&c.Input.ExpectedSaleGrossMinor, &c.Input.ExpectedStockDays, &c.Input.HoldingPerDayMinor, &c.Input.TargetMarginMinor,
		&c.Input.TargetMarginBasis, &c.Input.SellerType, &c.Input.Note, &c.CreatedAt, &c.CreatedBy,
		&accepted, &bid, &c.AcceptedAt, &c.LatestVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return Calculation{}, ErrNotFound
	}
	if err != nil {
		return Calculation{}, fmt.Errorf("read calculation: %w", err)
	}
	c.AcceptedVersionID = deref(accepted)
	if bid != nil {
		c.AcceptedBidMinor = *bid
	}
	rows, err := tx.Query(ctx, `SELECT kind, net_minor FROM purchase_calculation_costs WHERE calculation_version_id = $1 ORDER BY position`, id)
	if err != nil {
		return Calculation{}, fmt.Errorf("read costs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k CalculationCost
		if err := rows.Scan(&k.Kind, &k.NetMinor); err != nil {
			return Calculation{}, err
		}
		c.Input.Costs = append(c.Input.Costs, k)
	}
	return c, rows.Err()
}

// CalculationByID reads one version of a calculation, for whoever may see
// what a car costs; a salesperson asking directly is refused. Reading is
// Core's.
func CalculationByID(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string) (Calculation, error) {
	if !scope.Role.SeesPurchasePrice() {
		return Calculation{}, access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return Calculation{}, ErrNotFound
	}
	var c Calculation
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		c, err = calculationTx(ctx, tx, id)
		return err
	})
	return c, err
}

// CalculationsForVehicle is the newest version of each calculation made for
// a car, newest first, for whoever may see what a car costs. Reading is
// Core's.
func CalculationsForVehicle(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, vehicleID string) ([]Calculation, error) {
	if !scope.Role.SeesPurchasePrice() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(vehicleID) {
		return nil, ErrNotFound
	}
	var out []Calculation
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT DISTINCT ON (calculation_id) id FROM purchase_calculations WHERE vehicle_id = $1
			ORDER BY calculation_id, version DESC`, vehicleID)
		if err != nil {
			return fmt.Errorf("read calculations: %w", err)
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
			c, err := calculationTx(ctx, tx, id)
			if err != nil {
				return err
			}
			out = append(out, c)
		}
		slices.SortFunc(out, func(a, b Calculation) int { return b.CreatedAt.Compare(a.CreatedAt) })
		return nil
	})
	return out, err
}

// AcceptCalculation records that a bid was made on this version: the
// version is frozen as the one used, and an edit after is a new version
// beside it. Once per calculation. For whoever buys cars, in a shop that
// sells them.
func AcceptCalculation(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, versionID string, bidMinor int64) (Calculation, error) {
	if !scope.Role.BuysVehicles() {
		return Calculation{}, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return Calculation{}, err
	}
	if !looksLikeUUID(versionID) {
		return Calculation{}, ErrNotFound
	}
	if bidMinor < 0 || bidMinor > maxPurchaseMinor {
		return Calculation{}, errCalcBid
	}
	var out Calculation
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		c, err := calculationTx(ctx, tx, versionID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO purchase_calculation_acceptances (shop_id, calculation_id, calculation_version_id, bid_minor, accepted_by)
			VALUES ($1, $2, $3, $4, NULLIF($5, '')::uuid)`, scope.ShopID, c.CalculationID, c.ID, bidMinor, scope.UserID)
		if isUniqueViolation(err, "purchase_calculation_acceptances_calculation_id_key") {
			return errCalcAgain
		}
		if err != nil {
			return fmt.Errorf("record the bid: %w", err)
		}
		out, err = calculationTx(ctx, tx, versionID)
		return err
	})
	return out, err
}

// CarToBuy is a car the buyer is looking at: by its plate, its chassis
// number, or both.
type CarToBuy struct {
	Registration string
	VIN          string
	Make         string
	Model        string
	ModelYear    *int16
}

var errCarUnknown = fmt.Errorf("%w: say the car's registration or its chassis number", ErrInvalid)

// CarForPurchase is the vehicle a buyer means: the one carrying the plate
// now, or with the chassis number, or else a new one with what was typed.
// Whatever the workshop already knows of a car stays as it is; nothing
// typed here writes over it. For whoever buys cars, in a shop that sells
// them.
func CarForPurchase(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, c CarToBuy) (string, error) {
	if !scope.Role.BuysVehicles() {
		return "", access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return "", err
	}
	plate := NormaliseRegistration(c.Registration)
	c.VIN = strings.ToUpper(strings.TrimSpace(c.VIN))
	switch {
	case plate == "" && c.VIN == "":
		return "", errCarUnknown
	case len(plate) > 20 || utf8.RuneCountInString(c.VIN) > 40 ||
		utf8.RuneCountInString(c.Make) > 100 || utf8.RuneCountInString(c.Model) > 100:
		return "", fmt.Errorf("%w: that is longer than a car's details are", ErrInvalid)
	case c.ModelYear != nil && (*c.ModelYear < 1885 || *c.ModelYear > 2100):
		return "", fmt.Errorf("%w: a model year is four digits", ErrInvalid)
	}
	var id string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT v.id FROM vehicles v
			WHERE v.merged_into IS NULL AND (
			      ($1 <> '' AND EXISTS (SELECT 1 FROM vehicle_registrations r WHERE r.vehicle_id = v.id AND r.normalised = $1 AND r.valid_to IS NULL))
			   OR ($2 <> '' AND upper(btrim(v.vin)) = $2))
			ORDER BY (v.vin IS NOT NULL AND upper(btrim(v.vin)) = $2) DESC, v.created_at
			LIMIT 1`, plate, c.VIN).Scan(&id)
		if err == nil || !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO vehicles (shop_id, make, model, model_year, vin)
			VALUES ($1, nullif(btrim($2), ''), nullif(btrim($3), ''), $4, nullif($5, '')) RETURNING id`,
			scope.ShopID, c.Make, c.Model, c.ModelYear, c.VIN).Scan(&id); err != nil {
			return fmt.Errorf("add the car: %w", err)
		}
		if plate != "" {
			if _, err := tx.Exec(ctx, `
				INSERT INTO vehicle_registrations (shop_id, vehicle_id, registration, normalised) VALUES ($1, $2, $3, $4)`,
				scope.ShopID, id, strings.TrimSpace(c.Registration), plate); err != nil {
				return fmt.Errorf("record the registration: %w", err)
			}
		}
		return nil
	})
	return id, err
}
