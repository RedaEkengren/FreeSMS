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
	"github.com/RedaEkengren/FreeSMS/internal/vehicledata"
)

// Checks before money leaves for a car (#162). Each is a row on the stock
// unit: what was checked, what it showed, who or what said so, and when.
// Checking again is another row; the newest of each kind is what stands. A
// purchase's payout is refused while a check the shop requires stands
// failed, unknown or too old, unless whoever runs the shop overrides it,
// with a reason. A trade-in and a consigned car are checked the same way.

// The kinds of check.
const (
	CheckDrivingBan     = "driving_ban"
	CheckCreditPurchase = "credit_purchase"
	CheckUnpaidTax      = "unpaid_tax"
	CheckInspection     = "inspection"
	CheckLeasingConsent = "leasing_consent"
	CheckVINMatches     = "vin_matches"
	CheckOdometer       = "odometer"
)

// CheckKinds is every kind, in the order a page lists them.
var CheckKinds = []string{CheckDrivingBan, CheckCreditPurchase, CheckUnpaidTax, CheckInspection, CheckLeasingConsent,
	CheckVINMatches, CheckOdometer}

var checkKindLabels = map[string]string{
	CheckDrivingBan:     "No driving ban",
	CheckCreditPurchase: "No credit purchase or undisclosed lender",
	CheckUnpaidTax:      "No unpaid vehicle tax or fees",
	CheckInspection:     "Inspection status known",
	CheckLeasingConsent: "Not leased, or the lessor has consented",
	CheckVINMatches:     "Chassis number matches the registration",
	CheckOdometer:       "Odometer fits earlier readings",
}

// CheckKindLabel is a kind of check in words, a catalogue key: what a pass
// means.
func CheckKindLabel(kind string) string { return checkKindLabels[kind] }

// The results of a check. Unknown is never a pass: a provider that is down,
// or not set up, has not said the car is clear.
const (
	CheckPass    = "pass"
	CheckFail    = "fail"
	CheckUnknown = "unknown"
)

// How long a check stands, with no setting: a day.
const defaultCheckAge = 24 * time.Hour

var (
	errCheckKind         = fmt.Errorf("%w: that is not a check", ErrInvalid)
	errCheckResult       = fmt.Errorf("%w: a check passed, failed or is not known", ErrInvalid)
	errCheckNote         = fmt.Errorf("%w: a note is at most 500 characters", ErrInvalid)
	errCheckAge          = fmt.Errorf("%w: a check stands for between an hour and 30 days", ErrInvalid)
	errCheckEnded        = fmt.Errorf("%w: that car's cycle is over; check it again in a new one", ErrInvalid)
	errLoanAmount        = fmt.Errorf("%w: say what is owed on the car, above nothing", ErrInvalid)
	errLoanReference     = fmt.Errorf("%w: say the reference the loan is settled with, in 100 characters or fewer", ErrInvalid)
	errNothingToOverride = fmt.Errorf("%w: every check the shop requires has passed in time, so there is nothing to let through", ErrInvalid)
	errLoanTwice         = fmt.Errorf("%w: that lender's loan is already recorded on this purchase", ErrInvalid)
	// ErrChecksOutstanding is a purchase's payout refused because a check
	// the shop requires has not passed, or passed too long ago.
	ErrChecksOutstanding = fmt.Errorf("%w: not every check the shop requires has passed in time; check again, or have the owner let it through with a reason", ErrInvalid)
	// ErrOverrideNoReason is an override that does not say why.
	ErrOverrideNoReason = fmt.Errorf("%w: say why the money may leave anyway, in 300 characters or fewer", ErrInvalid)
)

// StockCheck is one check, as it stands.
type StockCheck struct {
	ID         string
	Kind       string
	Result     string
	Source     string
	Provider   string
	CheckedAt  time.Time
	CheckedBy  string
	EvidenceID string
	Note       string
}

// KindLabel is what the check is, a catalogue key.
func (c StockCheck) KindLabel() string { return CheckKindLabel(c.Kind) }

// NewStockCheck is a check made by hand.
type NewStockCheck struct {
	Kind       string
	Result     string
	EvidenceID string
	Note       string
}

