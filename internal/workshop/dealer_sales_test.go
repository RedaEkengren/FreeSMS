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
	tpls, err := workshop.SalesContractTemplates(context.Background(), pool, sales())
	if err != nil || len(tpls) != 1 {
		t.Fatalf("the shipped contract: %+v, %v", tpls, err)
	}
	return tpls[0].VersionID
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
	d, err := workshop.CreateDeal(ctx, pool, sales(), unit, lead.ID, consumerTerms(kim), "deal-1")
	if err != nil {
		t.Fatal(err)
	}
	if d.State != workshop.DealDraft || d.Version != 1 || d.TotalMinor() != 12_500_000+1_000_000 || len(d.Buyers) != 1 {
		t.Errorf("the new deal: %+v", d)
	}
	if again, err := workshop.CreateDeal(ctx, pool, sales(), unit, lead.ID, consumerTerms(kim), "deal-1"); err != nil || again.ID != d.ID {
		t.Errorf("the same request again: %+v, %v", again, err)
	}
	if _, err := workshop.CreateDeal(ctx, pool, sales(), unit, "", consumerTerms(kim), "deal-2"); !errors.Is(err, workshop.ErrInvalid) {
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
			d, err := workshop.CreateDeal(ctx, pool, sales(), p.StockUnitID, "", terms, "")
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
	d, err := workshop.CreateDeal(ctx, pool, sales(), unit, "", consumerTerms(kim), "")
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
		got, err := workshop.CreateDeal(ctx, pool, s, unit, "", consumerTerms(kim), "")
		if !errors.Is(err, access.ErrForbidden) && !errors.Is(err, workshop.ErrNotFound) {
			t.Errorf("%s made a deal: %v", name, err)
		}
		if handed := testsupport.Handed(got, "ID", "Buyers"); len(handed) != 0 {
			t.Errorf("%s was handed %v", name, handed)
		}
	}
	off := sales()
	off.Modules = []access.Module{access.Workshop}
	if _, err := workshop.CreateDeal(ctx, pool, off, unit, "", consumerTerms(kim), ""); !errors.Is(err, access.ErrModuleOff) {
		t.Errorf("a deal with car sales off: %v", err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = workshop.CreateDeal(ctx, pool, sales(), unit, "", consumerTerms(kim), "")
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
	if _, err := workshop.CreateDeal(ctx, pool, sales(), unit, "", consumerTerms(kim), ""); err != nil {
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
	d, err := workshop.CreateDeal(ctx, pool, sales(), unit, "", consumerTerms(driver(t, pool)), "")
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
