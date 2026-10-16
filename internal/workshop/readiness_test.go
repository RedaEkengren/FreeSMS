package workshop_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Getting ready for the first customer: a checklist read from the settings
// themselves, a few decisions kept as rows, and nothing in the way of work.

func taskOf(t *testing.T, s workshop.Readiness, key string) workshop.ReadinessTask {
	t.Helper()
	for _, x := range s.Tasks {
		if x.Key == key {
			return x
		}
	}
	t.Fatalf("no task %q", key)
	return workshop.ReadinessTask{}
}

func readiness(t *testing.T, pool *pgxpool.Pool, env workshop.ReadinessEnv) workshop.Readiness {
	t.Helper()
	s, err := workshop.ReadinessFor(context.Background(), pool, owner(), env)
	if err != nil {
		t.Fatalf("ReadinessFor: %v", err)
	}
	return s
}

func say(t *testing.T, pool *pgxpool.Pool, step, state, note string) {
	t.Helper()
	if err := workshop.ReadinessStatus(context.Background(), pool, owner(), step, state, note); err != nil {
		t.Fatalf("ReadinessStatus %s: %v", step, err)
	}
}

func completeDetails(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	d, err := workshop.ShopDetailsFor(context.Background(), pool, owner())
	if err != nil {
		t.Fatal(err)
	}
	d.AddressLine1, d.PostalCode, d.City = "Verkstadsgatan 1", "411 38", "Göteborg"
	d.OrgNumber, d.VATNumber, d.PaymentReference, d.Phone = "556677-8899", "SE556677889901", "Bankgiro 123-4567", "031-12 34 56"
	if err := workshop.SaveDetails(context.Background(), pool, owner(), d); err != nil {
		t.Fatal(err)
	}
}

func TestAFreshWorkshopSaysWhatIsMissingAndWhy(t *testing.T) {
	pool := setup(t)
	s := readiness(t, pool, workshop.ReadinessEnv{})
	if s.Ready || s.Missing == 0 || s.Solo {
		t.Fatalf("a workshop with only a name is %+v", s)
	}
	seller := taskOf(t, s, "seller")
	if seller.State != "todo" || !seller.Required || seller.Link != "/shop" || seller.Why == "" {
		t.Errorf("seller = %+v", seller)
	}
	var lacks []string
	for _, f := range seller.Facts() {
		lacks = append(lacks, f.Key)
	}
	for _, want := range []string{"address", "postal code", "city", "company number"} {
		if !strings.Contains(strings.Join(lacks, ","), want) {
			t.Errorf("the missing %q is not said: %v", want, lacks)
		}
	}
	for _, key := range []string{"payment", "vat", "locale", "staff", "data", "final_check", "backups", "support", "accountant", "invoice"} {
		if x := taskOf(t, s, key); x.State != "todo" || !x.Required {
			t.Errorf("%s = %+v, want a required task still to do", key, x)
		}
	}
	// What is optional is never in the way, and a missing provider is by hand.
	for _, key := range []string{"capacity", "rates", "catalogue", "messaging", "lookup", "accounts"} {
		if taskOf(t, s, key).Required {
			t.Errorf("%s is required", key)
		}
	}
	if m := taskOf(t, s, "messaging"); m.State != "manual" || !m.Satisfied() || len(m.Detail) == 0 {
		t.Errorf("messaging without a provider = %+v", m)
	}
	if l := taskOf(t, s, "lookup"); l.State != "manual" || !l.Satisfied() {
		t.Errorf("lookup without a provider = %+v", l)
	}
	if m := taskOf(t, readiness(t, pool, workshop.ReadinessEnv{Email: true, SMS: true, Lookup: true}), "messaging"); m.State != "done" {
		t.Errorf("messaging with email = %+v", m)
	}
}

