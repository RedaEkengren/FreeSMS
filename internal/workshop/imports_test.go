package workshop_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Starting from what the shop already has: Swedish files, a preview that
// writes nothing, duplicates that need a person, the same file twice, and a
// batch that is all or nothing.

func TestSwedishFilesAreReadTheWayTheyWereWritten(t *testing.T) {
	// Semicolons, a decimal comma, a space as thousands separator, a byte
	// order mark and åäö.
	file := "\xef\xbb\xbfartikelnummer;benämning;enhet;inpris;utpris;minsaldo;lagerplats\n" +
		"BP-100;Bromsklossar fram;st;1 234,50;1.650,00;2;Hylla Ä3\n" +
		"OIL-1;Motorolja 5W-30;liter;89,5;129;20,5;\n\n"
	p, err := workshop.ParseImport("parts", []byte(file))
	if err != nil {
		t.Fatal(err)
	}
	if p.Encoding != "UTF-8" || len(p.Rows) != 2 {
		t.Fatalf("encoding %s, %d rows", p.Encoding, len(p.Rows))
	}
	a, b := p.Rows[0], p.Rows[1]
	if len(a.Errors) != 0 || a.Values["cost"] != "123450" || a.Values["price"] != "165000" || a.Values["unit"] != "each" || a.Values["location"] != "Hylla Ä3" || a.Values["minimum"] != "2000" {
		t.Errorf("row 2 = %+v", a)
	}
	if len(b.Errors) != 0 || b.Values["cost"] != "8950" || b.Values["unit"] != "litre" || b.Values["minimum"] != "20500" {
		t.Errorf("row 3 = %+v", b)
	}
	if a.No != 2 || b.No != 3 {
		t.Errorf("row numbers = %d, %d, want the file's lines", a.No, b.No)
	}
}

func TestAnOlderPackagesFileIsReadAsWindows1252(t *testing.T) {
	file := []byte("regnr;märke;modell\nABC123;Citroën;Saxo\nDEF456;Volvo;V70 \x96 kombi\n")
	// "ö" in Latin-1 is 0xF6 and the en dash is 0x96 in Windows-1252.
	file = []byte(strings.ReplaceAll(strings.ReplaceAll(string(file), "ä", "\xe4"), "ë", "\xeb"))
	p, err := workshop.ParseImport("vehicles", file)
	if err != nil {
		t.Fatal(err)
	}
	if p.Encoding != "Windows-1252" || p.Rows[0].Values["make"] != "Citroën" || !strings.Contains(p.Rows[1].Values["model"], "–") {
		t.Errorf("%s: %+v", p.Encoding, p.Rows)
	}
}

func TestACellThatIsAFormulaOrANumberThatIsNotIsARowProblem(t *testing.T) {
	file := "number;name;unit;cost;price;minimum;location\n" +
		"A1;=HYPERLINK(\"http://x\");st;10;20;;\n" +
		"A2;Fine;st;10,555;20;;\n" +
		"A3;Fine;st;abc;20;;\n" +
		"A4;Fine;stycke;10;20;;\n" +
		"A5;+cmd|' /C calc'!A0;st;10;20;;\n" +
		"A6;-Minus;st;10;20;;\n" +
		"A7;Fine;st;-5;20;;\n" +
		"A8;Fine;st;10;20;;\n"
	p, err := workshop.ParseImport("parts", []byte(file))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A1": "formula", "A2": "decimals", "A3": "not a number", "A4": "unit", "A5": "formula", "A6": "formula", "A7": "negative", "A8": ""}
	for _, r := range p.Rows {
		got := strings.Join(r.Errors, "; ")
		if w := want[r.Ref]; (w == "") != (got == "") || !strings.Contains(got, w) {
			t.Errorf("%s: errors %q, want one about %q", r.Ref, got, w)
		}
	}
}

