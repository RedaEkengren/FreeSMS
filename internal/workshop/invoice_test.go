package workshop_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// readyToInvoice returns a work order with two priced lines and a warranty
// line, sitting at ready.
func readyToInvoice(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	id := newJob(t, pool)

	for _, l := range []workshop.NewLine{
		{Kind: "labour", Description: "Replace front pads", QuantityMilli: 2500,
			UnitPriceMinor: 89500, VATRateBasis: 2500},
		{Kind: "part", Description: "Engine oil 5W-30", QuantityMilli: 4500,
			UnitPriceMinor: 12900, VATRateBasis: 2500},
		{Kind: "part", Description: "Caliper (warranty)", QuantityMilli: 1000,
			UnitPriceMinor: 0, VATRateBasis: 2500, CostBearer: "supplier"},
	} {
		if err := workshop.AddLine(ctx, pool, advisor(), id, l); err != nil {
			t.Fatalf("AddLine: %v", err)
		}
	}
	move(t, pool, id, workshop.StateEstimated, workshop.StateAwaitingApproval,
		workshop.StateApproved, workshop.StateInProgress, workshop.StateReady)
	return id
}

func TestIssueSnapshotsTheCustomerAndTheLines(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	id := readyToInvoice(t, pool)

	inv, err := workshop.Issue(ctx, pool, advisor(), id)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if inv.Reference() != "A-1" {
		t.Errorf("reference = %q, want A-1", inv.Reference())
	}
	if inv.CustomerName != "A Customer" {
		t.Errorf("customer = %q, want the name at the time of issue", inv.CustomerName)
	}

	// 2.5 x 895.00 = 2237.50, plus 4.5 x 129.00 = 580.50. The warranty line is
	// on the order and not on the invoice.
	if inv.NetMinor != 281800 {
		t.Errorf("net = %d, want 281800", inv.NetMinor)
	}
	if inv.GrossMinor != inv.NetMinor+inv.VATMinor {
		t.Error("gross is not net plus VAT")
	}

	var lines int
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM invoice_lines WHERE invoice_id = $1`, inv.ID).Scan(&lines)
	}); err != nil {
		t.Fatalf("count lines: %v", err)
	}
	if lines != 2 {
		t.Errorf("the invoice has %d lines, want 2 -- the warranty line must not be charged", lines)
	}

	// Renaming the customer afterwards must not reach the document.
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE people SET display_name = 'Renamed Later'
		                        WHERE id = '22222222-2222-2222-2222-222222222223'`)
		return err
	}); err != nil {
		t.Fatalf("rename: %v", err)
	}

	var stored string
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT customer_name FROM invoices WHERE id = $1`, inv.ID).Scan(&stored)
	}); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored != "A Customer" {
		t.Errorf("the invoice now says %q; it must keep what was true when it was issued", stored)
	}
}

// The property the whole numbering design exists for.
func TestNumbersAreGapFreeAndUniqueUnderConcurrency(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	const n = 8
	ids := make([]string, n)
	for i := range ids {
		ids[i] = readyToInvoice(t, pool)
	}

	var wg sync.WaitGroup
	numbers := make([]int64, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			inv, err := workshop.Issue(ctx, pool, advisor(), ids[i])
			numbers[i], errs[i] = inv.Number, err
		}(i)
	}
	wg.Wait()

	seen := map[int64]bool{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("issue %d failed: %v", i, err)
		}
		if seen[numbers[i]] {
			t.Fatalf("number %d was issued twice", numbers[i])
		}
		seen[numbers[i]] = true
	}
	for want := int64(1); want <= n; want++ {
		if !seen[want] {
			t.Errorf("number %d is missing; the series has a gap", want)
		}
	}
}

// A transaction that fails after allocating must not spend the number.
func TestAFailedIssueLeavesNoGap(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	first := readyToInvoice(t, pool)
	if _, err := workshop.Issue(ctx, pool, advisor(), first); err != nil {
		t.Fatalf("first issue: %v", err)
	}

	// An order with nothing chargeable on it: the state moves, the number is
	// allocated, and then it fails.
	empty := newJob(t, pool)
	move(t, pool, empty, workshop.StateEstimated, workshop.StateAwaitingApproval,
		workshop.StateApproved, workshop.StateInProgress, workshop.StateReady)
	if _, err := workshop.Issue(ctx, pool, advisor(), empty); !errors.Is(err, workshop.ErrNothingToInvoice) {
		t.Fatalf("issuing an empty order = %v, want ErrNothingToInvoice", err)
	}

	third := readyToInvoice(t, pool)
	inv, err := workshop.Issue(ctx, pool, advisor(), third)
	if err != nil {
		t.Fatalf("third issue: %v", err)
	}
	if inv.Number != 2 {
		t.Errorf("the next invoice is number %d, want 2 -- the failed issue spent a number", inv.Number)
	}
}

// "We never update invoices" is a convention. This is a refusal.
func TestAnIssuedInvoiceCannotBeChanged(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	inv, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	for _, stmt := range []string{
		`UPDATE invoices SET gross_minor = 1 WHERE id = '%s'`,
		`DELETE FROM invoices WHERE id = '%s'`,
	} {
		err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, fmt.Sprintf(stmt, inv.ID))
			return err
		})
		if err == nil {
			t.Errorf("%q was allowed", stmt)
			continue
		}
		if !contains(err.Error(), "credit note") {
			t.Errorf("the refusal does not say what to do instead: %v", err)
		}
	}
}

// A credit note reverses exactly, to the minor unit.
func TestCreditNoteReversesTheInvoice(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	inv, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	note, err := workshop.CreditNote(ctx, pool, advisor(), inv.ID)
	if err != nil {
		t.Fatalf("CreditNote: %v", err)
	}
	if note.Number != inv.Number+1 {
		t.Errorf("credit note is %s, want the next number in the same series", note.Reference())
	}
	if note.CreditOfID == nil || *note.CreditOfID != inv.ID {
		t.Error("the credit note does not name what it reverses")
	}
	if inv.GrossMinor+note.GrossMinor != 0 {
		t.Errorf("invoice %d and credit note %d leave %d behind",
			inv.GrossMinor, note.GrossMinor, inv.GrossMinor+note.GrossMinor)
	}
	if inv.VATMinor+note.VATMinor != 0 {
		t.Error("the VAT does not reverse exactly")
	}

	if _, err := workshop.CreditNote(ctx, pool, advisor(), inv.ID); err == nil {
		t.Error("the same invoice was credited twice")
	}
}

func TestAnOrderCannotBeInvoicedTwice(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	id := readyToInvoice(t, pool)
	if _, err := workshop.Issue(ctx, pool, advisor(), id); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := workshop.Issue(ctx, pool, advisor(), id); !errors.Is(err, workshop.ErrAlreadyInvoiced) {
		t.Fatalf("second Issue = %v, want ErrAlreadyInvoiced", err)
	}
}

// The document is read back from the frozen rows, never from the work order.
// A second visit is a second job on the same order, and reading the order
// would make an invoice that changes after it was sent.
func TestTheDocumentDoesNotFollowTheWorkOrder(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	id := readyToInvoice(t, pool)

	inv, err := workshop.Issue(ctx, pool, advisor(), id)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	before, err := workshop.DocumentByID(ctx, pool, advisor(), inv.ID)
	if err != nil {
		t.Fatalf("DocumentByID: %v", err)
	}

	// The work order's own rows move. The state machine refuses invoiced ->
	// in_progress, so this is not a path a user has today -- which is exactly
	// why it is done in SQL: the test is about which table the document reads,
	// not about which route happens to be open this month.
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE work_order_lines
			    SET unit_price_minor = unit_price_minor * 10,
			        description = 'Rewritten after the invoice'
			  WHERE work_order_id = $1`, id)
		return err
	}); err != nil {
		t.Fatalf("move the work order: %v", err)
	}

	after, err := workshop.DocumentByID(ctx, pool, advisor(), inv.ID)
	if err != nil {
		t.Fatalf("DocumentByID: %v", err)
	}
	if len(after.Lines) != len(before.Lines) {
		t.Errorf("the document went from %d lines to %d; it is reading the work order",
			len(before.Lines), len(after.Lines))
	}
	if after.GrossMinor != before.GrossMinor {
		t.Errorf("the document's total moved from %d to %d after it was issued",
			before.GrossMinor, after.GrossMinor)
	}
	for _, l := range after.Lines {
		if l.Description == "Rewritten after the invoice" {
			t.Error("the issued document shows a line the work order changed afterwards")
		}
		if l.NetMinor*10 == l.UnitPriceMinor {
			t.Error("the issued document picked up a price the work order changed afterwards")
		}
	}
}