// unitOpenTx locks a unit and refuses one whose cycle is over.
func unitOpenTx(ctx context.Context, tx pgx.Tx, unitID string) (vehicleID string, err error) {
	var ended bool
	err = tx.QueryRow(ctx, `SELECT vehicle_id, closed_at IS NOT NULL FROM stock_units WHERE id = $1 FOR UPDATE`, unitID).Scan(&vehicleID, &ended)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read stock unit: %w", err)
	}
	if ended {
		return "", errCheckEnded
	}
	return vehicleID, nil
}

func insertCheckTx(ctx context.Context, tx pgx.Tx, scope access.Scope, unitID, kind, result, source, provider, evidence, note string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO stock_checks (shop_id, stock_unit_id, kind, result, source, provider, checked_by, evidence_document_id, note)
		VALUES ($1, $2, $3, $4, $5, nullif($6, ''), NULLIF($7, '')::uuid, NULLIF($8, '')::uuid, nullif($9, ''))`,
		scope.ShopID, unitID, kind, result, source, provider, scope.UserID, evidence, note)
	if err != nil {
		return fmt.Errorf("record the check: %w", err)
	}
	return nil
}

// RecordStockCheck records a check made by hand: the way that always works,
// whatever provider the shop has or has not. For whoever buys cars, in a
// shop that sells them, on a car whose cycle is open.
func RecordStockCheck(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, unitID string, c NewStockCheck) error {
	if !scope.Role.BuysVehicles() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return err
	}
	switch {
	case !slices.Contains(CheckKinds, c.Kind):
		return errCheckKind
	case c.Result != CheckPass && c.Result != CheckFail && c.Result != CheckUnknown:
		return errCheckResult
	case !looksLikeUUID(unitID), c.EvidenceID != "" && !looksLikeUUID(c.EvidenceID):
		return ErrNotFound
	}
	c.Note = strings.TrimSpace(c.Note)
	if utf8.RuneCountInString(c.Note) > 500 {
		return errCheckNote
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := unitOpenTx(ctx, tx, unitID); err != nil {
			return err
		}
		if c.EvidenceID != "" {
			if err := evidenceTx(ctx, tx, []string{c.EvidenceID}); err != nil {
				return err
			}
		}
		return insertCheckTx(ctx, tx, scope, unitID, c.Kind, c.Result, "manual", "", c.EvidenceID, c.Note)
	})
}

// CheckWithProvider asks the shop's vehicle data provider (#155) what it
// knows of the unit's car, and records a check for each fact the checks
// read: the driving ban, the tax, the inspection, the chassis number and
// the odometer. A fact the provider does not offer, or a provider that does
// not answer -- not set up, down, too slow -- is a check of unknown: never a
// pass, and the manual way stays open. A credit purchase and a lessor's
// consent are not in the register and stay the buyer's to check by hand.
// The provider is asked outside any transaction.
func CheckWithProvider(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, l vehicledata.Lookup, provider, unitID string) ([]StockCheck, error) {
	if !scope.Role.BuysVehicles() {
		return nil, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return nil, err
	}
	if !looksLikeUUID(unitID) {
		return nil, ErrNotFound
	}
	if provider = strings.TrimSpace(provider); provider == "" {
		provider = "provider"
	}
	var registration, vin string
	var purchaseKm *int64
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		vehicleID, err := unitOpenTx(ctx, tx, unitID)
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			SELECT coalesce((SELECT r.registration FROM vehicle_registrations r WHERE r.vehicle_id = v.id AND r.valid_to IS NULL), ''),
			       coalesce(v.vin, ''),
			       (SELECT o.km FROM vehicle_purchases p JOIN odometer_readings o ON o.id = p.odometer_reading_id WHERE p.stock_unit_id = $2)
			FROM vehicles v WHERE v.id = $1`, vehicleID, unitID).Scan(&registration, &vin, &purchaseKm)
	})
	if err != nil {
		return nil, err
	}
	var found vehicledata.Vehicle
	answered := false
	if registration != "" {
		got, err := LookUpVehicle(ctx, pool, scope, l, provider, registration, LookupPurchase, vin, true)
		if err == nil && got.Outcome == LookupFound {
			found, answered = got.Vehicle, true
		}
	}
	results := providerResults(found, answered, vin, purchaseKm)
	err = database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := unitOpenTx(ctx, tx, unitID); err != nil {
			return err
		}
		for _, kind := range []string{CheckDrivingBan, CheckUnpaidTax, CheckInspection, CheckVINMatches, CheckOdometer} {
			if err := insertCheckTx(ctx, tx, scope, unitID, kind, results[kind], "provider", provider, "", ""); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return StockChecks(ctx, pool, scope, unitID)
}

