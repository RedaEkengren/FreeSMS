package workshop_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/vehicledata"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Checks before money leaves for a car (#162).

// passEveryCheck records every check passed, by hand, as the buyer.
func passEveryCheck(t *testing.T, pool *pgxpool.Pool, unitID string) {
	t.Helper()
	for _, k := range workshop.CheckKinds {
		if err := workshop.RecordStockCheck(context.Background(), pool, buyerScope(), unitID,
			workshop.NewStockCheck{Kind: k, Result: workshop.CheckPass}); err != nil {
			t.Fatalf("RecordStockCheck(%s): %v", k, err)
		}
	}
}

// purchasePayout is a payout to seller for a car bought from them and
// checked: a car of its own, so that a test may make as many as it needs.
func purchasePayout(t *testing.T, pool *pgxpool.Pool, scope access.Scope, seller string) string {
	t.Helper()
	p := boughtCarFrom(t, pool, seller)
	passEveryCheck(t, pool, p.StockUnitID)
	payout, err := workshop.CreatePayout(context.Background(), pool, scope, workshop.NewHolding{PartyID: seller, Purpose: "purchase",
		SubjectKind: "purchase", SubjectID: p.ID})
	if err != nil {
		t.Fatalf("CreatePayout: %v", err)
	}
	return payout
}

func payOut(pool *pgxpool.Pool, s access.Scope, payout string, minor int64) error {
	_, err := workshop.RecordPayout(context.Background(), pool, s, payout, workshop.NewPayment{AmountMinor: &minor, Method: "bank", PaidOn: time.Now()})
	return err
}