// A job can carry two rates -- twenty-five per cent on the work and twelve on
// something else -- and the document has to show the split.
func TestTheDocumentGroupsVATByRate(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	id := newJob(t, pool)

	for _, l := range []workshop.NewLine{
		{Kind: "labour", Description: "Work", QuantityMilli: 1000,
			UnitPriceMinor: 100000, VATRateBasis: 2500},
		{Kind: "part", Description: "Something at twelve", QuantityMilli: 1000,
			UnitPriceMinor: 50000, VATRateBasis: 1200},
	} {
		if err := workshop.AddLine(ctx, pool, advisor(), id, l); err != nil {
			t.Fatalf("AddLine: %v", err)
		}
	}
	move(t, pool, id, workshop.StateEstimated, workshop.StateAwaitingApproval,
		workshop.StateApproved, workshop.StateInProgress, workshop.StateReady)

	inv, err := workshop.Issue(ctx, pool, advisor(), id)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	doc, err := workshop.DocumentByID(ctx, pool, advisor(), inv.ID)
	if err != nil {
		t.Fatalf("DocumentByID: %v", err)
	}
	if len(doc.Bands) != 2 {
		t.Fatalf("%d VAT bands, want 2: %+v", len(doc.Bands), doc.Bands)
	}
	// Ordered by rate, so the document reads the same way every time.
	if doc.Bands[0].RateBasisPoints != 1200 || doc.Bands[1].RateBasisPoints != 2500 {
		t.Errorf("bands are not in rate order: %+v", doc.Bands)
	}
	var vat int64
	for _, b := range doc.Bands {
		vat += b.VATMinor
	}
	if vat != doc.VATMinor {
		t.Errorf("the bands sum to %d and the document says %d", vat, doc.VATMinor)
	}
}

