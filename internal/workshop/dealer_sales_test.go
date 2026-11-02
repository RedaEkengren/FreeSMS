package workshop_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/money"
	"github.com/RedaEkengren/FreeSMS/internal/testsupport"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Deals and their contracts (#175).

func consumerTerms(buyer string) workshop.SaleTerms {
	km := int64(123_600)
	on := time.Now().AddDate(0, 0, 7)
	return workshop.SaleTerms{Buyers: []string{buyer}, BuyerKind: workshop.BuyerConsumer, PriceMinor: 12_500_000,
		Warranty: "Tre månader på motor och växellåda", DeliveryOn: &on, DeliveryPlace: "Hos oss", OdometerKm: &km,
		PaymentTerms: "Kontant vid leverans",
		Lines:        []workshop.SaleLine{{Description: "Vinterhjul", QuantityMilli: 1000, UnitPriceMinor: 800_000, VATRateBasis: 2500}}}
}

// signAll records every party of a document as having signed it in
// person.
func signAll(t *testing.T, pool *pgxpool.Pool, doc string) {
	t.Helper()
	ctx := context.Background()
	d, err := workshop.DocumentRecordByID(ctx, pool, sales(), doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range d.Parties {
		if err := workshop.RecordSignature(ctx, pool, sales(), doc, p.ID, workshop.SignatureEvidence{
			Method: workshop.SignedInPerson, Hash: d.Hash}); err != nil {
			t.Fatal(err)
		}
	}
}

func contractTemplate(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	// None chosen: the shipped contract, made with the first contract.
	return ""
}

// A consumer's deal goes from terms to a completed contract: how the sale
// is taxed is chosen on the deal and the margin scheme only for a car its
// purchase makes eligible; the contract is made from the terms as they
// stand, every buyer signs it, and every known defect is accepted on its
// own. Completed, the car is sold and the lead is won. Revised after, it
// needs a new contract, and both stay.
func TestADealIsContractedWithEachDefectAcceptedOnItsOwn(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	unit := carForSale(t, pool)
	kim := driver(t, pool)
	lead, err := workshop.CreateLead(ctx, pool, sales(), workshop.NewLead{PartyID: kim, StockUnitID: unit, Channel: "walk_in"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := workshop.CreateDeal(ctx, pool, sales(), workshop.NewDeal{UnitID: unit, LeadID: lead.ID, Terms: consumerTerms(kim), Key: "deal-1"})
	if err != nil {
		t.Fatal(err)
	}
	if d.State != workshop.DealDraft || d.Version != 1 || d.TotalMinor() != 12_500_000+1_000_000 || len(d.Buyers) != 1 {
		t.Errorf("the new deal: %+v", d)
	}
	if again, err := workshop.CreateDeal(ctx, pool, sales(), workshop.NewDeal{UnitID: unit, LeadID: lead.ID, Terms: consumerTerms(kim), Key: "deal-1"}); err != nil || again.ID != d.ID {
		t.Errorf("the same request again: %+v, %v", again, err)
	}
	if _, err := workshop.CreateDeal(ctx, pool, sales(), workshop.NewDeal{UnitID: unit, LeadID: "", Terms: consumerTerms(kim), Key: "deal-2"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a second deal on the car: %v", err)
	}
	if l, _ := workshop.LeadByID(ctx, pool, sales(), lead.ID); l.Status != workshop.LeadOfferMade || l.DealID != d.ID {
		t.Errorf("the lead with a deal: %s, %s", l.Status, l.DealID)
	}
	defect, err := workshop.AddDefect(ctx, pool, sales(), d.ID, "Repa på motorhuven, 10 cm", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.CompleteDeal(ctx, pool, sales(), d.ID); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("completed with nothing done: %v", err)
	}

	// The car was bought from a private seller with no evidence: not
	// eligible, so the margin scheme is refused and ordinary VAT chosen.
	scheme := consumerTerms(kim)
	scheme.TaxTreatment = workshop.TaxMarginScheme
	if _, err := workshop.ReviseDeal(ctx, pool, sales(), d.ID, 1, scheme); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("the margin scheme on a car not eligible: %v", err)
	}
	ordinary := consumerTerms(kim)
	ordinary.TaxTreatment = workshop.TaxOrdinary
	if _, err := workshop.ReviseDeal(ctx, pool, sales(), d.ID, 1, ordinary); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.ReviseDeal(ctx, pool, sales(), d.ID, 1, ordinary); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("revised over a version already revised: %v", err)
	}

	tpl := contractTemplate(t, pool)
	doc, warnings, err := workshop.ProduceSalesContract(ctx, pool, sales(), d.ID, tpl, "")
	if err != nil || len(warnings) != 0 {
		t.Fatalf("the contract: %s, %v, %v", doc, warnings, err)
	}
	contract, _ := workshop.DocumentRecordByID(ctx, pool, sales(), doc)
	for _, want := range []string{"Repa på motorhuven", "Vinterhjul", "Allmänna reklamationsnämnden", "Kim Köpare"} {
		if !strings.Contains(contract.Body, want) {
			t.Errorf("the contract does not say %q:\n%s", want, contract.Body)
		}
	}
	if strings.Contains(contract.Body, money.Format(8_500_000)) {
		t.Errorf("the contract shows what the car cost:\n%s", contract.Body)
	}
	signAll(t, pool, doc)
	if _, err := workshop.CompleteDeal(ctx, pool, sales(), d.ID); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("completed with a defect not accepted: %v", err)
	}
	if err := workshop.AnswerDefect(ctx, pool, sales(), defect, kim, true, "paper", ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("accepted on paper with no scan: %v", err)
	}
	if err := workshop.AnswerDefect(ctx, pool, sales(), defect, kim, true, "in_person", ""); err != nil {
		t.Fatal(err)
	}
	done, err := workshop.CompleteDeal(ctx, pool, sales(), d.ID)
	if err != nil || done.State != workshop.DealContracted || !done.Completed() {
		t.Fatalf("completed: %+v, %v", done, err)
	}
	if u, _ := workshop.StockUnitByID(ctx, pool, sales(), unit); u.Status != workshop.StockSold || u.VehicleSaleID != d.ID {
		t.Errorf("the car after the contract: %+v", u)
	}
	if l, _ := workshop.LeadByID(ctx, pool, sales(), lead.ID); l.Status != workshop.LeadWon {
		t.Errorf("the lead after the contract: %s", l.Status)
	}

	// The delivery moved after signing: a new version, a new contract
	// superseding the first, signed and completed again.
	later := ordinary
	on := time.Now().AddDate(0, 0, 14)
	later.DeliveryOn = &on
	revised, err := workshop.ReviseDeal(ctx, pool, sales(), d.ID, 2, later)
	if err != nil || revised.Completed() || len(revised.Missing()) == 0 {
		t.Fatalf("revised after the contract: %+v, %v", revised, err)
	}
	second, _, err := workshop.ProduceSalesContract(ctx, pool, sales(), d.ID, tpl, doc)
	if err != nil || second == doc {
		t.Fatalf("the second contract: %s, %v", second, err)
	}
	signAll(t, pool, second)
	if again, err := workshop.CompleteDeal(ctx, pool, sales(), d.ID); err != nil || !again.Completed() {
		t.Errorf("completed again: %+v, %v", again, err)
	}
	if first, err := workshop.DocumentRecordByID(ctx, pool, sales(), doc); err != nil || first.Body == "" {
		t.Errorf("the first contract after the second: %+v, %v", first, err)
	}
}

// A consumer who refuses a defect has no contract; a business buyer's
// answers are recorded but not required.
func TestARefusedDefectStopsAConsumersContractButNotABusinesss(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	for _, kind := range []string{workshop.BuyerConsumer, workshop.BuyerBusiness} {
		t.Run(kind, func(t *testing.T) {
			p := boughtCar(t, pool)
			moveStock(t, pool, p.StockUnitID, workshop.StockForSale)
			kim := driver(t, pool)
			terms := consumerTerms(kim)
			terms.BuyerKind, terms.TaxTreatment = kind, workshop.TaxOrdinary
			d, err := workshop.CreateDeal(ctx, pool, sales(), workshop.NewDeal{UnitID: p.StockUnitID, Terms: terms})
			if err != nil {
				t.Fatal(err)
			}
			defect, err := workshop.AddDefect(ctx, pool, sales(), d.ID, "Rost i tröskeln", "")
			if err != nil {
				t.Fatal(err)
			}
			if err := workshop.AnswerDefect(ctx, pool, sales(), defect, kim, false, "in_person", ""); err != nil {
				t.Fatal(err)
			}
			doc, _, err := workshop.ProduceSalesContract(ctx, pool, sales(), d.ID, contractTemplate(t, pool), "")
			if err != nil {
				t.Fatal(err)
			}
			signAll(t, pool, doc)
			_, err = workshop.CompleteDeal(ctx, pool, sales(), d.ID)
			if kind == workshop.BuyerConsumer && !errors.Is(err, workshop.ErrInvalid) {
				t.Errorf("a consumer's contract with a refused defect: %v", err)
			}
			if kind == workshop.BuyerBusiness && err != nil {
				t.Errorf("a business's contract: %v", err)
			}
		})
	}
}

// A consumer contract whose text waives defects "as is" is warned of: it
// exempts the dealer from nothing.
func TestAnAsIsClauseInAConsumerContractIsWarnedOf(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	unit := carForSale(t, pool)
	kim := driver(t, pool)
	tpl, err := workshop.SaveDocumentTemplate(ctx, pool, everything(owner()), "", workshop.DocSalesContract, "Eget avtal",
		"Bilen säljs i befintligt skick till {buyers}.")
	if err != nil {
		t.Fatal(err)
	}
	d, err := workshop.CreateDeal(ctx, pool, sales(), workshop.NewDeal{UnitID: unit, LeadID: "", Terms: consumerTerms(kim), Key: ""})
	if err != nil {
		t.Fatal(err)
	}
	if _, warnings, err := workshop.ProduceSalesContract(ctx, pool, sales(), d.ID, tpl.VersionID, ""); err != nil ||
		len(warnings) != 1 || warnings[0] != "befintligt skick" {
		t.Errorf("the warnings: %v, %v", warnings, err)
	}
	if w := workshop.SalesContractWarnings("Bilen säljs i befintligt skick.", workshop.BuyerBusiness); len(w) != 0 {
		t.Errorf("a business contract warned of: %v", w)
	}
}

// Deals are made by whoever sells cars, with car sales on; read with it
// off. A deal cancelled after its contract puts the car back for sale,
// and the car may then have a new deal. Two deals on one car at once: one.
func TestDealsByRoleModuleAndCancellation(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	unit := carForSale(t, pool)
	kim := driver(t, pool)
	for name, s := range map[string]access.Scope{"a buyer": buyerScope(), "the front desk": everything(advisor()), "a technician": everything(technician())} {
		got, err := workshop.CreateDeal(ctx, pool, s, workshop.NewDeal{UnitID: unit, LeadID: "", Terms: consumerTerms(kim), Key: ""})
		if !errors.Is(err, access.ErrForbidden) && !errors.Is(err, workshop.ErrNotFound) {
			t.Errorf("%s made a deal: %v", name, err)
		}
		if handed := testsupport.Handed(got, "ID", "Buyers"); len(handed) != 0 {
			t.Errorf("%s was handed %v", name, handed)
		}
	}
	off := sales()
	off.Modules = []access.Module{access.Workshop}
	if _, err := workshop.CreateDeal(ctx, pool, off, workshop.NewDeal{UnitID: unit, LeadID: "", Terms: consumerTerms(kim), Key: ""}); !errors.Is(err, access.ErrModuleOff) {
		t.Errorf("a deal with car sales off: %v", err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = workshop.CreateDeal(ctx, pool, sales(), workshop.NewDeal{UnitID: unit, LeadID: "", Terms: consumerTerms(kim), Key: ""})
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("%d of 4 deals on one car stood: %v", ok, errs)
	}
	deals, err := workshop.Deals(ctx, pool, off, false)
	if err != nil || len(deals) != 1 {
		t.Fatalf("the deals, read with car sales off: %d, %v", len(deals), err)
	}
	d := deals[0]
	terms := consumerTerms(kim)
	terms.TaxTreatment = workshop.TaxOrdinary
	d, err = workshop.ReviseDeal(ctx, pool, sales(), d.ID, d.Version, terms)
	if err != nil {
		t.Fatal(err)
	}
	doc, _, err := workshop.ProduceSalesContract(ctx, pool, sales(), d.ID, contractTemplate(t, pool), "")
	if err != nil {
		t.Fatal(err)
	}
	signAll(t, pool, doc)
	if _, err := workshop.CompleteDeal(ctx, pool, sales(), d.ID); err != nil {
		t.Fatal(err)
	}
	if err := workshop.CancelDeal(ctx, pool, sales(), d.ID, ""); !errors.Is(err, workshop.ErrDealNoReason) {
		t.Errorf("cancelled without why: %v", err)
	}
	if err := workshop.CancelDeal(ctx, pool, sales(), d.ID, "Köparen fick inte lån"); err != nil {
		t.Fatal(err)
	}
	if u, _ := workshop.StockUnitByID(ctx, pool, sales(), unit); u.Status != workshop.StockForSale {
		t.Errorf("the car after the deal was cancelled: %s", u.Status)
	}
	if _, err := workshop.CreateDeal(ctx, pool, sales(), workshop.NewDeal{UnitID: unit, LeadID: "", Terms: consumerTerms(kim), Key: ""}); err != nil {
		t.Errorf("a new deal after the first was cancelled: %v", err)
	}
	for _, stmt := range []string{`DELETE FROM vehicle_sales`, `UPDATE vehicle_sale_terms SET price_minor = 1`,
		`DELETE FROM vehicle_sale_defect_answers`, `UPDATE vehicle_sale_events SET kind = 'cancelled'`} {
		if err := asApp(ctx, pool, stmt); err == nil {
			t.Errorf("%q as the application was let through", stmt)
		}
	}
}

// A contract made again from a page that did not show the newest one is
// refused as made meanwhile: the person reads the deal again.
func TestAContractMadeOverAnotherIsRefused(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	unit := carForSale(t, pool)
	d, err := workshop.CreateDeal(ctx, pool, sales(), workshop.NewDeal{UnitID: unit, Terms: consumerTerms(driver(t, pool))})
	if err != nil {
		t.Fatal(err)
	}
	tpl := contractTemplate(t, pool)
	if _, _, err := workshop.ProduceSalesContract(ctx, pool, sales(), d.ID, tpl, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := workshop.ProduceSalesContract(ctx, pool, sales(), d.ID, tpl, ""); !errors.Is(err, workshop.ErrMadeMeanwhile) {
		t.Errorf("made again from a page that showed none: %v", err)
	}
}

// A buyer who is not a party yet is made with the deal: sent twice, one
// person; refused, none.
func TestANewBuyerIsMadeWithTheDeal(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	unit := carForSale(t, pool)
	parties := func() (n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM parties`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := parties()
	terms := consumerTerms("")
	terms.Buyers = nil
	n := workshop.NewDeal{UnitID: unit, Terms: terms, NewBuyer: &workshop.NewParty{Name: "Lo Letare", Phone: "0731234567"}, Key: "nb-1"}
	d, err := workshop.CreateDeal(ctx, pool, sales(), n)
	if err != nil || len(d.Buyers) != 1 || d.Buyers[0].Name != "Lo Letare" {
		t.Fatalf("the deal with a new buyer: %+v, %v", d, err)
	}
	if again, err := workshop.CreateDeal(ctx, pool, sales(), n); err != nil || again.ID != d.ID {
		t.Errorf("sent again: %+v, %v", again, err)
	}
	n.Key = "nb-2"
	if _, err := workshop.CreateDeal(ctx, pool, sales(), n); !errors.Is(err, workshop.ErrInvalid) {
		t.Fatalf("a second deal on the car: %v", err)
	}
	if got := parties(); got != before+1 {
		t.Errorf("%d people made for one new buyer", got-before)
	}
}

// eligibleCar is a car in stock for sale, bought from a private seller for
// 85 000 kr, with the purchase agreement as evidence: eligible for the
// margin scheme.
func eligibleCar(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	p := buy(t, pool, workshop.StockFromPurchase)
	if _, err := workshop.CorrectVATEligibility(context.Background(), pool, buyerScope(), p.ID, workshop.SellerPrivate,
		[]string{contractFor(t, pool, p.ID)}, "Köpeavtalet upprättat"); err != nil {
		t.Fatal(err)
	}
	moveStock(t, pool, p.StockUnitID, workshop.StockForSale)
	return p.StockUnitID
}

// A deal sold under the margin scheme records the eligibility it relied
// on, and its VAT is that car's margin alone: the price against the tax
// purchase price, never the car's costs. Ordinary VAT is a fifth of the
// price. Only whoever may see what a car cost reads it.
func TestADealsTaxIsItsCarsMarginAlone(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	unit := eligibleCar(t, pool)
	kim := driver(t, pool)
	terms := consumerTerms(kim)
	terms.TaxTreatment = workshop.TaxMarginScheme
	d, err := workshop.CreateDeal(ctx, pool, sales(), workshop.NewDeal{UnitID: unit, Terms: terms})
	if err != nil {
		t.Fatal(err)
	}
	if d.EligibilityID == "" {
		t.Errorf("the deal does not say which eligibility it relied on: %+v", d)
	}
	tax, err := workshop.SaleTaxOfDeal(ctx, pool, buyerScope(), d.ID)
	// 125 000 kr against 85 000 kr: a margin of 40 000 kr, 8 000 kr of VAT.
	if err != nil || tax.Treatment != workshop.TaxMarginScheme || tax.MarginMinor != 4_000_000 || tax.VATMinor != 800_000 ||
		tax.BaseMinor != 3_200_000 {
		t.Fatalf("the margin-scheme tax: %+v, %v", tax, err)
	}
	if _, err := workshop.RecordStockCost(ctx, pool, buyerScope(), unit, cost(workshop.CostReconditioning, 1_500_000)); err != nil {
		t.Fatal(err)
	}
	if again, _ := workshop.SaleTaxOfDeal(ctx, pool, buyerScope(), d.ID); again != tax {
		t.Errorf("the tax moved with a cost: %+v, then %+v", tax, again)
	}
	got, err := workshop.SaleTaxOfDeal(ctx, pool, sales(), d.ID)
	if !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a salesperson read the margin: %v", err)
	}
	if handed := testsupport.Handed(got, "MarginMinor", "VATMinor", "BaseMinor"); len(handed) != 0 {
		t.Errorf("a salesperson was handed %v", handed)
	}
	terms.TaxTreatment = workshop.TaxOrdinary
	if _, err := workshop.ReviseDeal(ctx, pool, sales(), d.ID, 1, terms); err != nil {
		t.Fatal(err)
	}
	if tax, err := workshop.SaleTaxOfDeal(ctx, pool, buyerScope(), d.ID); err != nil || tax.VATMinor != 2_500_000 || tax.MarginMinor != 0 {
		t.Errorf("the ordinary tax: %+v, %v", tax, err)
	}
}

// The database itself refuses the margin scheme for a car its purchase
// does not make eligible, whatever writes the terms.
func TestTheDatabaseRefusesTheMarginSchemeForACarNotEligible(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	unit := carForSale(t, pool)
	d, err := workshop.CreateDeal(ctx, pool, sales(), workshop.NewDeal{UnitID: unit, Terms: consumerTerms(driver(t, pool))})
	if err != nil {
		t.Fatal(err)
	}
	if err := asApp(ctx, pool, `INSERT INTO vehicle_sale_terms (shop_id, sale_id, version, buyer_kind, price_minor, sale_tax_treatment)
		VALUES ('`+shopID+`', '`+d.ID+`', 2, 'consumer', 1, 'margin_scheme')`); err == nil {
		t.Error("the margin scheme was written for a car not eligible")
	}
}

// completedDeal is a consumer's deal on a car of its own, contracted with
// one defect accepted.
func completedDeal(t *testing.T, pool *pgxpool.Pool) workshop.Deal {
	t.Helper()
	ctx := context.Background()
	p := boughtCar(t, pool)
	moveStock(t, pool, p.StockUnitID, workshop.StockForSale)
	kim := driver(t, pool)
	terms := consumerTerms(kim)
	terms.TaxTreatment = workshop.TaxOrdinary
	d, err := workshop.CreateDeal(ctx, pool, sales(), workshop.NewDeal{UnitID: p.StockUnitID, Terms: terms})
	if err != nil {
		t.Fatal(err)
	}
	defect, err := workshop.AddDefect(ctx, pool, sales(), d.ID, "Repa i lacken", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.AnswerDefect(ctx, pool, sales(), defect, kim, true, "in_person", ""); err != nil {
		t.Fatal(err)
	}
	doc, _, err := workshop.ProduceSalesContract(ctx, pool, sales(), d.ID, contractTemplate(t, pool), "")
	if err != nil {
		t.Fatal(err)
	}
	signAll(t, pool, doc)
	done, err := workshop.CompleteDeal(ctx, pool, sales(), d.ID)
	if err != nil || !done.Completed() {
		t.Fatalf("completed: %+v, %v", done, err)
	}
	return done
}

// A completed contract is not replaced while it still states everything:
// making it again is refused, and the signed one stays the contract.
func TestACompletedContractIsNotReplaced(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	d := completedDeal(t, pool)
	if _, _, err := workshop.ProduceSalesContract(ctx, pool, sales(), d.ID, contractTemplate(t, pool), d.ContractID); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a completed contract made again: %v", err)
	}
	if got, _ := workshop.DealByID(ctx, pool, sales(), d.ID); got.ContractID != d.ContractID || !got.Completed() {
		t.Errorf("the deal after: %+v", got)
	}
}

// A defect found after the contract was made makes it out of date: the
// signed contract does not state it, so the deal needs a new contract,
// signed, with the defect accepted, before it is complete again.
func TestADefectAddedAfterTheContractMakesItOutOfDate(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	d := completedDeal(t, pool)
	kim := d.Buyers[0].PartyID
	late, err := workshop.AddDefect(ctx, pool, sales(), d.ID, "Stenskott i vindrutan", "")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := workshop.DealByID(ctx, pool, sales(), d.ID)
	if got.Completed() || len(got.Missing()) == 0 {
		t.Fatalf("the deal with a defect its contract does not state: completed %v, missing %v", got.Completed(), got.Missing())
	}
	second, _, err := workshop.ProduceSalesContract(ctx, pool, sales(), d.ID, contractTemplate(t, pool), d.ContractID)
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := workshop.DocumentRecordByID(ctx, pool, sales(), second)
	if !strings.Contains(doc.Body, "Stenskott i vindrutan") {
		t.Errorf("the new contract does not state the defect:\n%s", doc.Body)
	}
	signAll(t, pool, second)
	if _, err := workshop.CompleteDeal(ctx, pool, sales(), d.ID); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("completed with the new defect not accepted: %v", err)
	}
	if err := workshop.AnswerDefect(ctx, pool, sales(), late, kim, true, "in_person", ""); err != nil {
		t.Fatal(err)
	}
	if again, err := workshop.CompleteDeal(ctx, pool, sales(), d.ID); err != nil || !again.Completed() || again.ContractID != second {
		t.Errorf("completed again: %+v, %v", again, err)
	}
}

// A salesperson sees and works their own deals, as with leads; another's
// are not found, asked for directly or acted on, and nothing of them is
// handed over. Whoever runs the shop sees every deal.
func TestASalespersonSeesTheirOwnDeals(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	anna := salesperson(t, pool, "Anna Säljare", "anna.s@example.test")
	bo := salesperson(t, pool, "Bo Säljare", "bo.s@example.test")
	unit := carForSale(t, pool)
	d, err := workshop.CreateDeal(ctx, pool, anna, workshop.NewDeal{UnitID: unit, Terms: consumerTerms(driver(t, pool))})
	if err != nil {
		t.Fatal(err)
	}
	got, err := workshop.DealByID(ctx, pool, bo, d.ID)
	if !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("another's deal, asked for directly: %v", err)
	}
	if handed := testsupport.Handed(got, "ID", "Buyers", "Terms"); len(handed) != 0 {
		t.Errorf("handed %v", handed)
	}
	if list, err := workshop.Deals(ctx, pool, bo, true); err != nil || len(list) != 0 {
		t.Errorf("Bo's deals: %d, %v", len(list), err)
	}
	if _, err := workshop.AddDefect(ctx, pool, bo, d.ID, "Rost", ""); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("a defect on another's deal: %v", err)
	}
	if err := workshop.CancelDeal(ctx, pool, bo, d.ID, "Fel"); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("another's deal cancelled: %v", err)
	}
	if _, err := workshop.CompleteDeal(ctx, pool, bo, d.ID); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("another's deal completed: %v", err)
	}
	if _, err := workshop.ReviseDeal(ctx, pool, bo, d.ID, 1, consumerTerms(d.Buyers[0].PartyID)); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("another's deal revised: %v", err)
	}
	if list, err := workshop.Deals(ctx, pool, anna, false); err != nil || len(list) != 1 {
		t.Errorf("Anna's deals: %d, %v", len(list), err)
	}
	if list, err := workshop.Deals(ctx, pool, everything(owner()), false); err != nil || len(list) != 1 {
		t.Errorf("the owner's deals: %d, %v", len(list), err)
	}
}

// A cancelled deal lets go of its car: the car no longer points at it, and
// its lead offers a new deal.
func TestACancelledDealLetsGoOfItsCarAndLead(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	d := completedDeal(t, pool)
	lead, err := workshop.CreateLead(ctx, pool, sales(), workshop.NewLead{PartyID: d.Buyers[0].PartyID, StockUnitID: d.StockUnitID,
		Channel: "walk_in"})
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.CancelDeal(ctx, pool, sales(), d.ID, "Köparen ångrade sig"); err != nil {
		t.Fatal(err)
	}
	if u, _ := workshop.StockUnitByID(ctx, pool, sales(), d.StockUnitID); u.VehicleSaleID != "" {
		t.Errorf("the car still points at the cancelled deal: %+v", u)
	}
	next, err := workshop.CreateDeal(ctx, pool, sales(), workshop.NewDeal{UnitID: d.StockUnitID, LeadID: lead.ID,
		Terms: consumerTerms(d.Buyers[0].PartyID)})
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.CancelDeal(ctx, pool, sales(), next.ID, "Igen"); err != nil {
		t.Fatal(err)
	}
	if l, _ := workshop.LeadByID(ctx, pool, sales(), lead.ID); l.DealID != "" {
		t.Errorf("the lead points at a cancelled deal: %s", l.DealID)
	}
}

// A deal revised after its car was delivered is completed again without
// moving the car back to sold, and its lead is won once.
func TestADeliveredDealIsCompletedAgainWithoutMovingTheCar(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	unit := carForSale(t, pool)
	kim := driver(t, pool)
	lead, err := workshop.CreateLead(ctx, pool, sales(), workshop.NewLead{PartyID: kim, StockUnitID: unit, Channel: "walk_in"})
	if err != nil {
		t.Fatal(err)
	}
	terms := consumerTerms(kim)
	terms.TaxTreatment = workshop.TaxOrdinary
	d, err := workshop.CreateDeal(ctx, pool, sales(), workshop.NewDeal{UnitID: unit, LeadID: lead.ID, Terms: terms})
	if err != nil {
		t.Fatal(err)
	}
	complete := func(replacing string) workshop.Deal {
		t.Helper()
		doc, _, err := workshop.ProduceSalesContract(ctx, pool, sales(), d.ID, contractTemplate(t, pool), replacing)
		if err != nil {
			t.Fatal(err)
		}
		signAll(t, pool, doc)
		done, err := workshop.CompleteDeal(ctx, pool, sales(), d.ID)
		if err != nil || !done.Completed() {
			t.Fatalf("completed: %+v, %v", done, err)
		}
		return done
	}
	first := complete("")
	moveStock(t, pool, unit, workshop.StockDelivered)
	terms.Warranty = "Sex månader"
	if _, err := workshop.ReviseDeal(ctx, pool, sales(), d.ID, first.Version, terms); err != nil {
		t.Fatal(err)
	}
	complete(first.ContractID)
	if u, _ := workshop.StockUnitByID(ctx, pool, sales(), unit); u.Status != workshop.StockDelivered {
		t.Errorf("the delivered car after the deal was completed again: %s", u.Status)
	}
	var won int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM lead_events WHERE lead_id = $1 AND status = 'won'`, lead.ID).Scan(&won); err != nil || won != 1 {
		t.Errorf("the lead was won %d times (%v)", won, err)
	}
}

// The "as is" warning finds whole phrases once: not "as is" inside "has
// issues", and "som den står och går" as one phrase, not two.
func TestTheAsIsWarningFindsWholePhrasesOnce(t *testing.T) {
	for body, want := range map[string][]string{
		"Bilen säljs som den står och går.":         {"som den står och går"},
		"The seller has issued this contract.":      nil,
		"Köparen har sett den, i befintligt skick.": {"befintligt skick"},
		"Sold AS IS, in its current condition.":     {"as is", "in its current condition"},
		"Vi besom den äran att ...":                 nil,
	} {
		got := workshop.SalesContractWarnings(body, workshop.BuyerConsumer)
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%q: %v, want %v", body, got, want)
		}
	}
}

// One request key sent at once for two cars opens one deal, and the other
// is told the key was used, never a database error.
func TestOneRequestKeyForTwoCarsAtOnceIsOneDeal(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	kim := driver(t, pool)
	units := []string{carForSale(t, pool), func() string {
		p := boughtCar(t, pool)
		moveStock(t, pool, p.StockUnitID, workshop.StockForSale)
		return p.StockUnitID
	}()}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, u := range units {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = workshop.CreateDeal(ctx, pool, sales(), workshop.NewDeal{UnitID: u, Terms: consumerTerms(kim), Key: "same-key"})
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, workshop.ErrInvalid):
			t.Errorf("the other car: %v; want told the key was used", err)
		}
	}
	if ok != 1 {
		t.Errorf("%d deals opened with one key", ok)
	}
}

// Terms written before the eligibility they relied on was recorded on
// them -- by the release before 0205 -- are read with the eligibility
// that stands, not refused.
func TestTermsWithoutTheirEligibilityAreReadWithTheStandingOne(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	unit := eligibleCar(t, pool)
	terms := consumerTerms(driver(t, pool))
	terms.TaxTreatment = workshop.TaxMarginScheme
	d, err := workshop.CreateDeal(ctx, pool, sales(), workshop.NewDeal{UnitID: unit, Terms: terms})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`ALTER TABLE vehicle_sale_terms DISABLE TRIGGER vehicle_sale_terms_eligibility`,
		`UPDATE vehicle_sale_terms SET eligibility_id = NULL WHERE sale_id = '` + d.ID + `'`,
		`ALTER TABLE vehicle_sale_terms ENABLE TRIGGER vehicle_sale_terms_eligibility`} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	tax, err := workshop.SaleTaxOfDeal(ctx, pool, buyerScope(), d.ID)
	if err != nil || tax.VATMinor != 800_000 || tax.EligibilityID == "" {
		t.Errorf("the tax of terms with no eligibility recorded: %+v, %v", tax, err)
	}
}