// providerResults reads what a provider said as checks. Unanswered, every
// one is unknown. Tax is a pass only when the register says it is paid, in
// the words a provider uses for that; anything else said about it is a
// failure for a person to look at, and nothing said is unknown.
func providerResults(v vehicledata.Vehicle, answered bool, vin string, purchaseKm *int64) map[string]string {
	out := map[string]string{CheckDrivingBan: CheckUnknown, CheckUnpaidTax: CheckUnknown, CheckInspection: CheckUnknown,
		CheckVINMatches: CheckUnknown, CheckOdometer: CheckUnknown}
	if !answered {
		return out
	}
	if v.DrivingBan.Provided {
		out[CheckDrivingBan] = passIf(!v.DrivingBan.Value)
	}
	if v.Tax.Provided {
		t := strings.ToLower(strings.TrimSpace(v.Tax.Value))
		out[CheckUnpaidTax] = passIf(t == "paid" || t == "betald" || t == "betalt")
	}
	if v.Inspection.Provided && strings.TrimSpace(v.Inspection.Value) != "" {
		out[CheckInspection] = CheckPass
	}
	if v.VIN.Provided && strings.TrimSpace(vin) != "" {
		out[CheckVINMatches] = passIf(strings.EqualFold(strings.TrimSpace(v.VIN.Value), strings.TrimSpace(vin)))
	}
	if v.OdometerKm.Provided && purchaseKm != nil {
		// The car read less than the register last saw: wound back.
		out[CheckOdometer] = passIf(*purchaseKm >= v.OdometerKm.Value)
	}
	return out
}

func passIf(ok bool) string {
	if ok {
		return CheckPass
	}
	return CheckFail
}

// odometerCheckTx records, as the system, whether a purchase's odometer
// reading fits the readings held before it: one lower than an earlier one
// is a failure for a person to look at.
func odometerCheckTx(ctx context.Context, tx pgx.Tx, scope access.Scope, unitID, readingID string) error {
	var decreasing bool
	if err := tx.QueryRow(ctx, `SELECT decreasing FROM odometer_readings WHERE id = $1`, readingID).Scan(&decreasing); err != nil {
		return fmt.Errorf("read the odometer: %w", err)
	}
	note := ""
	if decreasing {
		note = "Lower than a reading held before"
	}
	return insertCheckTx(ctx, tx, scope, unitID, CheckOdometer, passIf(!decreasing), "system", "", "", note)
}

// StockCheckSetting is whether the shop requires a kind of check before
// money leaves, and how old it may be.
type StockCheckSetting struct {
	Kind     string
	Required bool
	MaxAge   time.Duration
}

// KindLabel is what the check is, a catalogue key.
func (s StockCheckSetting) KindLabel() string { return CheckKindLabel(s.Kind) }

func checkSettingsTx(ctx context.Context, tx pgx.Tx) (map[string]StockCheckSetting, error) {
	out := map[string]StockCheckSetting{}
	for _, k := range CheckKinds {
		out[k] = StockCheckSetting{Kind: k, Required: true, MaxAge: defaultCheckAge}
	}
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT ON (kind) kind, required, (extract(epoch FROM max_age) * 1000000)::bigint
		FROM stock_check_settings ORDER BY kind, seq DESC`)
	if err != nil {
		return nil, fmt.Errorf("read the check settings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s StockCheckSetting
		var micros int64
		if err := rows.Scan(&s.Kind, &s.Required, &micros); err != nil {
			return nil, err
		}
		s.MaxAge = time.Duration(micros) * time.Microsecond
		out[s.Kind] = s
	}
	return out, rows.Err()
}

// StockCheckSettings reads which checks the shop requires, in the order
// they are listed. For whoever buys cars; reading is Core's.
func StockCheckSettings(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]StockCheckSetting, error) {
	if !scope.Role.BuysVehicles() {
		return nil, access.ErrForbidden
	}
	var out []StockCheckSetting
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		m, err := checkSettingsTx(ctx, tx)
		for _, k := range CheckKinds {
			out = append(out, m[k])
		}
		return err
	})
	return out, err
}

// SetStockCheck says whether the shop requires a kind of check before money
// leaves for a car, and how long one stands: from an hour to 30 days. For
// whoever runs the shop, in a shop that sells cars.
func SetStockCheck(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, s StockCheckSetting) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return err
	}
	if !slices.Contains(CheckKinds, s.Kind) {
		return errCheckKind
	}
	if s.MaxAge < time.Hour || s.MaxAge > 30*24*time.Hour {
		return errCheckAge
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO stock_check_settings (shop_id, kind, required, max_age, changed_by)
			VALUES ($1, $2, $3, make_interval(secs => $4), NULLIF($5, '')::uuid)`,
			scope.ShopID, s.Kind, s.Required, s.MaxAge.Seconds(), scope.UserID)
		return err
	})
}