func TestEverythingRequiredCanBeDoneWithoutAProviderOrAnImport(t *testing.T) {
	pool := setup(t)
	say(t, pool, "operating_model", "done", "solo")
	completeDetails(t, pool)
	s := readiness(t, pool, workshop.ReadinessEnv{})
	if !s.Solo {
		t.Fatal("a one-person workshop is not remembered")
	}
	// A one-person shop is not told to add staff.
	if st := taskOf(t, s, "staff"); st.Required || st.State != "decided" || !st.Satisfied() {
		t.Errorf("staff for a solo workshop = %+v", st)
	}
	for _, key := range []string{"seller", "payment", "vat"} {
		if x := taskOf(t, s, key); x.State != "done" {
			t.Errorf("%s = %+v after the details were saved", key, x)
		}
	}
	if s.Ready {
		t.Fatal("ready with decisions still to make")
	}

	say(t, pool, "locale", "done", "")
	say(t, pool, "invoice", "done", "")
	say(t, pool, "data", "skipped", "")
	say(t, pool, "final_check", "skipped", "")
	say(t, pool, "accountant", "done", "Rut på Redovisning AB, 031-555 01 02")
	say(t, pool, "backups", "done", "Olle, 070-111 22 33")
	say(t, pool, "support", "done", "Hosting-leverantören, 08-123 45")
	s = readiness(t, pool, workshop.ReadinessEnv{})
	if !s.Ready || s.Missing != 0 || s.Done != s.Total {
		t.Fatalf("after every decision: ready %v, %d missing, %d of %d", s.Ready, s.Missing, s.Done, s.Total)
	}
	if b := taskOf(t, s, "backups"); b.Note != "Olle, 070-111 22 33" || b.By == "" || b.At == nil {
		t.Errorf("who owns the backups was not kept with who said so and when: %+v", b)
	}

	// Taking a decision back puts the task back.
	say(t, pool, "data", "reset", "")
	if s = readiness(t, pool, workshop.ReadinessEnv{}); s.Ready || taskOf(t, s, "data").State != "todo" {
		t.Errorf("a decision taken back is still standing: %+v", taskOf(t, s, "data"))
	}
}

func TestADecisionAboutAPersonSaysWhoAndNothingIsInventedForTheRest(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	for _, step := range []string{"accountant", "backups", "support"} {
		if err := workshop.ReadinessStatus(ctx, pool, owner(), step, "done", "  "); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("%s without saying who = %v", step, err)
		}
	}
	for name, args := range map[string][3]string{
		"an invented step":  {"teleport", "done", ""},
		"an invented state": {"locale", "finished", ""},
		"a model of none":   {"operating_model", "done", "several"},
		"a very long note":  {"support", "done", strings.Repeat("x", 301)},
	} {
		if err := workshop.ReadinessStatus(ctx, pool, owner(), args[0], args[1], args[2]); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("%s = %v", name, err)
		}
	}
	if n := countRows(t, pool, `SELECT count(*) FROM setup_steps`); n != 0 {
		t.Errorf("%d steps were written by refused calls", n)
	}
	// Saying it twice is saying it once, with the later words.
	say(t, pool, "support", "done", "First")
	say(t, pool, "support", "done", "Second")
	if n := countRows(t, pool, `SELECT count(*) FROM setup_steps WHERE step = 'support' AND note = 'Second'`); n != 1 {
		t.Error("a repeated step is not one row with the later words")
	}
}

func TestOnlyWhoRunsTheShopSeesOrChangesTheChecklist(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	for _, who := range []access.Scope{advisor(), technician(), partsDesk()} {
		if _, err := workshop.ReadinessFor(ctx, pool, who, workshop.ReadinessEnv{}); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s read the checklist: %v", who.Role, err)
		}
		if err := workshop.ReadinessStatus(ctx, pool, who, "locale", "done", ""); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s changed the checklist: %v", who.Role, err)
		}
		if _, _, err := workshop.InvoiceExample(ctx, pool, who); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s saw the invoice example: %v", who.Role, err)
		}
	}
	if n := countRows(t, pool, `SELECT count(*) FROM setup_steps`); n != 0 {
		t.Errorf("%d steps written by roles that may not", n)
	}
}