func TestAFileThatIsNotAFileOfThatKindIsRefused(t *testing.T) {
	for name, file := range map[string]string{
		"empty":            "",
		"only a header":    "number;name\n",
		"a missing column": "number;unit\nA;st\n",
		"binary":           "number;name\nA;\x00\x01\n",
		"a doubled column": "number;name;name\nA;B;C\n",
	} {
		if _, err := workshop.ParseImport("parts", []byte(file)); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("%s = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := workshop.ParseImport("invoices", []byte("a;b\n1;2\n")); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("an unimportable kind = %v", err)
	}
	// Columns we do not know are said, not dropped.
	p, err := workshop.ParseImport("parts", []byte("number;name;colour\nA;B;red\n"))
	if err != nil || len(p.Unknown) != 1 || p.Unknown[0] != "colour" {
		t.Errorf("unknown columns = %v, %v", p.Unknown, err)
	}
}

const customerFile = "kundnr;typ;namn;kontakt;orgnr;telefon;epost;adress;postnr;ort\n" +
	"K-1;privat;Anna Åkesson;;;070-123 45 67;ANNA@example.se;Storgatan 1;411 38;Göteborg\n" +
	"K-2;företag;Åkeri Nilsson AB;Per Nilsson;556677-8899;031-12 34 56;info@akeri.example;Hamnvägen 4;418 34;Göteborg\n" +
	"K-3;privat;;;;;;;;\n" +
	"K-4;privat;Anna Åkesson igen;;;0701234567;;;;\n"

func TestAPreviewWritesNothingAndSaysWhatEachRowWouldDo(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	if _, err := workshop.CreateCustomer(ctx, pool, advisor(), workshop.NewCustomer{Kind: "private", Name: "Anna A", Phone: "070 123 45 67"}); err != nil {
		t.Fatal(err)
	}
	before := countRows(t, pool, `SELECT count(*) FROM customers`)
	b, err := workshop.PreviewImport(ctx, pool, owner(), "customers", "kunder.csv", []byte(customerFile))
	if err != nil {
		t.Fatal(err)
	}
	if countRows(t, pool, `SELECT count(*) FROM customers`) != before {
		t.Fatal("a preview wrote a customer")
	}
	if b.Status != "previewed" || b.RowCount != 4 || b.Existing {
		t.Fatalf("batch = %+v", b)
	}
	row := func(n int) workshop.ImportRow {
		for _, r := range b.Rows {
			if r.No == n {
				return r
			}
		}
		t.Fatalf("no row %d", n)
		return workshop.ImportRow{}
	}
	if r := row(2); r.Action != "create" || r.Values["email"] != "anna@example.se" || len(r.Candidates) != 1 || r.Candidates[0].Name != "Anna A" || !r.NeedsDecision() {
		t.Errorf("row 2 (the same telephone as an existing customer) = %+v", r)
	}
	if r := row(3); r.Action != "create" || len(r.Candidates) != 0 || r.NeedsDecision() {
		t.Errorf("row 3 = %+v", r)
	}
	if r := row(4); r.Action != "error" || !strings.Contains(strings.Join(r.Errors, ";"), "name is missing") {
		t.Errorf("row 4 = %+v", r)
	}
	// The same number again in the file, and the same as an earlier row.
	if r := row(5); len(r.Candidates) == 0 {
		t.Errorf("row 5 shares a number with the existing customer: %+v", r)
	}
}

func TestAnImportIsAllOrNothingAndNeedsADecisionOnAPossibleDuplicate(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	existing, err := workshop.CreateCustomer(ctx, pool, advisor(), workshop.NewCustomer{Kind: "private", Name: "Anna A", Phone: "070 123 45 67"})
	if err != nil {
		t.Fatal(err)
	}
	base := countRows(t, pool, `SELECT count(*) FROM customers`)
	b, _ := workshop.PreviewImport(ctx, pool, owner(), "customers", "kunder.csv", []byte(customerFile))

	// Row 2 may be Anna A and nobody has said; row 4 cannot be imported.
	if _, err := workshop.ApplyImport(ctx, pool, owner(), b.ID, nil, true); !errors.Is(err, workshop.ErrInvalid) || !strings.Contains(err.Error(), "row 2") {
		t.Fatalf("applying with an undecided duplicate = %v", err)
	}
	decide := map[int]workshop.ImportDecision{2: {Choice: "link", Target: existing}, 5: {Choice: "skip"}}
	if _, err := workshop.ApplyImport(ctx, pool, owner(), b.ID, decide, false); !errors.Is(err, workshop.ErrInvalid) || !strings.Contains(err.Error(), "cannot be imported") {
		t.Fatalf("applying with a row that cannot be imported, without saying so = %v", err)
	}
	if got := countRows(t, pool, `SELECT count(*) FROM customers`); got != base {
		t.Fatalf("a refused apply left %d customers, had %d", got, base)
	}
	if _, err := workshop.ApplyImport(ctx, pool, owner(), b.ID, map[int]workshop.ImportDecision{2: {Choice: "link", Target: "11111111-1111-1111-1111-111111111111"}}, true); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("linking to somebody who is not a candidate = %v", err)
	}

	done, err := workshop.ApplyImport(ctx, pool, owner(), b.ID, decide, true)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != "applied" || done.Created != 1 || done.Skipped != 2 || done.Failed != 1 || done.Updated != 0 {
		t.Errorf("report = created %d updated %d skipped %d failed %d (%s)", done.Created, done.Updated, done.Skipped, done.Failed, done.Status)
	}
	if got := countRows(t, pool, `SELECT count(*) FROM customers`); got != base+1 {
		t.Errorf("%d customers, want %d and one created", got, base)
	}
	// A linked row remembers what the source called it; Anna A was not touched.
	if n := countRows(t, pool, `SELECT count(*) FROM import_refs WHERE external_id = 'K-1' AND entity_id = $1`, existing); n != 1 {
		t.Error("the link to the existing customer was not remembered")
	}
	// What the file said is gone; what was done is not.
	if n := countRows(t, pool, `SELECT count(*) FROM import_rows WHERE data IS NOT NULL OR info IS NOT NULL`); n != 0 {
		t.Errorf("%d rows still carry the file's contents", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM import_rows WHERE outcome IS NOT NULL`); n != 4 {
		t.Errorf("%d rows have an outcome, want 4", n)
	}
}

func TestTheSameFileTwiceIsTheSameBatchAndACorrectedFileFindsWhatItMade(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	base := countRows(t, pool, `SELECT count(*) FROM customers`)
	file := []byte("ref;type;name;phone\nK-1;private;Berit Berg;\nK-2;company;Verkstad AB;\n")
	first, err := workshop.PreviewImport(ctx, pool, owner(), "customers", "a.csv", file)
	if err != nil {
		t.Fatal(err)
	}
	again, err := workshop.PreviewImport(ctx, pool, owner(), "customers", "kopia.csv", file)
	if err != nil || !again.Existing || again.ID != first.ID {
		t.Fatalf("the same file = %+v, %v", again, err)
	}
	if _, err := workshop.ApplyImport(ctx, pool, owner(), first.ID, nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.ApplyImport(ctx, pool, owner(), first.ID, nil, false); !errors.Is(err, workshop.ErrImportApplied) {
		t.Errorf("applying twice = %v, want ErrImportApplied", err)
	}
	if again, err = workshop.PreviewImport(ctx, pool, owner(), "customers", "a.csv", file); err != nil || !again.Existing || again.Status != "applied" {
		t.Errorf("uploading an applied file = %+v, %v", again, err)
	}
	if got := countRows(t, pool, `SELECT count(*) FROM customers`); got != base+2 {
		t.Fatalf("%d customers after a retried upload, want %d", got, base+2)
	}

	// The same people with a telephone number each, in a different file: the
	// references find the customers, and only the blanks are filled in.
	fixed := []byte("ref;type;name;phone\nK-1;private;Berit Berg;070-111 22 33\nK-2;company;Verkstad AB;\n")
	b, err := workshop.PreviewImport(ctx, pool, owner(), "customers", "b.csv", fixed)
	if err != nil {
		t.Fatal(err)
	}
	if b.WouldUpdate() != 1 || b.WouldSkip() != 1 || b.WouldCreate() != 0 {
		t.Fatalf("the corrected file would create %d, fill %d, leave %d", b.WouldCreate(), b.WouldUpdate(), b.WouldSkip())
	}
	done, err := workshop.ApplyImport(ctx, pool, owner(), b.ID, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if done.Updated != 1 || done.Created != 0 {
		t.Errorf("report = %+v", done)
	}
	if got := countRows(t, pool, `SELECT count(*) FROM customers`); got != base+2 {
		t.Errorf("%d customers after the corrected file, want %d", got, base+2)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM people WHERE display_name = 'Berit Berg' AND phone = '070-111 22 33'`); n != 1 {
		t.Error("the blank telephone number was not filled in")
	}
	if n := countRows(t, pool, `SELECT count(*) FROM record_changes WHERE field = 'phone' AND reason LIKE 'Import%'`); n != 1 {
		t.Error("filling in a value was not recorded as a change")
	}
}

func TestVehiclesFindTheirOwnerByTheSourcesReference(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	custs, _ := workshop.PreviewImport(ctx, pool, owner(), "customers", "k.csv", []byte("ref;type;name\nK-1;private;Carin Carlsson\n"))
	workshop.ApplyImport(ctx, pool, owner(), custs.ID, nil, false)

	file := "registration;vin;make;model;year;owner_ref;odometer_km;odometer_date\n" +
		"abc 123;;Volvo;V70;2012;K-1;184 500;2026-01-15\n" +
		"DEF-456;YV1BW59P0B1234567;Saab;9-3;2005;K-99;;\n" +
		"ABC123;;Volvo;V70;;;\n"
	b, err := workshop.PreviewImport(ctx, pool, owner(), "vehicles", "bilar.csv", []byte(file))
	if err != nil {
		t.Fatal(err)
	}
	if r := b.Rows[2]; r.Action != "error" || !strings.Contains(strings.Join(r.Errors, ";"), "also on row 2") {
		t.Errorf("the same plate twice in a file = %+v", r)
	}
	if r := b.Rows[1]; r.Action != "create" || !strings.Contains(strings.Join(r.Warnings, ";"), "K-99 is not known") {
		t.Errorf("an unknown owner = %+v", r)
	}
	done, err := workshop.ApplyImport(ctx, pool, owner(), b.ID, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if done.Created != 2 || done.Failed != 1 {
		t.Errorf("report = %+v", done)
	}
	if n := countRows(t, pool, `
		SELECT count(*) FROM vehicle_ownership o JOIN vehicle_registrations r ON r.vehicle_id = o.vehicle_id
		WHERE r.normalised = 'ABC123' AND o.owned_to IS NULL AND o.customer_id = (SELECT entity_id FROM import_refs WHERE external_id = 'K-1')`); n != 1 {
		t.Error("the vehicle did not get its owner")
	}
	if n := countRows(t, pool, `SELECT count(*) FROM vehicle_ownership o JOIN vehicle_registrations r ON r.vehicle_id = o.vehicle_id WHERE r.normalised = 'DEF456'`); n != 0 {
		t.Error("a vehicle with an unknown owner was given one")
	}
	if n := countRows(t, pool, `SELECT count(*) FROM odometer_readings WHERE km = 184500 AND source = 'import' AND (read_at AT TIME ZONE 'Europe/Stockholm')::date = '2026-01-15'`); n != 1 {
		t.Error("the odometer reading was not recorded with its date and source")
	}
	// The reading is on the vehicle's own history, with the plate as written.
	if n := countRows(t, pool, `SELECT count(*) FROM vehicle_registrations WHERE registration = 'abc 123'`); n != 1 {
		t.Error("the plate was not kept as written")
	}
}

func TestOpeningStockIsADatedLedgerEntryThatIsNeverAddedTwice(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	parts, err := workshop.PreviewImport(ctx, pool, owner(), "parts", "delar.csv", []byte("number;name;unit;cost;price\nBP-100;Bromsklossar;st;450,00;650,00\nOIL-1;Olja;l;89,50;129\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.ApplyImport(ctx, pool, owner(), parts.ID, nil, false); err != nil {
		t.Fatal(err)
	}

	file := "part_number;quantity;as_of;unit_cost\nBP-100;6;2026-01-31;450,00\nOIL-1;48,5;2026-01-31;\nNOPE-1;3;2026-01-31;\nBP-100;1;2026-01-31;\nBP-100;1;2099-01-01;\n"
	b, err := workshop.PreviewImport(ctx, pool, owner(), "stock", "lager.csv", []byte(file))
	if err != nil {
		t.Fatal(err)
	}
	if b.WouldCreate() != 2 || b.WouldRefuse() != 3 {
		t.Fatalf("would create %d, refuse %d: %+v", b.WouldCreate(), b.WouldRefuse(), b.Rows)
	}
	if r := b.Rows[2]; !strings.Contains(strings.Join(r.Errors, ";"), "import the parts first") {
		t.Errorf("an unknown part = %+v", r)
	}
	if _, err := workshop.ApplyImport(ctx, pool, owner(), b.ID, nil, true); err != nil {
		t.Fatal(err)
	}
	var onHand string
	if err := pool.QueryRow(ctx, `
		SELECT sum(quantity)::text FROM stock_movements m JOIN parts p ON p.id = m.part_id WHERE p.number = 'OIL-1'`).Scan(&onHand); err != nil || onHand != "48.500" {
		t.Errorf("on hand = %s, %v", onHand, err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM stock_movements m WHERE m.import_batch_id = $1 AND m.kind = 'counted' AND m.moved_by IS NOT NULL AND m.note LIKE 'Opening stock%'`, b.ID); n != 2 {
		t.Errorf("%d ledger entries with provenance, want 2", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM stock_movements WHERE (moved_at AT TIME ZONE 'Europe/Stockholm')::date = '2026-01-31'`); n != 2 {
		t.Errorf("the entries are not dated the day the file says")
	}

	// A different file for the same part and day does not stack.
	again, _ := workshop.PreviewImport(ctx, pool, owner(), "stock", "lager2.csv", []byte("part_number;quantity;as_of\nBP-100;6;2026-01-31\nOIL-1;50;2026-01-31\n"))
	if again.WouldSkip() != 1 || again.WouldRefuse() != 1 || again.WouldCreate() != 0 {
		t.Errorf("a second file for the same day: skip %d refuse %d create %d", again.WouldSkip(), again.WouldRefuse(), again.WouldCreate())
	}
}

func TestPartsThatCollideAreSaidAndAnInterruptedBatchLeavesNothing(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	if _, err := workshop.SavePart(ctx, pool, owner(), workshop.CatalogueEntry{Number: "EXISTS-1", Name: "Redan här", Unit: "each", Codes: []string{"EAN-9"}}); err != nil {
		t.Fatal(err)
	}
	file := "number;name;unit\nEAN-9;Samma som en kod;st\nEXISTS-1;Redan här;l\nNEW-1;Ny;st\nNEW-2;Också ny;st\n"
	b, err := workshop.PreviewImport(ctx, pool, owner(), "parts", "delar.csv", []byte(file))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(b.Rows[0].Errors, ";"), "already a code") || !strings.Contains(strings.Join(b.Rows[1].Errors, ";"), "unit") {
		t.Errorf("collisions = %v | %v", b.Rows[0].Errors, b.Rows[1].Errors)
	}

	// Between the preview and the apply somebody adds NEW-2 by hand: the batch
	// fails on it and NEW-1, written before it, goes with it.
	if _, err := workshop.SavePart(ctx, pool, owner(), workshop.CatalogueEntry{Number: "NEW-2", Name: "Hann före", Unit: "each"}); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.ApplyImport(ctx, pool, owner(), b.ID, nil, true); err == nil {
		t.Fatal("a batch that could not be written was applied")
	}
	if n := countRows(t, pool, `SELECT count(*) FROM parts WHERE number = 'NEW-1'`); n != 0 {
		t.Error("half a batch was written")
	}
	after, _ := workshop.ImportBatchByID(ctx, pool, owner(), b.ID)
	if after.Status != "previewed" || len(after.Rows) == 0 || after.Rows[3].Values == nil {
		t.Errorf("the batch after a failure = %s, %d rows: it should be where it was", after.Status, len(after.Rows))
	}
}

func TestOnlyWhoRunsTheShopMayImportAndAForgottenPreviewIsWiped(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	file := []byte("ref;type;name;email\nK-1;private;Dag Dahl;dag@example.se\n")
	for _, who := range []access.Scope{advisor(), technician(), partsDesk()} {
		if _, err := workshop.PreviewImport(ctx, pool, who, "customers", "a.csv", file); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s previewed an import: %v", who.Role, err)
		}
		if _, err := workshop.ImportBatches(ctx, pool, who); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s listed imports: %v", who.Role, err)
		}
	}
	if n := countRows(t, pool, `SELECT count(*) FROM import_batches`); n != 0 {
		t.Fatalf("a refused preview left %d batches behind", n)
	}
	b, err := workshop.PreviewImport(ctx, pool, owner(), "customers", "a.csv", file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.ApplyImport(ctx, pool, advisor(), b.ID, nil, false); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("an advisor applied an import: %v", err)
	}
	if err := workshop.DiscardImport(ctx, pool, advisor(), b.ID); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("an advisor discarded an import: %v", err)
	}

	// Left for more than a week, it is wiped and discarded.
	if _, err := pool.Exec(ctx, `UPDATE import_batches SET created_at = now() - interval '8 days'`); err != nil {
		t.Fatal(err)
	}
	r, err := workshop.Sweep(ctx, pool, shopID)
	if err != nil || r.ImportPreviews != 1 {
		t.Fatalf("sweep = %+v, %v", r, err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM import_rows WHERE data IS NOT NULL OR info IS NOT NULL`); n != 0 {
		t.Error("an old preview kept the file's contents")
	}
	gone, _ := workshop.ImportBatchByID(ctx, pool, owner(), b.ID)
	if gone.Status != "discarded" {
		t.Errorf("status = %s", gone.Status)
	}
	// And the file can be uploaded again.
	if again, err := workshop.PreviewImport(ctx, pool, owner(), "customers", "a.csv", file); err != nil || again.Existing {
		t.Errorf("uploading a discarded file again = %+v, %v", again, err)
	}
}