// A credit note is an invoice and renders through the same view. Both halves
// name the other.
func TestACreditNoteAndItsInvoicePointAtEachOther(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	id := readyToInvoice(t, pool)

	inv, err := workshop.Issue(ctx, pool, advisor(), id)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	note, err := workshop.CreditNote(ctx, pool, advisor(), inv.ID)
	if err != nil {
		t.Fatalf("CreditNote: %v", err)
	}

	doc, err := workshop.DocumentByID(ctx, pool, advisor(), note.ID)
	if err != nil {
		t.Fatalf("DocumentByID(note): %v", err)
	}
	if !doc.IsCreditNote() {
		t.Error("the credit note does not know it is one")
	}
	if doc.CreditOf != inv.Reference() {
		t.Errorf("the note credits %q, want %q", doc.CreditOf, inv.Reference())
	}
	if doc.GrossMinor >= 0 {
		t.Errorf("the note's total is %d; a credit note is negative", doc.GrossMinor)
	}

	original, err := workshop.DocumentByID(ctx, pool, advisor(), inv.ID)
	if err != nil {
		t.Fatalf("DocumentByID(invoice): %v", err)
	}
	if len(original.CreditedBy) != 1 || original.CreditedBy[0] != note.Reference() {
		t.Errorf("the invoice does not say it was credited: %v", original.CreditedBy)
	}
}

// Authorisation is on the read. A technician with an invoice id in hand is
// refused by the function that would fetch it, not by a template.
func TestATechnicianCannotReadAnInvoice(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	id := readyToInvoice(t, pool)

	inv, err := workshop.Issue(ctx, pool, advisor(), id)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := workshop.DocumentByID(ctx, pool, technician(), inv.ID); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("DocumentByID as a technician = %v, want ErrForbidden", err)
	}
}

// The whole reason the seller is snapshotted. A workshop that changes its name
// or moves premises has not changed who issued last year's invoices, and until
// this the document joined to shops and rewrote itself.
func TestTheSellerOnAnIssuedInvoiceDoesNotMove(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	if err := workshop.SaveDetails(ctx, pool, owner(), workshop.ShopDetails{
		Name: "Verkstaden AB", AddressLine1: "Verkstadsgatan 1",
		PostalCode: "123 45", City: "Stockholm",
		OrgNumber: "556677-8899", VATNumber: "SE556677889901",
		PaymentReference: "Bankgiro 123-4567", PaymentTermsDays: 30, FTax: true,
	}); err != nil {
		t.Fatalf("SaveDetails: %v", err)
	}

	id := readyToInvoice(t, pool)
	inv, err := workshop.Issue(ctx, pool, advisor(), id)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	before, err := workshop.DocumentByID(ctx, pool, advisor(), inv.ID)
	if err != nil {
		t.Fatalf("DocumentByID: %v", err)
	}
	if before.Seller.Name != "Verkstaden AB" {
		t.Fatalf("seller name = %q, want the shop at the time of issue", before.Seller.Name)
	}
	if before.Seller.OrgNumber != "556677-8899" || before.Seller.VATNumber != "SE556677889901" {
		t.Errorf("the registration numbers are missing from the document: %+v", before.Seller)
	}
	if before.Seller.Address != "Verkstadsgatan 1, 123 45 Stockholm" {
		t.Errorf("address = %q, want it assembled the way it is printed", before.Seller.Address)
	}

	// The shop is sold, renamed and moved.
	if err := workshop.SaveDetails(ctx, pool, owner(), workshop.ShopDetails{
		Name: "Nya Bilservice AB", AddressLine1: "Industrivägen 9",
		PostalCode: "987 65", City: "Uppsala",
		OrgNumber: "559900-1122", PaymentTermsDays: 10,
	}); err != nil {
		t.Fatalf("SaveDetails: %v", err)
	}

	after, err := workshop.DocumentByID(ctx, pool, advisor(), inv.ID)
	if err != nil {
		t.Fatalf("DocumentByID: %v", err)
	}
	if after.Seller != before.Seller {
		t.Errorf("the issued document's seller changed:\n before %+v\n after  %+v", before.Seller, after.Seller)
	}
	// And the due date with it, because it is arithmetic on two frozen values.
	if !after.Due().Equal(before.Due()) {
		t.Errorf("the due date moved from %s to %s", before.Due(), after.Due())
	}
}