func TestTheStaffTaskReadsThePeopleAndDoesNotAskTwice(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	s := readiness(t, pool, workshop.ReadinessEnv{})
	staff := taskOf(t, s, "staff")
	if staff.State != "done" || !staff.Required {
		t.Errorf("staff with a technician = %+v", staff)
	}
	var facts []string
	for _, f := range staff.Facts() {
		facts = append(facts, f.Key+"="+f.Arg)
	}
	if !strings.Contains(strings.Join(facts, ","), "technicians=1") {
		t.Errorf("the count is not said: %v", facts)
	}
	// Lifts are a setting, and make the capacity task done without being asked for.
	if err := workshop.SetLifts(context.Background(), pool, owner(), 2); err != nil {
		t.Fatal(err)
	}
	if c := taskOf(t, readiness(t, pool, workshop.ReadinessEnv{}), "capacity"); c.State != "done" || c.Required {
		t.Errorf("capacity with lifts = %+v", c)
	}
}

func TestTheInvoiceExampleIsDrawnFromTheDetailsAndWritesNothing(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	doc, lacks, err := workshop.InvoiceExample(ctx, pool, owner())
	if err != nil {
		t.Fatal(err)
	}
	if doc.Series != "EXAMPLE" || doc.Seller.Name != "Verkstaden" || len(doc.Lines) == 0 || doc.GrossMinor != 125000 {
		t.Errorf("example = %+v", doc)
	}
	if got := strings.Join(lacks, ","); !strings.Contains(got, "address") || !strings.Contains(got, "company number") || !strings.Contains(got, "payment details") {
		t.Errorf("lacks = %v", lacks)
	}
	completeDetails(t, pool)
	doc, lacks, _ = workshop.InvoiceExample(ctx, pool, owner())
	if len(lacks) != 0 || !strings.Contains(doc.Seller.Address, "Verkstadsgatan 1") || doc.Seller.OrgNumber != "556677-8899" || doc.Seller.PaymentReference == "" {
		t.Errorf("example after the details = %+v, lacks %v", doc.Seller, lacks)
	}
	// Nothing was made and no number was taken.
	if n := countRows(t, pool, `SELECT count(*) FROM invoices`); n != 0 {
		t.Errorf("%d invoices after looking at an example", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM invoice_series`); n != 0 {
		t.Errorf("%d series after looking at an example", n)
	}
	// And the first real invoice is still A-1.
	inv, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil || inv.Reference() != "A-1" {
		t.Errorf("the first real invoice = %v, %v", inv.Reference(), err)
	}
}

func TestAnInvoiceIssuedWithoutTheSellersDetailsSaysSoAndIsNeverRewritten(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	first, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := workshop.DocumentByID(ctx, pool, advisor(), first.ID)
	if got := strings.Join(doc.SellerGaps(), ","); got != "address,company number,payment details" {
		t.Errorf("gaps = %q", got)
	}
	// The details are put right afterwards: the issued document is as it was,
	// and the next one is complete.
	completeDetails(t, pool)
	again, _ := workshop.DocumentByID(ctx, pool, advisor(), first.ID)
	if len(again.SellerGaps()) == 0 || again.Seller.Address != "" {
		t.Errorf("an issued invoice was backfilled: %+v", again.Seller)
	}
	second, err := workshop.Issue(ctx, pool, advisor(), readyToInvoice(t, pool))
	if err != nil {
		t.Fatal(err)
	}
	doc2, _ := workshop.DocumentByID(ctx, pool, advisor(), second.ID)
	if len(doc2.SellerGaps()) != 0 {
		t.Errorf("a complete invoice has gaps: %v", doc2.SellerGaps())
	}
}

func TestADocumentIssuedBeforeTheSellerWasKeptIsNotAccusedOfLackingIt(t *testing.T) {
	var old workshop.Document
	if gaps := old.SellerGaps(); gaps != nil {
		t.Errorf("a document with no seller at all was flagged: %v", gaps)
	}
}
