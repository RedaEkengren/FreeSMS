package workshop_test

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

func TestAnInvoiceWithEverythingIsWrittenAsAnElectronicInvoice(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	if err := workshop.SaveDetails(ctx, pool, owner(), workshop.ShopDetails{
		Name: "Verkstaden AB", AddressLine1: "Verkstadsgatan 1", PostalCode: "123 45", City: "Stockholm",
		OrgNumber: "556677-8899", VATNumber: "SE556677889901",
		PaymentReference: "Bankgiro 123-4567", PaymentScheme: "bankgiro", EndpointID: "0007:5566778899", PaymentTermsDays: 30,
	}); err != nil {
		t.Fatal(err)
	}
	card := cardOf(t, pool, fixtureCustomer)
	edit := editOf(card)
	edit.Name, edit.AddressLine1 = card.Name, "Kundgatan 2"
	edit.PostalCode, edit.City, edit.Country = "111 22", "Göteborg", "SE"
	edit.EndpointID, edit.BuyerReference = "0007:1122334455", "Avd 4"
	if _, err := workshop.EditCustomer(ctx, pool, advisor(), fixtureCustomer, card.Revision, edit); err != nil {
		t.Fatal(err)
	}
	inv, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatal(err)
	}

	body, missing, err := workshop.EInvoice(ctx, pool, advisor(), inv.ID)
	if err != nil || len(missing) > 0 {
		t.Fatalf("EInvoice = %v, missing %v", err, missing)
	}
	var doc struct {
		XMLName xml.Name
		ID      string `xml:"ID"`
		Payable string `xml:"LegalMonetaryTotal>PayableAmount"`
		Buyer   string `xml:"AccountingCustomerParty>Party>PostalAddress>StreetName"`
		Account string `xml:"PaymentMeans>PayeeFinancialAccount>ID"`
	}
	if err := xml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("not XML: %v", err)
	}
	if doc.XMLName.Local != "Invoice" || doc.ID != inv.Reference() || doc.Account != "1234567" || doc.Buyer != "Kundgatan 2" {
		t.Errorf("document: %+v", doc)
	}
	// Payable is what the invoice comes to, written the way a network reads it.
	if want := fmt.Sprintf("%d.%02d", inv.GrossMinor/100, inv.GrossMinor%100); doc.Payable != want {
		t.Errorf("payable %s, the invoice comes to %s", doc.Payable, want)
	}

	// The credit note is its own document, and is written as one.
	note, err := workshop.CreditNote(ctx, pool, advisor(), inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	nb, missing, err := workshop.EInvoice(ctx, pool, advisor(), note.ID)
	if err != nil || len(missing) > 0 {
		t.Fatalf("credit note: %v, missing %v", err, missing)
	}
	if !strings.Contains(string(nb), "<CreditNote ") || !strings.Contains(string(nb), "<cbc:CreditNoteTypeCode>381<") || !strings.Contains(string(nb), "<cbc:ID>"+inv.Reference()+"</cbc:ID>") {
		t.Errorf("not a credit note of %s:\n%s", inv.Reference(), nb)
	}
	if strings.Contains(string(nb), "Amount currencyID=\"SEK\">-") {
		t.Error("a credit note carries a negative amount")
	}
}

func TestAnInvoiceIssuedWithoutTheDetailsSaysWhatItLacks(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	inv, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatal(err)
	}
	body, missing, err := workshop.EInvoice(ctx, pool, advisor(), inv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		t.Error("half an invoice was written")
	}
	joined := strings.Join(missing, "|")
	for _, want := range []string{"electronic invoice address", "customer's reference", "customer's country"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q from what it says it lacks: %v", want, missing)
		}
	}
}

func TestOnlyTheFrontDeskCanWriteAnElectronicInvoice(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	inv, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := workshop.EInvoice(ctx, pool, technician(), inv.ID); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("as a technician = %v, want ErrForbidden", err)
	}
	if _, _, err := workshop.EInvoice(ctx, pool, advisor(), "not-an-id"); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("a bad id = %v, want ErrNotFound", err)
	}
}