// A credit note is its own document, issued today by whoever the shop is
// today. That it can differ from the invoice it credits is correct.
func TestACreditNoteTakesTheSellerAsItIsWhenItIsIssued(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	if err := workshop.SaveDetails(ctx, pool, owner(), workshop.ShopDetails{
		Name: "Verkstaden AB", OrgNumber: "556677-8899", PaymentTermsDays: 30,
	}); err != nil {
		t.Fatalf("SaveDetails: %v", err)
	}
	id := readyToInvoice(t, pool)
	inv, err := workshop.Issue(ctx, pool, advisor(), id)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	if err := workshop.SaveDetails(ctx, pool, owner(), workshop.ShopDetails{
		Name: "Nya Bilservice AB", OrgNumber: "559900-1122", PaymentTermsDays: 30,
	}); err != nil {
		t.Fatalf("SaveDetails: %v", err)
	}
	note, err := workshop.CreditNote(ctx, pool, advisor(), inv.ID)
	if err != nil {
		t.Fatalf("CreditNote: %v", err)
	}

	doc, err := workshop.DocumentByID(ctx, pool, advisor(), note.ID)
	if err != nil {
		t.Fatalf("DocumentByID: %v", err)
	}
	if doc.Seller.Name != "Nya Bilservice AB" {
		t.Errorf("the note's seller = %q, want the shop as it is today", doc.Seller.Name)
	}
	original, _ := workshop.DocumentByID(ctx, pool, advisor(), inv.ID)
	if original.Seller.Name != "Verkstaden AB" {
		t.Errorf("crediting rewrote the original's seller to %q", original.Seller.Name)
	}
}

// The bank details and the organisation number are not a service advisor's to
// change, even though they may see a customer's address to invoice them.
func TestOnlyWhoeverRunsTheShopCanChangeItsDetails(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	for _, who := range []struct {
		name  string
		scope access.Scope
	}{
		{"a technician", technician()},
		{"a service advisor", advisor()},
	} {
		if _, err := workshop.ShopDetailsFor(ctx, pool, who.scope); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s could read the shop's details: %v", who.name, err)
		}
		if err := workshop.SaveDetails(ctx, pool, who.scope, workshop.ShopDetails{Name: "Mine now"}); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s could change the shop's details: %v", who.name, err)
		}
	}
}

// Invoicing is a document, not a state change.
//
// The "Move it along" buttons on a ready order used to include one that posted
// state=invoiced straight at the state endpoint, and it worked: the order read
// as billed while no invoice existed, no number had been allocated and the
// parts were still on the shelf. Reproduced against the running application
// before this was written -- the order went to invoiced with zero invoices.
func TestAnOrderCannotBecomeInvoicedWithoutADocument(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	for _, from := range []workshop.State{workshop.StateReady, workshop.StateDeclined} {
		id := newJob(t, pool)
		if err := workshop.AddLine(ctx, pool, advisor(), id, workshop.NewLine{
			Kind: "labour", Description: "Diagnosis", QuantityMilli: 1000,
			UnitPriceMinor: 89500, VATRateBasis: 2500,
		}); err != nil {
			t.Fatalf("AddLine: %v", err)
		}
		switch from {
		case workshop.StateReady:
			move(t, pool, id, workshop.StateEstimated, workshop.StateAwaitingApproval,
				workshop.StateApproved, workshop.StateInProgress, workshop.StateReady)
		case workshop.StateDeclined:
			move(t, pool, id, workshop.StateEstimated, workshop.StateAwaitingApproval,
				workshop.StateDeclined)
		}

		if err := workshop.SetState(ctx, pool, advisor(), id, workshop.StateInvoiced); !errors.Is(err, workshop.ErrNeedsDocument) {
			t.Errorf("from %s: SetState(invoiced) = %v, want ErrNeedsDocument", from, err)
		}

		var state string
		var invoices int
		if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT state FROM work_orders WHERE id = $1`, id).Scan(&state); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT count(*) FROM invoices WHERE work_order_id = $1`, id).Scan(&invoices)
		}); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if state == string(workshop.StateInvoiced) && invoices == 0 {
			t.Errorf("from %s: the order says invoiced and no document exists", from)
		}

		// And the way that does work still works.
		if _, err := workshop.Issue(ctx, pool, advisor(), id); err != nil {
			t.Errorf("from %s: Issue was refused: %v", from, err)
		}
	}
}