// A payout is refused, by a direct call, while a check the shop requires
// stands failed, unknown, too old or not made; once they all pass it goes
// through. An owner may let it through with a reason, which is on the
// unit's trail; a check made after that is new, and the override no
// longer covers it.
func TestNoMoneyLeavesForACarBeforeItsChecksPass(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	p := buy(t, pool, workshop.StockFromPurchase)
	payout, err := workshop.CreatePayout(ctx, pool, buyerScope(), workshop.NewHolding{PartyID: p.SellerPartyID, Purpose: "purchase",
		SubjectKind: "purchase", SubjectID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	// The odometer was checked as the purchase was recorded, by the system.
	checks, err := workshop.StockChecks(ctx, pool, buyerScope(), p.StockUnitID)
	if err != nil || len(checks) != 1 || checks[0].Kind != workshop.CheckOdometer || checks[0].Source != "system" || checks[0].Result != workshop.CheckPass {
		t.Fatalf("checks after the purchase: %+v, %v", checks, err)
	}
	if err := payOut(pool, buyerScope(), payout, 8_500_000); !errors.Is(err, workshop.ErrChecksOutstanding) {
		t.Fatalf("paid out with no checks: %v", err)
	}
	passEveryCheck(t, pool, p.StockUnitID)
	if err := workshop.RecordStockCheck(ctx, pool, buyerScope(), p.StockUnitID, workshop.NewStockCheck{
		Kind: workshop.CheckCreditPurchase, Result: workshop.CheckFail, Note: "Kreditköp hos banken"}); err != nil {
		t.Fatal(err)
	}
	kinds, overridden, err := workshop.ChecksOutstanding(ctx, pool, buyerScope(), p.StockUnitID)
	if err != nil || overridden || !slices.Equal(kinds, []string{workshop.CheckCreditPurchase}) {
		t.Errorf("outstanding after a failed check: %v %v %v", kinds, overridden, err)
	}
	if err := payOut(pool, buyerScope(), payout, 8_500_000); !errors.Is(err, workshop.ErrChecksOutstanding) {
		t.Errorf("paid out with a failed check: %v", err)
	}

	// The owner lets it through, with a reason.
	if err := workshop.OverrideStockChecks(ctx, pool, buyerScope(), p.StockUnitID, "x"); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the buyer overrode the checks: %v", err)
	}
	if err := workshop.OverrideStockChecks(ctx, pool, everything(owner()), p.StockUnitID, ""); !errors.Is(err, workshop.ErrOverrideNoReason) {
		t.Errorf("overridden with no reason: %v", err)
	}
	if err := workshop.OverrideStockChecks(ctx, pool, everything(owner()), p.StockUnitID, "Lånet löses av oss i dag"); err != nil {
		t.Fatal(err)
	}
	trail, _ := workshop.AuditTrail(ctx, pool, buyerScope(), "stock_unit", p.StockUnitID)
	if last := trail[len(trail)-1]; last.Event != "stock_checks.overridden" || last.Reason != "Lånet löses av oss i dag" || last.Actor == "" {
		t.Errorf("the override on the trail: %+v", last)
	}
	// Checked again since: the override covered what stood then.
	if err := workshop.RecordStockCheck(ctx, pool, buyerScope(), p.StockUnitID, workshop.NewStockCheck{
		Kind: workshop.CheckDrivingBan, Result: workshop.CheckUnknown}); err != nil {
		t.Fatal(err)
	}
	if err := payOut(pool, buyerScope(), payout, 8_500_000); !errors.Is(err, workshop.ErrChecksOutstanding) {
		t.Errorf("paid out over a check made after the override: %v", err)
	}
	if err := workshop.OverrideStockChecks(ctx, pool, everything(owner()), p.StockUnitID, "Kontrollerat per telefon"); err != nil {
		t.Fatal(err)
	}
	if err := payOut(pool, buyerScope(), payout, 8_500_000); err != nil {
		t.Errorf("paid out once overridden: %v", err)
	}
}

// A check that passed too long ago stands no more: the shop says how long.
// A check the shop does not require keeps nothing back.
func TestACheckThatHasExpiredKeepsTheMoneyBack(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	p := buy(t, pool, workshop.StockFromPurchase)
	passEveryCheck(t, pool, p.StockUnitID)
	payout, err := workshop.CreatePayout(ctx, pool, buyerScope(), workshop.NewHolding{PartyID: p.SellerPartyID, Purpose: "purchase",
		SubjectKind: "purchase", SubjectID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE stock_checks SET checked_at = checked_at - interval '2 days' WHERE kind = 'inspection'`); err != nil {
		t.Fatal(err)
	}
	if err := payOut(pool, buyerScope(), payout, 100); !errors.Is(err, workshop.ErrChecksOutstanding) {
		t.Errorf("paid out over a check two days old: %v", err)
	}
	if err := workshop.SetStockCheck(ctx, pool, buyerScope(), workshop.StockCheckSetting{Kind: workshop.CheckInspection, MaxAge: time.Hour}); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the buyer changed what the shop requires: %v", err)
	}
	if err := workshop.SetStockCheck(ctx, pool, everything(owner()), workshop.StockCheckSetting{Kind: workshop.CheckInspection, Required: true, MaxAge: time.Minute}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a check standing a minute: %v", err)
	}
	if err := workshop.SetStockCheck(ctx, pool, everything(owner()), workshop.StockCheckSetting{Kind: workshop.CheckInspection, Required: true, MaxAge: 72 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := payOut(pool, buyerScope(), payout, 100); err != nil {
		t.Errorf("paid out within three days of the check: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE stock_checks SET result = 'fail' WHERE kind = 'leasing_consent'`); err != nil {
		t.Fatal(err)
	}
	if err := workshop.SetStockCheck(ctx, pool, everything(owner()), workshop.StockCheckSetting{Kind: workshop.CheckLeasingConsent, MaxAge: 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := payOut(pool, buyerScope(), payout, 100); err != nil {
		t.Errorf("paid out with a check the shop does not require failed: %v", err)
	}
	settings, err := workshop.StockCheckSettings(ctx, pool, buyerScope())
	if err != nil || len(settings) != len(workshop.CheckKinds) {
		t.Fatalf("settings: %+v, %v", settings, err)
	}
	for _, s := range settings {
		switch s.Kind {
		case workshop.CheckInspection:
			if !s.Required || s.MaxAge != 72*time.Hour {
				t.Errorf("inspection: %+v", s)
			}
		case workshop.CheckLeasingConsent:
			if s.Required {
				t.Errorf("leasing consent: %+v", s)
			}
		default:
			if !s.Required || s.MaxAge != 24*time.Hour {
				t.Errorf("%s by default: %+v", s.Kind, s)
			}
		}
	}
}

// A purchase payout names its purchase: one that does not cannot be paid
// out, so the checks cannot be stepped round.
func TestAPurchasePayoutNamesItsPurchase(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	if _, err := workshop.CreatePayout(ctx, pool, buyerScope(), workshop.NewHolding{PartyID: privateSeller(t, pool), Purpose: "purchase"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a purchase payout naming no purchase was opened: %v", err)
	}
	// One written as an earlier release could, with no purchase, is not paid.
	var payout string
	if err := pool.QueryRow(ctx, `INSERT INTO payouts (shop_id, party_id, purpose, created_by) SELECT $1, id, 'purchase', $2 FROM parties LIMIT 1 RETURNING id`,
		shopID, userID).Scan(&payout); err != nil {
		t.Fatal(err)
	}
	if err := payOut(pool, buyerScope(), payout, 100); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a purchase payout naming no purchase was paid: %v", err)
	}
}

// providerLookup answers as a provider would, or fails as one that is down.
type providerLookup struct {
	v   vehicledata.Vehicle
	err error
}

func (p providerLookup) Lookup(context.Context, string) (vehicledata.Vehicle, error) {
	return p.v, p.err
}

// A provider fills in what the register knows; a provider that does not
// answer leaves every check unknown, never passed, and the manual way works.
func TestAProviderFillsChecksAndOneThatIsDownLeavesThemUnknown(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE vehicles SET vin = 'YV1SW61R021234567' WHERE id = $1`, vehicleA); err != nil {
		t.Fatal(err)
	}
	p := buy(t, pool, workshop.StockFromPurchase)
	down := providerLookup{err: context.DeadlineExceeded}
	checks, err := workshop.CheckWithProvider(ctx, pool, buyerScope(), down, "testleverantör", p.StockUnitID)
	if err != nil {
		t.Fatalf("with the provider down: %v", err)
	}
	for _, c := range checks {
		if c.Source == "provider" && c.Result != workshop.CheckUnknown {
			t.Errorf("with the provider down, %s is %s", c.Kind, c.Result)
		}
	}
	if err := workshop.RecordStockCheck(ctx, pool, buyerScope(), p.StockUnitID, workshop.NewStockCheck{
		Kind: workshop.CheckDrivingBan, Result: workshop.CheckPass, Note: "Kontrollerat i Transportstyrelsens e-tjänst"}); err != nil {
		t.Errorf("the manual way with the provider down: %v", err)
	}

	up := providerLookup{v: vehicledata.Vehicle{Make: "Volvo", VIN: vehicledata.Of("YV1SW61R021234567"),
		DrivingBan: vehicledata.Of(false), Tax: vehicledata.Of("unpaid"), Inspection: vehicledata.Of("approved until 2027-05-31"),
		OdometerKm: vehicledata.Of(int64(130000))}}
	checks, err = workshop.CheckWithProvider(ctx, pool, buyerScope(), up, "testleverantör", p.StockUnitID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range checks {
		got[c.Kind] = c.Result
	}
	want := map[string]string{workshop.CheckDrivingBan: "pass", workshop.CheckUnpaidTax: "fail", workshop.CheckInspection: "pass",
		workshop.CheckVINMatches: "pass", workshop.CheckOdometer: "fail"}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s from the provider: %s, want %s", k, got[k], w)
		}
	}
	if _, ok := got[workshop.CheckCreditPurchase]; ok {
		t.Error("the provider was taken to have said something about a credit purchase")
	}
}

// A loan on the car is paid to the lender out of the price; the seller has
// the rest, or owes the difference.
func TestALoanOnTheCarIsPaidOutOfThePrice(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	p := buy(t, pool, workshop.StockFromPurchase)
	lender, err := workshop.CreateParty(ctx, pool, buyerScope(), workshop.NewParty{Kind: "company", Name: "Bilfinans AB",
		OrgNumber: "556000-0000", Role: workshop.PartySupplier})
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.RecordPurchaseLoan(ctx, pool, buyerScope(), p.ID, workshop.PurchaseLoan{LenderPartyID: lender,
		AmountMinor: 9_000_000, SettlementReference: "Lösen 4711"}); err != nil {
		t.Fatal(err)
	}
	if err := workshop.RecordPurchaseLoan(ctx, pool, buyerScope(), p.ID, workshop.PurchaseLoan{LenderPartyID: lender,
		AmountMinor: 1, SettlementReference: "igen"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("the same lender twice: %v", err)
	}
	split, err := workshop.PurchaseSplitOf(ctx, pool, buyerScope(), p.ID)
	if err != nil || split.PriceMinor != 8_500_000 || len(split.Loans) != 1 || split.SellerMinor != -500_000 {
		t.Errorf("a loan larger than the price: %+v, %v", split, err)
	}
	if _, err := workshop.PurchaseSplitOf(ctx, pool, sales(), p.ID); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a salesperson read the split: %v", err)
	}
	// Still the whole price for the margin scheme.
	if got, _ := workshop.PurchaseOfUnit(ctx, pool, buyerScope(), p.StockUnitID); got.TaxPurchasePriceMinor != 8_500_000 {
		t.Errorf("the tax purchase price with a loan: %d", got.TaxPurchasePriceMinor)
	}
}

// Checks by role and module, and as facts that stay.
func TestChecksByRoleAndModule(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	p := buy(t, pool, workshop.StockFromPurchase)
	c := workshop.NewStockCheck{Kind: workshop.CheckDrivingBan, Result: workshop.CheckPass}
	for name, s := range map[string]access.Scope{"a salesperson": sales(), "the front desk": everything(advisor())} {
		if err := workshop.RecordStockCheck(ctx, pool, s, p.StockUnitID, c); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s recorded a check: %v", name, err)
		}
		if _, err := workshop.StockChecks(ctx, pool, s, p.StockUnitID); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s read the checks: %v", name, err)
		}
	}
	off := buyerScope()
	off.Modules = []access.Module{access.Workshop}
	if err := workshop.RecordStockCheck(ctx, pool, off, p.StockUnitID, c); !errors.Is(err, access.ErrModuleOff) {
		t.Errorf("recorded with car sales off: %v", err)
	}
	if _, err := workshop.StockChecks(ctx, pool, off, p.StockUnitID); err != nil {
		t.Errorf("read with car sales off: %v", err)
	}
	if err := workshop.RecordStockCheck(ctx, pool, buyerScope(), p.StockUnitID, workshop.NewStockCheck{Kind: "stolen", Result: "pass"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a check there is not: %v", err)
	}
	for _, stmt := range []string{`UPDATE stock_checks SET result = 'pass'`, `DELETE FROM stock_checks`,
		`DELETE FROM stock_check_settings`, `UPDATE purchase_loans SET amount_minor = 1`} {
		if err := asApp(ctx, pool, stmt); err == nil {
			t.Errorf("%q as the application was let through", stmt)
		}
	}
}

// A purchase payout pays the purchase it names, to its seller or to a
// lender on it: naming another car's purchase -- one whose checks passed --
// does not step round this car's. A consignment payout waits for its
// agreement (#166), and none can be opened before there is one.
func TestAPayoutPaysThePurchaseItNamesToThoseItOwes(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	sellerA := theSeller(t, pool)
	a := boughtCarFrom(t, pool, sellerA)
	other, err := workshop.CreateParty(ctx, pool, buyerScope(), workshop.NewParty{Kind: "private", Name: "Siv Säljare",
		Phone: "070-111 22 33", Role: workshop.PartySeller})
	if err != nil {
		t.Fatal(err)
	}
	b := boughtCarFrom(t, pool, other)
	passEveryCheck(t, pool, b.StockUnitID)
	for name, n := range map[string]workshop.NewHolding{
		"another car's purchase":  {PartyID: sellerA, Purpose: "purchase", SubjectKind: "purchase", SubjectID: b.ID},
		"a purchase there is not": {PartyID: sellerA, Purpose: "purchase", SubjectKind: "purchase", SubjectID: "99999999-9999-9999-9999-999999999999"},
		"no purchase at all":      {PartyID: sellerA, Purpose: "purchase"},
		"a consignment":           {PartyID: sellerA, Purpose: "consignment", SubjectKind: "consignment", SubjectID: a.StockUnitID},
	} {
		if _, err := workshop.CreatePayout(ctx, pool, buyerScope(), n); !errors.Is(err, workshop.ErrInvalid) && !errors.Is(err, workshop.ErrNotFound) {
			t.Errorf("a payout for %s: %v", name, err)
		}
	}
	lender, err := workshop.CreateParty(ctx, pool, buyerScope(), workshop.NewParty{Kind: "company", Name: "Bilfinans AB",
		OrgNumber: "556000-0000", Role: workshop.PartySupplier})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.CreatePayout(ctx, pool, buyerScope(), workshop.NewHolding{PartyID: lender, Purpose: "purchase",
		SubjectKind: "purchase", SubjectID: a.ID}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a payout to a lender with no loan on the purchase: %v", err)
	}
	if err := workshop.RecordPurchaseLoan(ctx, pool, buyerScope(), a.ID, workshop.PurchaseLoan{LenderPartyID: lender,
		AmountMinor: 1_000_000, SettlementReference: "Lösen 1"}); err != nil {
		t.Fatal(err)
	}
	for _, party := range []string{sellerA, lender} {
		if _, err := workshop.CreatePayout(ctx, pool, buyerScope(), workshop.NewHolding{PartyID: party, Purpose: "purchase",
			SubjectKind: "purchase", SubjectID: a.ID}); err != nil {
			t.Errorf("a payout of the purchase to %s: %v", party, err)
		}
	}
}

// A check being recorded and a payout made at the same moment: the payout
// waits for the check, and a check that failed keeps the money back.
func TestAPayoutWaitsForACheckBeingRecorded(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	p := buy(t, pool, workshop.StockFromPurchase)
	passEveryCheck(t, pool, p.StockUnitID)
	payout, err := workshop.CreatePayout(ctx, pool, buyerScope(), workshop.NewHolding{PartyID: p.SellerPartyID, Purpose: "purchase",
		SubjectKind: "purchase", SubjectID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	// A check being recorded holds the unit, as RecordStockCheck does,
	// and has not committed.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT set_config('app.current_shop', $1, true)`, shopID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM stock_units WHERE id = $1 FOR UPDATE`, p.StockUnitID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- payOut(pool, buyerScope(), payout, 100) }()
	select {
	case err := <-done:
		t.Fatalf("the payout did not wait for the check being recorded: %v", err)
	case <-time.After(700 * time.Millisecond):
	}
	if _, err := tx.Exec(ctx, `INSERT INTO stock_checks (shop_id, stock_unit_id, kind, result, source) VALUES ($1, $2, 'credit_purchase', 'fail', 'manual')`,
		shopID, p.StockUnitID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, workshop.ErrChecksOutstanding) {
		t.Errorf("paid out over the check that failed meanwhile: %v", err)
	}
}