// A contract made before contracts said what they were made from is read
// as made from its terms and the defects known when it was made: a deal
// completed then is still complete, and a defect found later still makes
// it out of date.
func TestAContractWithoutItsBasisIsReadAsMadeFromItsTime(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	d := completedDeal(t, pool)
	for _, q := range []string{`ALTER TABLE document_records DISABLE TRIGGER document_records_are_frozen`,
		`UPDATE document_records SET data = data - 'basis' WHERE id = '` + d.ContractID + `'`,
		`ALTER TABLE document_records ENABLE TRIGGER document_records_are_frozen`} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	got, err := workshop.DealByID(ctx, pool, sales(), d.ID)
	if err != nil || !got.Completed() {
		t.Fatalf("a deal completed on a contract with no basis: completed %v, missing %v, %v", got.Completed(), got.Missing(), err)
	}
	if _, err := workshop.AddDefect(ctx, pool, sales(), d.ID, "Buckla i dörren", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := workshop.DealByID(ctx, pool, sales(), d.ID); got.Completed() {
		t.Error("a defect found after the old contract does not make it out of date")
	}
}

// With no template chosen the contract is the shipped one, never another
// of the shop's: a shop with only its own template is asked which.
func TestNoTemplateChosenIsTheShippedOneOrAQuestion(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	if _, err := workshop.SaveDocumentTemplate(ctx, pool, everything(owner()), "", workshop.DocSalesContract, "Eget avtal",
		"Eget avtal för {buyers}."); err != nil {
		t.Fatal(err)
	}
	d, err := workshop.CreateDeal(ctx, pool, sales(), workshop.NewDeal{UnitID: carForSale(t, pool), Terms: consumerTerms(driver(t, pool))})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := workshop.ProduceSalesContract(ctx, pool, sales(), d.ID, "", ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("no template chosen, with only the shop's own: %v; want asked which", err)
	}
}