// A button that is always refused is worse than no button, so it is not shown.
func TestInvoicedIsNotOfferedAsAMove(t *testing.T) {
	for _, from := range []workshop.State{workshop.StateReady, workshop.StateDeclined} {
		for _, hasWork := range []bool{false, true} {
			for _, s := range workshop.AvailableStates(from, hasWork) {
				if s == workshop.StateInvoiced {
					t.Errorf("AvailableStates(%s, hasWork=%v) offers invoiced", from, hasWork)
				}
			}
		}
	}
	// The transition itself is still legal; only the route is restricted.
	if !workshop.CanTransition(workshop.StateReady, workshop.StateInvoiced) {
		t.Error("ready can no longer reach invoiced at all; Issue needs that")
	}
	if !workshop.CanTransition(workshop.StateDeclined, workshop.StateInvoiced) {
		t.Error("declined can no longer reach invoiced at all")
	}
}

// One order, several tills. The existing concurrency test gives each goroutine
// its own work order and proves the numbering does not collide; it says nothing
// about two people invoicing the same car, which is the ordinary accident --
// a double click, a retried request, two advisors at two screens.
func TestOneOrderYieldsOneInvoiceUnderConcurrency(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	id := readyToInvoice(t, pool)

	const n = 8
	var wg sync.WaitGroup
	var ready sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	ready.Add(n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ready.Done()
			// All of them go at the same moment, which is what makes the
			// window between the check and the lock reachable at all.
			<-start
			_, errs[i] = workshop.Issue(ctx, pool, advisor(), id)
		}(i)
	}
	ready.Wait()
	close(start)
	wg.Wait()

	var issued, refused int
	for i, err := range errs {
		switch {
		case err == nil:
			issued++
		case errors.Is(err, workshop.ErrAlreadyInvoiced):
			refused++
		default:
			t.Errorf("call %d failed with something other than a refusal: %v", i, err)
		}
	}
	if issued != 1 {
		t.Errorf("%d of %d calls issued a document; exactly one must", issued, n)
	}
	if refused != n-1 {
		t.Errorf("%d calls were refused, want %d", refused, n-1)
	}

	var invoices, lines int
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM invoices WHERE work_order_id = $1 AND credit_of_id IS NULL`,
			id).Scan(&invoices); err != nil {
			return err
		}
		// The parts leave the shelf in the same transaction. Issuing twice
		// would take them twice.
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM stock_movements WHERE work_order_id = $1 AND kind = 'consumed'`,
			id).Scan(&lines)
	}); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if invoices != 1 {
		t.Errorf("%d original invoices exist for one order", invoices)
	}
	if lines > 1 {
		t.Errorf("%d consumption movements; the shelf was emptied more than once", lines)
	}
}

// The same accident on the other side: crediting one invoice twice reverses it
// twice, and the customer is owed money that was never charged.
func TestOneInvoiceYieldsOneCreditNoteUnderConcurrency(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	id := readyToInvoice(t, pool)
	inv, err := workshop.Issue(ctx, pool, advisor(), id)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	const n = 6
	var wg sync.WaitGroup
	var ready sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	ready.Add(n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ready.Done()
			<-start
			_, errs[i] = workshop.CreditNote(ctx, pool, advisor(), inv.ID)
		}(i)
	}
	ready.Wait()
	close(start)
	wg.Wait()

	var issued int
	for _, err := range errs {
		if err == nil {
			issued++
		}
	}
	if issued != 1 {
		t.Errorf("%d of %d calls produced a credit note; exactly one must", issued, n)
	}

	var notes int
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM invoices WHERE credit_of_id = $1`, inv.ID).Scan(&notes)
	}); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if notes != 1 {
		t.Errorf("%d credit notes reverse one invoice", notes)
	}
}