// StockChecks is the checks standing on a unit -- the newest of each kind
// -- in the order kinds are listed. For whoever buys cars; reading is
// Core's.
func StockChecks(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, unitID string) ([]StockCheck, error) {
	if !scope.Role.BuysVehicles() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(unitID) {
		return nil, ErrNotFound
	}
	var out []StockCheck
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = standingChecksTx(ctx, tx, unitID)
		return err
	})
	return out, err
}

func standingChecksTx(ctx context.Context, tx pgx.Tx, unitID string) ([]StockCheck, error) {
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT ON (c.kind) c.id, c.kind, c.result, c.source, coalesce(c.provider, ''), c.checked_at,
		       coalesce(pe.display_name, ''), coalesce(c.evidence_document_id::text, ''), coalesce(c.note, '')
		FROM stock_checks c
		LEFT JOIN users u ON u.id = c.checked_by LEFT JOIN people pe ON pe.id = u.person_id
		WHERE c.stock_unit_id = $1
		ORDER BY c.kind, c.seq DESC`, unitID)
	if err != nil {
		return nil, fmt.Errorf("read checks: %w", err)
	}
	defer rows.Close()
	byKind := map[string]StockCheck{}
	for rows.Next() {
		var c StockCheck
		if err := rows.Scan(&c.ID, &c.Kind, &c.Result, &c.Source, &c.Provider, &c.CheckedAt, &c.CheckedBy, &c.EvidenceID, &c.Note); err != nil {
			return nil, err
		}
		byKind[c.Kind] = c
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []StockCheck
	for _, k := range CheckKinds {
		if c, ok := byKind[k]; ok {
			out = append(out, c)
		}
	}
	return out, nil
}

// checksOutstandingTx is why a unit's checks do not let money leave yet: the kinds
// the shop requires that stand failed, unknown, too old, or not made at all.
// Nil when nothing is outstanding.
func checksOutstandingTx(ctx context.Context, tx pgx.Tx, unitID string) ([]string, error) {
	settings, err := checkSettingsTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return nil, err
	}
	standing, err := standingChecksTx(ctx, tx, unitID)
	if err != nil {
		return nil, err
	}
	byKind := map[string]StockCheck{}
	for _, c := range standing {
		byKind[c.Kind] = c
	}
	var out []string
	for _, k := range CheckKinds {
		s := settings[k]
		if !s.Required {
			continue
		}
		c, ok := byKind[k]
		if !ok || c.Result != CheckPass || now.Sub(c.CheckedAt) > s.MaxAge {
			out = append(out, k)
		}
	}
	return out, nil
}

// overriddenTx reports whether an owner's override lets the unit's money
// leave over the kinds of check outstanding now: each must be one an
// override was made over, no check of it made since, and the override no
// older than a check of that kind may be.
func overriddenTx(ctx context.Context, tx pgx.Tx, unitID string, outstanding []string, settings map[string]StockCheckSetting) (bool, error) {
	for _, k := range outstanding {
		var ok bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM stock_check_overrides o
			               WHERE o.stock_unit_id = $1 AND $2 = ANY (o.kinds)
			                 AND o.upto_seq >= coalesce((SELECT max(seq) FROM stock_checks WHERE stock_unit_id = $1 AND kind = $2), 0)
			                 AND o.at > clock_timestamp() - make_interval(secs => $3))`,
			unitID, k, settings[k].MaxAge.Seconds()).Scan(&ok); err != nil {
			return false, fmt.Errorf("read overrides: %w", err)
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

var (
	errPayoutSubject     = fmt.Errorf("%w: a payout for a car names the purchase it pays, and is to its seller or to a lender on it", ErrInvalid)
	errConsignmentPayout = fmt.Errorf("%w: a consigned car's principal is paid under the consignment agreement, which this release does not keep yet", ErrInvalid)
)

// payoutSubjectTx refuses a payout of purpose that does not say what it
// pays, or pays somebody it does not owe: a purchase's payout names the
// purchase, and is to the seller or a lender on it, so that the checks it
// is let through on are that car's. A consignment's waits for its
// agreement (#166). Called when a payout is opened and when it is paid.
func payoutSubjectTx(ctx context.Context, tx pgx.Tx, purpose, subjectKind, subjectID, partyID string) error {
	switch purpose {
	case "purchase":
		if subjectKind != "purchase" || !looksLikeUUID(subjectID) {
			return errPayoutSubject
		}
		var owed bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM vehicle_purchases p WHERE p.id = $1 AND (
			       party_now(p.seller_party_id) = party_now($2)
			    OR EXISTS (SELECT 1 FROM purchase_loans l WHERE l.purchase_id = p.id AND party_now(l.lender_party_id) = party_now($2))))`,
			subjectID, partyID).Scan(&owed); err != nil {
			return fmt.Errorf("read the purchase paid: %w", err)
		}
		if !owed {
			return errPayoutSubject
		}
	case "consignment":
		return errConsignmentPayout
	}
	return nil
}

// purchaseChecksClearTx refuses a payout for a purchase whose unit's
// required checks do not all stand passed in time, unless an owner let it
// through. Called by RecordPayout, inside its transaction.
func purchaseChecksClearTx(ctx context.Context, tx pgx.Tx, purchaseID string) error {
	// The unit held for share: a check being recorded holds it for update
	// (unitOpenTx), so the payout waits for that check and then reads it,
	// rather than paying on the checks as they stood a moment before.
	var unitID string
	err := tx.QueryRow(ctx, `
		SELECT u.id FROM vehicle_purchases p JOIN stock_units u ON u.id = p.stock_unit_id
		WHERE p.id = $1 FOR SHARE OF u`, purchaseID).Scan(&unitID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("read purchase: %w", err)
	}
	outstanding, err := checksOutstandingTx(ctx, tx, unitID)
	if err != nil || len(outstanding) == 0 {
		return err
	}
	settings, err := checkSettingsTx(ctx, tx)
	if err != nil {
		return err
	}
	ok, err := overriddenTx(ctx, tx, unitID, outstanding, settings)
	if err != nil {
		return err
	}
	if !ok {
		return ErrChecksOutstanding
	}
	return nil
}

// ChecksOutstanding is the kinds of check that keep a unit's money from
// leaving now, and whether an owner has let it through anyway. For whoever
// buys cars; reading is Core's.
func ChecksOutstanding(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, unitID string) (kinds []string, overridden bool, err error) {
	if !scope.Role.BuysVehicles() {
		return nil, false, access.ErrForbidden
	}
	if !looksLikeUUID(unitID) {
		return nil, false, ErrNotFound
	}
	err = database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM stock_units WHERE id = $1)`, unitID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		if kinds, err = checksOutstandingTx(ctx, tx, unitID); err != nil {
			return err
		}
		if len(kinds) == 0 {
			return nil
		}
		settings, err := checkSettingsTx(ctx, tx)
		if err != nil {
			return err
		}
		overridden, err = overriddenTx(ctx, tx, unitID, kinds, settings)
		return err
	})
	return kinds, overridden, err
}

