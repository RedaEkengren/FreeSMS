package workshop_test

import (
	"context"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// The one action queue (#158).

// permanentAdapter is a provider that refuses for good.
type permanentAdapter struct{}

func (permanentAdapter) Send(context.Context, workshop.OutboxItem) error {
	return workshop.ErrPermanent
}

// Core's items and every module's on one list, in one order: what has gone
// wrong first -- a call given up -- then by module, Core's before the
// workshop's before the dealer's, and the workshop's by job as they always
// were. Each for the roles that act on it: a technician gets none of Core's
// or the dealer's, and the dealer's goes with its module.
func TestTheQueueIsOneListAcrossModulesInOneOrder(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	currentPool = pool
	all := everything(owner())

	// A job ready and the customer not told: the workshop's.
	readyToInvoice(t, pool)
	// A purchase agreement waiting for its signatures: the dealer's.
	tpl := purchaseTemplate(t, pool)
	doc := purchaseAgreement(t, pool, tpl, "")
	// A flow in use and not checked: Core's.
	flow, err := workshop.AddPaymentFlow(ctx, pool, all, "Swish i disken", "swish")
	if err != nil {
		t.Fatal(err)
	}
	rental := advisor()
	rental.Modules = []access.Module{access.Rental}
	if _, err := workshop.IssueManualAndPay(ctx, pool, rental, workshop.ManualInvoice{CustomerID: fixtureCustomer,
		Lines: []workshop.SourcedLine{{Description: "Extra tankning", QuantityMilli: 1000, UnitPriceMinor: 50000, VATRateBasis: 2500}}},
		workshop.CashSale{FlowID: flow, Payments: []workshop.CashPayment{{Method: "swish"}}}); err != nil {
		t.Fatal(err)
	}
	// A signature request the signing service refused for good: gone wrong.
	d, _ := workshop.DocumentRecordByID(ctx, pool, buyerScope(), doc)
	if _, err := workshop.QueueSignatureRequest(ctx, pool, buyerScope(), doc, d.Parties[0].ID); err != nil {
		t.Fatal(err)
	}
	if run, err := workshop.RunOutbox(ctx, pool, shopID, map[string]workshop.OutboxAdapter{"signature_request": permanentAdapter{}}, 10); err != nil || run.GaveUp != 1 {
		t.Fatalf("the refused request: %+v, %v", run, err)
	}

	items, err := workshop.WaitingFor(ctx, pool, all)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, it := range items {
		kinds = append(kinds, it.Kind)
	}
	want := []string{"outbox_given_up", "flow_unchecked", "not_told", "unsigned"}
	if len(kinds) != len(want) {
		t.Fatalf("the queue: %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("the queue: %v, want %v", kinds, want)
		}
	}
	got := items[3]
	if got.Module != access.Dealer || got.SubjectKind != "document" || got.SubjectID != doc || got.Text != tpl.Name || got.Link() != "/contracts/"+doc {
		t.Errorf("the unsigned document: %+v, link %s", got, got.Link())
	}
	if got := items[0]; got.Count != 1 || got.Link() != "/outbox" || got.Urgency != workshop.UrgencyNow {
		t.Errorf("the call given up: %+v", got)
	}
	if n, err := workshop.CountWaiting(ctx, pool, all); err != nil || n != len(want) {
		t.Errorf("the count: %d, %v; want the whole list, %d", n, err, len(want))
	}

	// The dealer switched off: its document leaves the list.
	workshopOnly := owner()
	for _, it := range must(workshop.WaitingFor(ctx, pool, workshopOnly)) {
		if it.Kind == "unsigned" {
			t.Errorf("a dealer's document on a workshop-only list: %+v", it)
		}
	}
	// The buyer, who makes the agreement, has it; a technician has none
	// of it, and the front desk none of the owner's.
	if items := must(workshop.WaitingFor(ctx, pool, buyerScope())); len(items) != 1 || items[0].Kind != "unsigned" {
		t.Errorf("the buyer's list: %+v", items)
	}
	for _, s := range []access.Scope{everything(technician()), everything(advisor())} {
		for _, it := range must(workshop.WaitingFor(ctx, pool, s)) {
			switch it.Kind {
			case "outbox_given_up", "outbox_manual", "flow_unchecked", "unsigned":
				t.Errorf("%s has %s on their list", s.Role, it.Kind)
			}
		}
	}

	// Signed, the document leaves every list at once.
	for _, p := range d.Parties {
		if err := workshop.RecordSignature(ctx, pool, buyerScope(), doc, p.ID, workshop.SignatureEvidence{Method: workshop.SignedOnPaper, Hash: d.Hash, WitnessedBy: "Anna"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []access.Scope{all, buyerScope()} {
		for _, it := range must(workshop.WaitingFor(ctx, pool, s)) {
			if it.Kind == "unsigned" {
				t.Errorf("%s: a signed document still on the list", s.Role)
			}
		}
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
