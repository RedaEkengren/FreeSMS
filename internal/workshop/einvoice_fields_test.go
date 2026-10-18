package workshop_test

import (
	"context"
	"errors"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
)

// An issued invoice is frozen, so a fact an electronic invoice needs that was
// not written onto it when it was issued can never be added. These tests hold
// the facts on the document, and hold them still when the shop or the customer
// is later changed.

func TestAnIssuedInvoiceKeepsWhatAnElectronicInvoiceNeeds(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	if err := workshop.SaveDetails(ctx, pool, owner(), workshop.ShopDetails{
		Name: "Verkstaden AB", AddressLine1: "Verkstadsgatan 1", AddressLine2: "Plan 2",
		PostalCode: "123 45", City: "Stockholm", OrgNumber: "556677-8899",
		PaymentReference: "123-4567", PaymentScheme: "bankgiro", EndpointID: "0007:5566778899",
		PaymentTermsDays: 30,
	}); err != nil {
		t.Fatalf("SaveDetails: %v", err)
	}
	card := cardOf(t, pool, fixtureCustomer)
	edit := editOf(card)
	edit.PostalCode, edit.City = "111 22", "Göteborg"
	edit.Country, edit.EndpointID, edit.BuyerReference = "se", "0007:1122334455", "Anna A, avd 4"
	if _, err := workshop.EditCustomer(ctx, pool, advisor(), fixtureCustomer, card.Revision, edit); err != nil {
		t.Fatalf("EditCustomer: %v", err)
	}

	inv, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	doc, err := workshop.DocumentByID(ctx, pool, advisor(), inv.ID)
	if err != nil {
		t.Fatalf("DocumentByID: %v", err)
	}

	s := doc.Seller
	if s.Street != "Verkstadsgatan 1, Plan 2" || s.PostalCode != "123 45" || s.City != "Stockholm" || s.Country != "SE" {
		t.Errorf("the seller's address was not taken apart: %+v", s)
	}
	if s.EndpointID != "0007:5566778899" || s.PaymentScheme != "bankgiro" {
		t.Errorf("the seller's electronic address or payment means is missing: %+v", s)
	}
	if doc.CustomerPostalCode != "111 22" || doc.CustomerCity != "Göteborg" || doc.CustomerCountry != "SE" {
		t.Errorf("the buyer's address was not taken apart: %q %q %q", doc.CustomerPostalCode, doc.CustomerCity, doc.CustomerCountry)
	}
	if doc.CustomerEndpointID != "0007:1122334455" || doc.CustomerReference != "Anna A, avd 4" {
		t.Errorf("the buyer's electronic address or reference is missing: %q %q", doc.CustomerEndpointID, doc.CustomerReference)
	}
	if len(doc.Lines) == 0 {
		t.Fatal("no lines")
	}
	for _, l := range doc.Lines {
		want := "C62"
		if l.Kind == "labour" {
			want = "HUR"
		}
		if l.UnitCode != want || l.VATCategory != "S" {
			t.Errorf("line %q: unit %q category %q, want %q and S", l.Description, l.UnitCode, l.VATCategory, want)
		}
	}

	// Moved, renamed and re-addressed afterwards: the document does not follow.
	if err := workshop.SaveDetails(ctx, pool, owner(), workshop.ShopDetails{Name: "Nya AB", PaymentTermsDays: 30}); err != nil {
		t.Fatal(err)
	}
	card = cardOf(t, pool, fixtureCustomer)
	edit = editOf(card)
	edit.PostalCode, edit.City, edit.Country, edit.EndpointID, edit.BuyerReference = "999 99", "Kiruna", "NO", "", ""
	if _, err := workshop.EditCustomer(ctx, pool, advisor(), fixtureCustomer, card.Revision, edit); err != nil {
		t.Fatal(err)
	}
	after, _ := workshop.DocumentByID(ctx, pool, advisor(), inv.ID)
	if after.Seller != doc.Seller || after.CustomerPostalCode != doc.CustomerPostalCode || after.CustomerCountry != doc.CustomerCountry ||
		after.CustomerEndpointID != doc.CustomerEndpointID || after.CustomerReference != doc.CustomerReference {
		t.Errorf("the issued document followed a later change:\n before %+v\n after  %+v", doc, after)
	}

	// A credit note is of the same buyer, and takes the units off the lines it reverses.
	note, err := workshop.CreditNote(ctx, pool, advisor(), inv.ID)
	if err != nil {
		t.Fatalf("CreditNote: %v", err)
	}
	nd, _ := workshop.DocumentByID(ctx, pool, advisor(), note.ID)
	if nd.CustomerReference != "Anna A, avd 4" || nd.CustomerCountry != "SE" || nd.CustomerEndpointID != "0007:1122334455" {
		t.Errorf("the credit note's buyer is not the invoice's: %+v", nd)
	}
	for i, l := range nd.Lines {
		if l.UnitCode != doc.Lines[i].UnitCode || l.VATCategory != "S" {
			t.Errorf("credit line %d: unit %q category %q, want %q and S", i, l.UnitCode, l.VATCategory, doc.Lines[i].UnitCode)
		}
	}
}