// OverrideStockChecks lets a unit's money leave although its checks have
// not all passed in time: whoever runs the shop, with a reason, an audit
// event. It covers the kinds of check outstanding as it is made, for as
// long as such a check would stand; a check of the kind made after it is
// new information, and the override no longer covers it. With nothing
// outstanding there is nothing to override.
func OverrideStockChecks(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, unitID, reason string) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return err
	}
	if !looksLikeUUID(unitID) {
		return ErrNotFound
	}
	reason, err := checkReason(reason, ErrOverrideNoReason)
	if err != nil {
		return err
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := unitOpenTx(ctx, tx, unitID); err != nil {
			return err
		}
		outstanding, err := checksOutstandingTx(ctx, tx, unitID)
		if err != nil {
			return err
		}
		if len(outstanding) == 0 {
			return errNothingToOverride
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO stock_check_overrides (shop_id, stock_unit_id, upto_seq, kinds, by_user, reason)
			VALUES ($1, $2, coalesce((SELECT max(seq) FROM stock_checks WHERE stock_unit_id = $2), 0), $3, $4, $5)`,
			scope.ShopID, unitID, outstanding, scope.UserID, reason); err != nil {
			return fmt.Errorf("record the override: %w", err)
		}
		return auditTx(ctx, tx, scope, unitID, "stock_checks.overridden", "", "", reason,
			map[string]any{"outstanding": outstanding})
	})
}

// PurchaseLoan is a loan found on a car bought: to whom, how much, and the
// reference it is settled with.
type PurchaseLoan struct {
	ID                  string
	LenderPartyID       string
	AmountMinor         int64
	SettlementReference string
}

// RecordPurchaseLoan records a loan the checks found on a car bought. What
// is paid to the lender is still part of the tax purchase price; the seller
// is paid what is left (PurchaseSplit). For whoever buys cars, in a shop
// that sells them.
func RecordPurchaseLoan(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, purchaseID string, l PurchaseLoan) error {
	if !scope.Role.BuysVehicles() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return err
	}
	if !looksLikeUUID(purchaseID) || !looksLikeUUID(l.LenderPartyID) {
		return ErrNotFound
	}
	if l.AmountMinor <= 0 || l.AmountMinor > maxPurchaseMinor {
		return errLoanAmount
	}
	l.SettlementReference = strings.TrimSpace(l.SettlementReference)
	if l.SettlementReference == "" || utf8.RuneCountInString(l.SettlementReference) > 100 {
		return errLoanReference
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := partySeenTx(ctx, tx, scope, l.LenderPartyID); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM vehicle_purchases WHERE id = $1)`, purchaseID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO purchase_loans (shop_id, purchase_id, lender_party_id, amount_minor, settlement_reference, recorded_by)
			VALUES ($1, $2, party_now($3), $4, $5, NULLIF($6, '')::uuid)`,
			scope.ShopID, purchaseID, l.LenderPartyID, l.AmountMinor, l.SettlementReference, scope.UserID)
		if isUniqueViolation(err, "purchase_loans_purchase_id_lender_party_id_key") {
			return errLoanTwice
		}
		return err
	})
}

// PurchaseSplit is how a purchase's price is paid: to each lender what is
// owed to it, and to the seller the rest. SellerMinor below nothing is what
// the seller owes the dealer: the loan was larger than the price.
type PurchaseSplit struct {
	PriceMinor  int64
	Loans       []PurchaseLoan
	SellerMinor int64
}

// PurchaseSplitOf reads how a purchase's price is paid, for whoever may see
// what the car cost. Reading is Core's.
func PurchaseSplitOf(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, purchaseID string) (PurchaseSplit, error) {
	if !scope.Role.SeesPurchasePrice() {
		return PurchaseSplit{}, access.ErrForbidden
	}
	if !looksLikeUUID(purchaseID) {
		return PurchaseSplit{}, ErrNotFound
	}
	var out PurchaseSplit
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT tax_purchase_price_minor FROM vehicle_purchases WHERE id = $1`, purchaseID).Scan(&out.PriceMinor)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT id, lender_party_id, amount_minor, settlement_reference FROM purchase_loans
			WHERE purchase_id = $1 ORDER BY recorded_at`, purchaseID)
		if err != nil {
			return err
		}
		defer rows.Close()
		out.SellerMinor = out.PriceMinor
		for rows.Next() {
			var l PurchaseLoan
			if err := rows.Scan(&l.ID, &l.LenderPartyID, &l.AmountMinor, &l.SettlementReference); err != nil {
				return err
			}
			out.Loans = append(out.Loans, l)
			out.SellerMinor -= l.AmountMinor
		}
		return rows.Err()
	})
	return out, err
}
