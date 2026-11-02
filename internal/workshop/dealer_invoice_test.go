package workshop_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A deal invoiced (#177), behind the G2 defaults.

// completedDealOn is a consumer's deal on unit, taxed as treatment, with
// winter tyres at 8 000 kr and a tow bar at 4 000 kr at 12 per cent,
// contracted.
func completedDealOn(t *testing.T, pool *pgxpool.Pool, unit, treatment string) workshop.Deal {
	t.Helper()
	ctx := context.Background()
	terms := consumerTerms(driver(t, pool))
	terms.TaxTreatment = treatment
	terms.Lines = append(terms.Lines, workshop.SaleLine{Description: "Dragkrok", QuantityMilli: 1000, UnitPriceMinor: 400_000,
		VATRateBasis: 1200})
	d, err := workshop.CreateDeal(ctx, pool, sales(), workshop.NewDeal{UnitID: unit, Terms: terms})
	if err != nil {
		t.Fatal(err)
	}
	doc, _, err := workshop.ProduceSalesContract(ctx, pool, sales(), d.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	signAll(t, pool, doc)
	done, err := workshop.CompleteDeal(ctx, pool, sales(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	return done
}

// A margin-scheme car with ordinary-VAT extras: the car at its gross price
// with no VAT and outside the bands, the extras in their bands, every
// printed figure adding up, the statement exactly as the law has it, and
// the scheme's VAT kept for the books but nowhere on the buyer's document.
// The terms are frozen once it is invoiced.
func TestAMarginSchemeSaleIsInvoicedWithTheStatementAndNoMargin(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	d := completedDealOn(t, pool, eligibleCar(t, pool), workshop.TaxMarginScheme)
	inv, err := workshop.IssueDealInvoice(ctx, pool, sales(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := workshop.IssueDealInvoice(ctx, pool, sales(), d.ID); err != nil || again.ID != inv.ID {
		t.Errorf("invoiced again: %+v, %v", again, err)
	}
	doc, err := workshop.DocumentByID(ctx, pool, everything(owner()), inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 125 000 kr car; 8 000 + 2 000 kr tyres; 4 000 + 480 kr tow bar.
	if len(doc.Lines) != 3 || !doc.Lines[0].MarginScheme || doc.Lines[0].NetMinor != 12_500_000 || doc.Lines[0].VATMinor != 0 {
		t.Fatalf("the lines: %+v", doc.Lines)
	}
	if doc.MarginSchemeStatement != "Vinstmarginalbeskattning för begagnade varor" {
		t.Errorf("the statement: %q", doc.MarginSchemeStatement)
	}
	if len(doc.Bands) != 2 || doc.Bands[0].RateBasisPoints != 1200 || doc.Bands[1].RateBasisPoints != 2500 {
		t.Errorf("the bands: %+v; want the extras' alone", doc.Bands)
	}
	if doc.MarginSchemeMinor() != 12_500_000 || doc.OrdinaryNetMinor() != 1_200_000 || doc.VATMinor != 248_000 ||
		doc.GrossMinor != 12_500_000+1_200_000+248_000 {
		t.Errorf("the totals: scheme %d, net %d, VAT %d, total %d", doc.MarginSchemeMinor(), doc.OrdinaryNetMinor(), doc.VATMinor, doc.GrossMinor)
	}
	if doc.MarginSchemeMinor()+doc.OrdinaryNetMinor()+doc.VATMinor != doc.GrossMinor {
		t.Error("the printed figures do not add up to the total")
	}
	var schemeVAT int64
	if err := pool.QueryRow(ctx, `SELECT margin_scheme_vat_minor FROM invoices WHERE id = $1`, inv.ID).Scan(&schemeVAT); err != nil || schemeVAT != 800_000 {
		t.Errorf("the scheme's VAT kept for the books: %d (%v); want 8 000 kr", schemeVAT, err)
	}
	terms := consumerTerms(d.Buyers[0].PartyID)
	terms.TaxTreatment = workshop.TaxOrdinary
	if _, err := workshop.ReviseDeal(ctx, pool, sales(), d.ID, d.Version, terms); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("the treatment changed after the invoice: %v", err)
	}
	from, to := time.Now().AddDate(0, 0, -1), time.Now().AddDate(0, 0, 1)
	if _, _, err := workshop.ExportAccounting(ctx, pool, everything(owner()), from, to, false); !errors.Is(err, workshop.ErrCarSalePostings) {
		t.Errorf("a car sale exported before the accountant has said how: %v", err)
	}

	// Credited, the terms may change again and the sale be invoiced anew.
	if _, err := workshop.IssueCredit(ctx, pool, everything(owner()), inv.ID, workshop.CreditRequest{Reason: "Fel pris"}); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.ReviseDeal(ctx, pool, sales(), d.ID, d.Version, terms); err != nil {
		t.Errorf("revised after the invoice was credited: %v", err)
	}
}

// Under ordinary VAT the car is a line at 25 per cent, and the invoice
// says nothing of the margin scheme. A deal not complete is not invoiced,
// and the workshop's roles invoice no deal.
func TestAnOrdinarySaleIsInvoicedAtTwentyFivePerCent(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	d := completedDealOn(t, pool, carForSale(t, pool), workshop.TaxOrdinary)
	for name, s := range map[string]access.Scope{"the front desk": everything(advisor()), "a technician": everything(technician())} {
		if _, err := workshop.IssueDealInvoice(ctx, pool, s, d.ID); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s invoiced a deal: %v", name, err)
		}
	}
	inv, err := workshop.IssueDealInvoice(ctx, pool, sales(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := workshop.DocumentByID(ctx, pool, everything(owner()), inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	car := doc.Lines[0]
	if car.MarginScheme || car.VATRateBasis != 2500 || car.NetMinor != 10_000_000 || car.VATMinor != 2_500_000 || doc.MarginSchemeStatement != "" {
		t.Errorf("the car under ordinary VAT: %+v, statement %q", car, doc.MarginSchemeStatement)
	}

	open, err := workshop.CreateDeal(ctx, pool, sales(), workshop.NewDeal{UnitID: carForSaleAgain(t, pool), Terms: consumerTerms(driver(t, pool))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.IssueDealInvoice(ctx, pool, sales(), open.ID); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a deal not complete invoiced: %v", err)
	}
}

// carForSaleAgain is another car of its own, for sale.
func carForSaleAgain(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	p := boughtCar(t, pool)
	moveStock(t, pool, p.StockUnitID, workshop.StockForSale)
	return p.StockUnitID
}

// An invoice credited only in part still stands: the terms stay frozen,
// and invoicing again is the same invoice, never a second for the car.
// Credited in full, it no longer stands.
func TestAPartlyCreditedSaleInvoiceStillStands(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	d := completedDealOn(t, pool, carForSale(t, pool), workshop.TaxOrdinary)
	inv, err := workshop.IssueDealInvoice(ctx, pool, sales(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The tow bar credited, the car not.
	if _, err := workshop.IssueCredit(ctx, pool, everything(owner()), inv.ID, workshop.CreditRequest{
		Lines: []workshop.CreditLine{{Position: 3}}, Reason: "Dragkroken monterades inte"}); err != nil {
		t.Fatal(err)
	}
	terms := consumerTerms(d.Buyers[0].PartyID)
	terms.TaxTreatment = workshop.TaxOrdinary
	if _, err := workshop.ReviseDeal(ctx, pool, sales(), d.ID, d.Version, terms); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("revised with the invoice credited in part: %v", err)
	}
	if again, err := workshop.IssueDealInvoice(ctx, pool, sales(), d.ID); err != nil || again.ID != inv.ID {
		t.Errorf("invoiced again with the invoice credited in part: %+v, %v", again, err)
	}
	if _, err := workshop.IssueCredit(ctx, pool, everything(owner()), inv.ID, workshop.CreditRequest{Reason: "Köpet gick tillbaka"}); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.ReviseDeal(ctx, pool, sales(), d.ID, d.Version, terms); err != nil {
		t.Errorf("revised with the invoice credited in full: %v", err)
	}
}

// A deal with an invoice standing is not cancelled: the car would be on
// sale again with a live invoice, and could be invoiced twice. Credited,
// it is.
func TestAnInvoicedDealIsNotCancelledUntilCredited(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	d := completedDealOn(t, pool, carForSale(t, pool), workshop.TaxOrdinary)
	inv, err := workshop.IssueDealInvoice(ctx, pool, sales(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.CancelDeal(ctx, pool, sales(), d.ID, "Köparen ångrade sig"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("an invoiced deal cancelled: %v", err)
	}
	if u, _ := workshop.StockUnitByID(ctx, pool, sales(), d.StockUnitID); u.Status != workshop.StockSold {
		t.Errorf("the car after the refused cancel: %s", u.Status)
	}
	if _, err := workshop.IssueCredit(ctx, pool, everything(owner()), inv.ID, workshop.CreditRequest{Reason: "Köpet gick tillbaka"}); err != nil {
		t.Fatal(err)
	}
	if err := workshop.CancelDeal(ctx, pool, sales(), d.ID, "Köparen ångrade sig"); err != nil {
		t.Errorf("a credited deal cancelled: %v", err)
	}
}

// A car under ordinary VAT is invoiced at exactly its price: the VAT is
// what is inside the price, and the net the rest -- not a net from which
// the VAT is worked out again and rounds a öre the other way.
func TestAnOrdinaryCarIsInvoicedAtExactlyItsPrice(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	kim := driver(t, pool)
	for _, price := range []int64{102, 12_500_003, 9_999_999} {
		unit := carForSaleAgain(t, pool)
		terms := workshop.SaleTerms{Buyers: []string{kim}, BuyerKind: workshop.BuyerConsumer, PriceMinor: price, TaxTreatment: workshop.TaxOrdinary}
		d, err := workshop.CreateDeal(ctx, pool, sales(), workshop.NewDeal{UnitID: unit, Terms: terms})
		if err != nil {
			t.Fatal(err)
		}
		doc, _, err := workshop.ProduceSalesContract(ctx, pool, sales(), d.ID, "", "")
		if err != nil {
			t.Fatal(err)
		}
		signAll(t, pool, doc)
		if _, err := workshop.CompleteDeal(ctx, pool, sales(), d.ID); err != nil {
			t.Fatal(err)
		}
		inv, err := workshop.IssueDealInvoice(ctx, pool, sales(), d.ID)
		if err != nil {
			t.Fatal(err)
		}
		if inv.GrossMinor != price {
			t.Errorf("a car priced %d öre was invoiced at %d (net %d, VAT %d)", price, inv.GrossMinor, inv.NetMinor, inv.VATMinor)
		}
	}
}

// schemeOf is what an invoice keeps for the books: its margin-scheme VAT
// and its statement.
func schemeOf(t *testing.T, pool *pgxpool.Pool, id string) (int64, string) {
	t.Helper()
	var vat *int64
	var statement *string
	if err := pool.QueryRow(context.Background(), `SELECT margin_scheme_vat_minor, margin_scheme_statement FROM invoices WHERE id = $1`,
		id).Scan(&vat, &statement); err != nil {
		t.Fatal(err)
	}
	var v int64
	var s string
	if vat != nil {
		v = *vat
	}
	if statement != nil {
		s = *statement
	}
	return v, s
}

// A margin-scheme sale credited takes its VAT back with it, in proportion
// to the car credited and to the öre over every credit, and says how it is
// taxed; credited in full and billed again to the right buyer, the new
// invoice keeps the VAT and the statement.
func TestAMarginSchemeCreditAndRebillKeepTheSchemesVAT(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	d := completedDealOn(t, pool, eligibleCar(t, pool), workshop.TaxMarginScheme)
	inv, err := workshop.IssueDealInvoice(ctx, pool, sales(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 125 000 kr against 85 000: 8 000 kr of VAT in a 40 000 kr margin.
	// 10 000 kr off the price takes 10 000 off the margin: 2 000 kr back.
	part, err := workshop.IssueCredit(ctx, pool, everything(owner()), inv.ID, workshop.CreditRequest{
		Lines: []workshop.CreditLine{{Position: 1, QuantityMilli: 80}}, Reason: "Prisavdrag 10 000 kr"})
	if err != nil {
		t.Fatal(err)
	}
	if vat, statement := schemeOf(t, pool, part.ID); vat != -200_000 || statement != workshop.MarginSchemeStatement {
		t.Errorf("10 000 kr off the car: VAT %d, statement %q; want -2 000 kr and the statement", vat, statement)
	}
	rest, err := workshop.IssueCredit(ctx, pool, everything(owner()), inv.ID, workshop.CreditRequest{Reason: "Resten"})
	if err != nil {
		t.Fatal(err)
	}
	if vat, _ := schemeOf(t, pool, rest.ID); vat != -600_000 {
		t.Errorf("the rest credited: VAT %d; want -6 000 kr, so 8 000 kr back over both", vat)
	}

	// Billed again to another buyer is not done by rebilling: a car sale is
	// invoiced again through its deal, whose terms say who buys.
	other, err := workshop.CreateParty(ctx, pool, everything(owner()), workshop.NewParty{Kind: "private", Name: "Lo Letare",
		Phone: "0731234567", Role: workshop.PartyBuyer})
	if err != nil {
		t.Fatal(err)
	}
	customer, err := workshop.CustomerForParty(ctx, pool, everything(owner()), other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.RebillTo(ctx, pool, everything(owner()), inv.ID, workshop.RebillRequest{CustomerID: customer,
		Reason: "Fel köpare"}); !errors.Is(err, workshop.ErrInvalid) || !strings.Contains(err.Error(), "through its deal") {
		t.Errorf("a car sale rebilled past its deal: %v", err)
	}
}

// A sale that comes to nothing is not invoiced: a 0 kr invoice could never
// be credited, and would stand for ever or not at all.
func TestASaleOfNothingIsNotInvoiced(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	kim := driver(t, pool)
	terms := workshop.SaleTerms{Buyers: []string{kim}, BuyerKind: workshop.BuyerBusiness, PriceMinor: 0, TaxTreatment: workshop.TaxOrdinary}
	d, err := workshop.CreateDeal(ctx, pool, sales(), workshop.NewDeal{UnitID: carForSale(t, pool), Terms: terms})
	if err != nil {
		t.Fatal(err)
	}
	doc, _, err := workshop.ProduceSalesContract(ctx, pool, sales(), d.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	signAll(t, pool, doc)
	if _, err := workshop.CompleteDeal(ctx, pool, sales(), d.ID); err != nil {
		t.Fatal(err)
	}
	if inv, err := workshop.IssueDealInvoice(ctx, pool, sales(), d.ID); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a sale of nothing invoiced: %+v, %v", inv, err)
	}
}

// A credit that takes the price below what the car was bought for takes
// back all of its margin-scheme VAT: there is no margin left.
func TestACreditBelowThePurchasePriceTakesBackAllTheSchemesVAT(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	d := completedDealOn(t, pool, eligibleCar(t, pool), workshop.TaxMarginScheme)
	inv, err := workshop.IssueDealInvoice(ctx, pool, sales(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 70 000 kr off a 125 000 kr car bought for 85 000: no margin left.
	c, err := workshop.IssueCredit(ctx, pool, everything(owner()), inv.ID, workshop.CreditRequest{
		Lines: []workshop.CreditLine{{Position: 1, QuantityMilli: 560}}, Reason: "Stort prisavdrag"})
	if err != nil {
		t.Fatal(err)
	}
	if vat, _ := schemeOf(t, pool, c.ID); vat != -800_000 {
		t.Errorf("70 000 kr off: VAT %d; want all 8 000 kr back", vat)
	}
}

// An adjustment on a margin-scheme invoice is refused: it would carry
// ordinary VAT and take back none of the scheme's. The car's line is
// credited instead.
func TestAnAdjustmentOnAMarginSchemeInvoiceIsRefused(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	d := completedDealOn(t, pool, eligibleCar(t, pool), workshop.TaxMarginScheme)
	inv, err := workshop.IssueDealInvoice(ctx, pool, sales(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.IssueCredit(ctx, pool, everything(owner()), inv.ID, workshop.CreditRequest{Reason: "Rabatt",
		Adjustments: []workshop.Adjustment{{Description: "Rabatt", NetMinor: 500_000, VATRateBasis: 2500}}}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("an adjustment on a margin-scheme invoice: %v", err)
	}
}