// A rate of nothing could be exempt, zero-rated, reverse charge or outside the
// scope of VAT. The rate does not say which, so nothing is claimed.
func TestAZeroRatedLineIsNotGivenACategoryItWasNotTold(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	id := newJob(t, pool)
	if err := workshop.AddLine(ctx, pool, advisor(), id, workshop.NewLine{
		Kind: "labour", Description: "Export job", QuantityMilli: 1000, UnitPriceMinor: 50000, VATRateBasis: 0}); err != nil {
		t.Fatalf("AddLine: %v", err)
	}
	move(t, pool, id, workshop.StateEstimated, workshop.StateAwaitingApproval,
		workshop.StateApproved, workshop.StateInProgress, workshop.StateReady)
	inv, err := workshop.Issue(ctx, pool, advisor(), id)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	doc, _ := workshop.DocumentByID(ctx, pool, advisor(), inv.ID)
	for _, l := range doc.Lines {
		if l.VATRateBasis == 0 && l.VATCategory != "" {
			t.Errorf("a 0 per cent line was given category %q", l.VATCategory)
		}
	}
}

func TestTheElectronicFieldsAreChecked(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	if err := workshop.SaveDetails(ctx, pool, owner(), workshop.ShopDetails{Name: "X", PaymentScheme: "swish"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a payment means that is none of the three = %v, want ErrInvalid", err)
	}
	card := cardOf(t, pool, fixtureCustomer)
	edit := editOf(card)
	for _, bad := range []string{"S", "SWE", "5E", "s1"} {
		edit.Country = bad
		if _, err := workshop.EditCustomer(ctx, pool, advisor(), fixtureCustomer, card.Revision, edit); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("country %q = %v, want ErrInvalid", bad, err)
		}
	}
}

// What the person gave for electronic invoices is theirs: it is in the copy
// they are given, and it goes when they are erased.
func TestTheElectronicAddressAndReferenceAreTheirsToHaveAndToHaveErased(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	card := cardOf(t, pool, fixtureCustomer)
	edit := editOf(card)
	edit.Country, edit.EndpointID, edit.BuyerReference = "SE", "0007:1122334455", "Ref 77"
	if _, err := workshop.EditCustomer(ctx, pool, advisor(), fixtureCustomer, card.Revision, edit); err != nil {
		t.Fatalf("EditCustomer: %v", err)
	}
	e, err := workshop.ExportPerson(ctx, pool, advisor(), customerPersonID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range e.Addresses {
		found = found || (a.EndpointID == "0007:1122334455" && a.Reference == "Ref 77")
	}
	if !found {
		t.Errorf("the copy they are given leaves out what they gave for invoices: %+v", e.Addresses)
	}

	if err := workshop.ErasePerson(ctx, pool, advisor(), customerPersonID, "Asked"); err != nil {
		t.Fatal(err)
	}
	var country, endpoint, ref *string
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT country, endpoint_id, buyer_reference FROM customers WHERE person_id = $1`,
			customerPersonID).Scan(&country, &endpoint, &ref)
	}); err != nil {
		t.Fatal(err)
	}
	if country != nil || endpoint != nil || ref != nil {
		t.Errorf("erasure left %v %v %v", country, endpoint, ref)
	}
}
