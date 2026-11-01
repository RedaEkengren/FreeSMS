package workshop_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Document records (#150). The shop runs every module by its scope, as a
// session in such a shop would carry.

const purchaseA = "77777777-0000-0000-0000-000000000001"

func buyerScope() access.Scope { return otherRoles(access.Of(access.RoleBuyer)) }

func purchaseTemplate(t *testing.T, pool *pgxpool.Pool) workshop.DocumentTemplate {
	t.Helper()
	tpl, err := workshop.SaveDocumentTemplate(context.Background(), pool, owner(), "", workshop.DocPurchaseAgreement,
		"Köpeavtal", "{seller} säljer {registration} till handlaren för {price}.")
	if err != nil {
		t.Fatalf("SaveDocumentTemplate: %v", err)
	}
	return tpl
}

// theSeller is the private seller, made the first time it is asked for.
func theSeller(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(), `
		SELECT pa.id::text FROM parties pa JOIN people p ON p.id = pa.person_id WHERE p.display_name = 'Sven Säljare'`).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return privateSeller(t, pool)
	}
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// theCustomerAsSeller is the dealer's workshop customer's party, given by
// the front desk the seller's role the buyer deals in, so the buyer may name
// it on a contract.
func theCustomerAsSeller(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	id := partyOf(t, pool, fixtureCustomer)
	if err := workshop.StartPartyRole(context.Background(), pool, everything(advisor()), id, workshop.PartySeller, ""); err != nil &&
		!errors.Is(err, workshop.ErrInvalid) {
		t.Fatalf("StartPartyRole: %v", err)
	}
	return id
}

// purchaseAgreement is a seller's purchase agreement, made by the buyer,
// with the dealer's customer on it as guarantor so it has two parties.
func purchaseAgreement(t *testing.T, pool *pgxpool.Pool, tpl workshop.DocumentTemplate, supersedes string) string {
	t.Helper()
	id, err := workshop.ProduceDocument(context.Background(), pool, buyerScope(), workshop.NewDocumentRecord{
		Kind: workshop.DocPurchaseAgreement, SubjectKind: "purchase", SubjectID: purchaseA,
		TemplateVersionID: tpl.VersionID, Title: "Köpeavtal ABC 12D",
		Parties: []workshop.DocumentPartyRole{
			{PartyID: theSeller(t, pool), Role: "seller"},
			{PartyID: theCustomerAsSeller(t, pool), Role: "guarantor"},
		},
		Data:       map[string]string{"seller": "Sven Säljare", "registration": "ABC 12D", "price": "85 000 kr"},
		Supersedes: supersedes,
	})
	if err != nil {
		t.Fatalf("ProduceDocument: %v", err)
	}
	return id
}

// A template version never changes: editing writes the next, and a
// document says which one it was made from, rendered the same way from the
// same data; the document itself is frozen by the database.
func TestADocumentIsFrozenFromTheTemplateVersionItWasMadeFrom(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	tpl := purchaseTemplate(t, pool)
	preview, err := workshop.PreviewDocument(ctx, pool, buyerScope(), workshop.DocPurchaseAgreement, tpl.VersionID,
		map[string]string{"seller": "Sven Säljare", "registration": "ABC 12D", "price": "85 000 kr"})
	if err != nil {
		t.Fatal(err)
	}
	// The template is edited between the preview and producing the document.
	v2, err := workshop.SaveDocumentTemplate(ctx, pool, owner(), tpl.ID, workshop.DocPurchaseAgreement, "Köpeavtal", "Ny text {seller}.")
	if err != nil || v2.Version != 2 || v2.VersionID == tpl.VersionID {
		t.Fatalf("an edit: %+v, %v; want version 2", v2, err)
	}
	id := purchaseAgreement(t, pool, tpl, "")
	d, err := workshop.DocumentRecordByID(ctx, pool, buyerScope(), id)
	if err != nil {
		t.Fatal(err)
	}
	if d.Body != preview || d.Body != "Sven Säljare säljer ABC 12D till handlaren för 85 000 kr." || d.TemplateVersion != 1 {
		t.Errorf("the document says %q from version %d; the preview said %q", d.Body, d.TemplateVersion, preview)
	}
	if len(d.Parties) != 2 || d.Parties[0].Name != "Sven Säljare" || d.Parties[0].Address == "" || len(d.Hash) != 64 {
		t.Errorf("parties %+v, hash %q", d.Parties, d.Hash)
	}
	for _, stmt := range []string{
		`UPDATE document_records SET body = 'ändrad' WHERE id = $1`,
		`UPDATE document_records SET title = 'annan' WHERE id = $1`,
		`DELETE FROM document_records WHERE id = $1`,
		`UPDATE document_record_parties SET name = 'någon' WHERE document_id = $1`,
		`DELETE FROM document_record_parties WHERE document_id = $1`,
		`UPDATE document_template_versions SET body = 'x' WHERE id = (SELECT template_version_id FROM document_records WHERE id = $1)`,
	} {
		if err := database.InScope(ctx, pool, owner(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, stmt, id)
			return err
		}); err == nil {
			t.Errorf("%q was allowed on a produced document", stmt)
		}
		// By trigger, not only by privilege: the database's owner is
		// refused too.
		if _, err := pool.Exec(ctx, stmt, id); err == nil {
			t.Errorf("%q was allowed to the database's owner", stmt)
		}
	}
	// A party's later move does not move the contract.
	seller := d.Parties[0].PartyID
	p, _ := workshop.PartyByID(ctx, pool, buyerScope(), seller)
	if _, err := workshop.EditParty(ctx, pool, buyerScope(), seller, p.Revision, workshop.PartyEdit{
		Name: "Sven Säljare", AddressLine1: "Nygatan 9", City: "Malmö"}); err != nil {
		t.Fatal(err)
	}
	if again, _ := workshop.DocumentRecordByID(ctx, pool, buyerScope(), id); again.Parties[0].Address != d.Parties[0].Address {
		t.Errorf("the contract's address followed the party to %q", again.Parties[0].Address)
	}
}

// Signing is rows of evidence: one party, then the other; a hash that is
// not the document's is refused, a second scan of a signed party adds
// nothing, and a superseded document is not signed -- its correction is.
func TestSigningIsEvidenceOfTheDocumentAsItIs(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	tpl := purchaseTemplate(t, pool)
	id := purchaseAgreement(t, pool, tpl, "")
	d, _ := workshop.DocumentRecordByID(ctx, pool, buyerScope(), id)
	if d.Signing() != "unsigned" {
		t.Errorf("a new document is %s", d.Signing())
	}
	seller, guarantor := d.Parties[0].ID, d.Parties[1].ID
	paper := workshop.SignatureEvidence{Method: workshop.SignedOnPaper, Hash: d.Hash, EvidenceKey: "scan-1", WitnessedBy: "Anna på kontoret"}

	wrong := paper
	wrong.Hash = "00" + d.Hash[2:]
	if err := workshop.RecordSignature(ctx, pool, buyerScope(), id, seller, wrong); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a signature of another hash: %v, want refused", err)
	}
	if err := workshop.RecordSignature(ctx, pool, buyerScope(), id, seller, workshop.SignatureEvidence{Method: workshop.SignedOnPaper, Hash: d.Hash}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a paper signature with no scan and no witness: %v", err)
	}
	if err := workshop.RecordSignature(ctx, pool, buyerScope(), id, seller, paper); err != nil {
		t.Fatal(err)
	}
	if err := workshop.RecordSignature(ctx, pool, buyerScope(), id, seller, paper); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("the same scan a second time: %v, want refused", err)
	}
	if d, _ = workshop.DocumentRecordByID(ctx, pool, buyerScope(), id); d.Signing() != "partly signed" || d.Parties[0].Signature.WitnessedBy != "Anna på kontoret" {
		t.Errorf("after one of two: %s, %+v", d.Signing(), d.Parties[0].Signature)
	}

	// Corrected before the second party signs: the old one stays readable
	// and is not signed again; the new one is.
	fixed := purchaseAgreement(t, pool, tpl, id)
	if err := workshop.RecordSignature(ctx, pool, buyerScope(), id, guarantor, paper); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("signing a superseded document: %v, want refused", err)
	}
	if old, err := workshop.DocumentRecordByID(ctx, pool, buyerScope(), id); err != nil || old.SupersededByID != fixed || old.Body == "" {
		t.Errorf("the superseded document reads %+v, %v", old, err)
	}
	if _, err := workshop.ProduceDocument(ctx, pool, buyerScope(), workshop.NewDocumentRecord{
		Kind: workshop.DocPurchaseAgreement, SubjectKind: "purchase", SubjectID: purchaseA, TemplateVersionID: tpl.VersionID,
		Title: "x", Parties: []workshop.DocumentPartyRole{{PartyID: theCustomerAsSeller(t, pool), Role: "seller"}}, Supersedes: id,
	}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a second correction of the same document: %v, want refused", err)
	}
	n, _ := workshop.DocumentRecordByID(ctx, pool, buyerScope(), fixed)
	for _, p := range n.Parties {
		if err := workshop.RecordSignature(ctx, pool, buyerScope(), fixed, p.ID, workshop.SignatureEvidence{
			Method: workshop.SignedInPerson, Hash: n.Hash}); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ = workshop.DocumentRecordByID(ctx, pool, buyerScope(), fixed); n.Signing() != "fully signed" {
		t.Errorf("the correction, both signed: %s", n.Signing())
	}
}

type downSigner struct{}

func (downSigner) Request(context.Context, workshop.DocumentRecord, workshop.DocumentParty, string) (string, error) {
	return "", errors.New("connection refused")
}

// A signing service that is down leaves the document unsigned and says
// so; it is then signed on paper, and nothing waited for the service.
func TestASigningServiceThatIsDownLeavesTheDocumentUnsigned(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	id := purchaseAgreement(t, pool, purchaseTemplate(t, pool), "")
	d, _ := workshop.DocumentRecordByID(ctx, pool, buyerScope(), id)
	if _, err := workshop.RequestSignature(ctx, pool, buyerScope(), downSigner{}, id, d.Parties[0].ID); !errors.Is(err, workshop.ErrSignerUnavailable) {
		t.Errorf("a signer that is down: %v, want ErrSignerUnavailable", err)
	}
	if d, _ = workshop.DocumentRecordByID(ctx, pool, buyerScope(), id); d.Signing() != "unsigned" {
		t.Errorf("after the signer failed: %s", d.Signing())
	}
	if ref, err := workshop.RequestSignature(ctx, pool, buyerScope(), workshop.ManualSigner{}, id, d.Parties[0].ID); err != nil || ref != "" {
		t.Errorf("the manual signer: %q, %v", ref, err)
	}
	if err := workshop.RecordSignature(ctx, pool, buyerScope(), id, d.Parties[0].ID, workshop.SignatureEvidence{
		Method: workshop.SignedOnPaper, Hash: d.Hash, WitnessedBy: "Bo"}); err != nil {
		t.Errorf("signed on paper instead: %v", err)
	}
}

// Read per kind and role: the buyer reads the purchase agreements, the
// salesperson and the workshop do not; producing needs the module, and
// reading what was made does not.
func TestADocumentIsReadByThoseWhoMadeItsKind(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	id := purchaseAgreement(t, pool, purchaseTemplate(t, pool), "")
	for name, s := range map[string]access.Scope{
		"sales": otherRoles(access.Of(access.RoleSales)), "rental desk": rentalDesk(),
		"front desk": everything(advisor()), "technician": everything(technician()),
	} {
		if _, err := workshop.DocumentRecordByID(ctx, pool, s, id); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s read a purchase agreement: %v", name, err)
		}
	}
	if _, err := workshop.DocumentRecordByID(ctx, pool, everything(owner()), id); err != nil {
		t.Errorf("the owner: %v", err)
	}
	off := buyerScope()
	off.Modules = []access.Module{access.Workshop}
	if _, err := workshop.DocumentRecordByID(ctx, pool, off, id); err != nil {
		t.Errorf("the buyer reading with car sales off: %v", err)
	}
	if _, err := workshop.ProduceDocument(ctx, pool, off, workshop.NewDocumentRecord{Kind: workshop.DocPurchaseAgreement}); !errors.Is(err, access.ErrModuleOff) {
		t.Errorf("a purchase agreement made with car sales off: %v", err)
	}
	if _, err := workshop.SaveDocumentTemplate(ctx, pool, buyerScope(), "", workshop.DocPurchaseAgreement, "x", "y"); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the buyer wrote the shop's template: %v", err)
	}
}

// The customer's link opens the document as produced, without who at the
// shop recorded what; a wrong token is no answer at all.
func TestTheCustomersLinkOpensTheDocument(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	id := purchaseAgreement(t, pool, purchaseTemplate(t, pool), "")
	token, err := workshop.CreateDocumentRecordLink(ctx, pool, buyerScope(), id)
	if err != nil {
		t.Fatal(err)
	}
	d, err := workshop.OpenDocumentRecordLink(ctx, pool, shopID, token)
	if err != nil || d.ID != id || d.CreatedBy != "" {
		t.Errorf("the link opens %+v, %v", d, err)
	}
	if _, err := workshop.OpenDocumentRecordLink(ctx, pool, shopID, token+"x"); !errors.Is(err, workshop.ErrShareNotUsable) {
		t.Errorf("a wrong token: %v", err)
	}
	again, _ := workshop.CreateDocumentRecordLink(ctx, pool, buyerScope(), id)
	if _, err := workshop.OpenDocumentRecordLink(ctx, pool, shopID, token); !errors.Is(err, workshop.ErrShareNotUsable) {
		t.Errorf("the first link after a second was made: %v, want closed", err)
	}
	if _, err := workshop.OpenDocumentRecordLink(ctx, pool, shopID, again); err != nil {
		t.Errorf("the second link: %v", err)
	}
}

// Erasure blanks a contract past its kind's retention -- what it said,
// the parties as named -- and keeps that it existed, its hash and its
// signatures; one within its retention it keeps, and the erasure says so.
func TestErasureBlanksOnlyContractsPastTheirRetention(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	tpl := purchaseTemplate(t, pool)
	old := purchaseAgreement(t, pool, tpl, "")
	recent, err := workshop.ProduceDocument(ctx, pool, buyerScope(), workshop.NewDocumentRecord{
		Kind: workshop.DocPurchaseAgreement, SubjectKind: "purchase", SubjectID: "77777777-0000-0000-0000-000000000002",
		TemplateVersionID: tpl.VersionID, Title: "Nyare",
		Parties: []workshop.DocumentPartyRole{{PartyID: theCustomerAsSeller(t, pool), Role: "seller"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := workshop.DocumentRecordByID(ctx, pool, buyerScope(), old)
	if err := workshop.RecordSignature(ctx, pool, buyerScope(), old, d.Parties[1].ID, workshop.SignatureEvidence{
		Method: workshop.SignedOnPaper, Hash: d.Hash, WitnessedBy: "Bo"}); err != nil {
		t.Fatal(err)
	}
	if err := workshop.SetDocumentRetention(ctx, pool, owner(), workshop.DocPurchaseAgreement, 2); err != nil {
		t.Fatal(err)
	}
	if r, _ := workshop.DocumentRetention(ctx, pool, owner()); r[workshop.DocPurchaseAgreement] != 2 || r[workshop.DocRentalAgreement] != 7 {
		t.Errorf("retention = %v", r)
	}
	// The older one made three years ago.
	if _, err := pool.Exec(ctx, `ALTER TABLE document_records DISABLE TRIGGER document_records_are_frozen`); err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `UPDATE document_records SET created_at = now() - interval '3 years' WHERE id = $1`, old)
	if _, e := pool.Exec(ctx, `ALTER TABLE document_records ENABLE TRIGGER document_records_are_frozen`); e != nil || err != nil {
		t.Fatal(err, e)
	}

	e, err := workshop.ExportPerson(ctx, pool, advisor(), customerPersonID)
	if err != nil || len(e.Contracts) != 2 || !e.Contracts[0].Signed || e.Contracts[0].Text == "" {
		t.Errorf("the person's export lists the contracts naming them as %+v, %v", e.Contracts, err)
	}
	if err := workshop.ErasePerson(ctx, pool, advisor(), &recordingRemover{}, customerPersonID, "Begäran"); err != nil {
		t.Fatal(err)
	}
	gone, _ := workshop.DocumentRecordByID(ctx, pool, buyerScope(), old)
	if gone.ErasedAt == nil || gone.Body != "" || gone.Data != nil || gone.Hash != d.Hash || gone.Parties[1].Name != workshop.ErasedName ||
		gone.Parties[1].Signature == nil {
		t.Errorf("past its retention: %+v", gone)
	}
	if kept, _ := workshop.DocumentRecordByID(ctx, pool, buyerScope(), recent); kept.ErasedAt != nil || kept.Body == "" {
		t.Errorf("within its retention: %+v", kept)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM erasures WHERE cleared LIKE '%1 contracts past their retention%' AND kept LIKE '%1 contracts still within%'`); n != 1 {
		t.Error("the erasure does not say which contracts it blanked and which it kept")
	}
}

// A contract copies its parties' names and addresses onto itself, so naming
// a party is reading it: a salesperson cannot put a workshop customer they
// do not deal with onto a sales contract and read back who they are. Once
// the front desk has made the customer a buyer -- the salesperson's own
// dealing -- they can.
func TestADocumentNamesOnlyPartiesTheCallerMaySee(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	sales := otherRoles(access.Of(access.RoleSales))
	tpl, err := workshop.SaveDocumentTemplate(ctx, pool, owner(), "", workshop.DocSalesContract, "Köpeavtal", "{buyer} köper bilen.")
	if err != nil {
		t.Fatal(err)
	}
	customer := partyOf(t, pool, fixtureCustomer)
	contract := workshop.NewDocumentRecord{
		Kind: workshop.DocSalesContract, SubjectKind: "sale", SubjectID: "77777777-0000-0000-0000-000000000009",
		TemplateVersionID: tpl.VersionID, Title: "Köpeavtal",
		Parties: []workshop.DocumentPartyRole{{PartyID: customer, Role: "buyer"}},
	}
	if _, err := workshop.ProduceDocument(ctx, pool, sales, contract); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("a salesperson named a workshop customer on a contract: %v, want not found", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM document_records`); n != 0 {
		t.Errorf("%d documents produced, want none", n)
	}
	if err := workshop.StartPartyRole(ctx, pool, sales, customer, workshop.PartyBuyer, ""); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("a salesperson made a workshop customer a buyer: %v, want not found", err)
	}
	if err := workshop.StartPartyRole(ctx, pool, everything(advisor()), customer, workshop.PartyBuyer, ""); err != nil {
		t.Fatal(err)
	}
	id, err := workshop.ProduceDocument(ctx, pool, sales, contract)
	if err != nil {
		t.Fatalf("the customer, now a buyer: %v", err)
	}
	d, err := workshop.DocumentRecordByID(ctx, pool, sales, id)
	want, _ := workshop.PartyByID(ctx, pool, sales, customer)
	if err != nil || len(d.Parties) != 1 || d.Parties[0].Name != want.Name || d.Parties[0].Address != want.Address || want.Name == "" {
		t.Errorf("the contract names %+v, %v; want the party as it is read, %+v", d.Parties, err, want)
	}
}

// Erasing one person from a contract past its retention takes them off it,
// and only them: the other party stays named. Their name goes from the
// title as well, and the scan of the signed paper copy -- which shows the
// whole contract, them included, and cannot be blanked in part -- goes with
// the text, file and all. Erasing the other party later takes them off the
// same contract.
func TestErasingOnePersonTakesOnlyThemOffAContract(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	tpl := purchaseTemplate(t, pool)
	seller := theSeller(t, pool)
	id, err := workshop.ProduceDocument(ctx, pool, buyerScope(), workshop.NewDocumentRecord{
		Kind: workshop.DocPurchaseAgreement, SubjectKind: "purchase", SubjectID: purchaseA,
		TemplateVersionID: tpl.VersionID, Title: "Köpeavtal ABC 12D – Sven Säljare",
		Parties: []workshop.DocumentPartyRole{
			{PartyID: seller, Role: "seller"},
			{PartyID: theCustomerAsSeller(t, pool), Role: "guarantor"},
		},
		Data: map[string]string{"seller": "Sven Säljare", "registration": "ABC 12D", "price": "85 000 kr"},
	})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := workshop.DocumentRecordByID(ctx, pool, buyerScope(), id)
	guarantor := d.Parties[1].Name
	if err := workshop.RecordSignature(ctx, pool, buyerScope(), id, d.Parties[0].ID, workshop.SignatureEvidence{
		Method: workshop.SignedOnPaper, Hash: d.Hash, EvidenceKey: "scan-sven.jpg"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE document_records DISABLE TRIGGER document_records_are_frozen`); err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `UPDATE document_records SET created_at = now() - interval '8 years' WHERE id = $1`, id)
	if _, e := pool.Exec(ctx, `ALTER TABLE document_records ENABLE TRIGGER document_records_are_frozen`); e != nil || err != nil {
		t.Fatal(err, e)
	}
	var sellerPerson string
	if err := pool.QueryRow(ctx, `SELECT person_id::text FROM parties WHERE id = $1`, seller).Scan(&sellerPerson); err != nil {
		t.Fatal(err)
	}

	files := &recordingRemover{}
	if err := workshop.ErasePerson(ctx, pool, advisor(), files, sellerPerson, "Begäran"); err != nil {
		t.Fatal(err)
	}
	gone, err := workshop.DocumentRecordByID(ctx, pool, buyerScope(), id)
	if err != nil || gone.ErasedAt == nil || gone.Body != "" {
		t.Fatalf("past its retention: %+v, %v", gone, err)
	}
	if gone.Parties[0].Name != workshop.ErasedName || gone.Parties[0].Address != "" {
		t.Errorf("the erased seller is still named: %+v", gone.Parties[0])
	}
	if gone.Parties[1].Name != guarantor || guarantor == "" {
		t.Errorf("the guarantor, who asked for nothing, is named %q, want %q", gone.Parties[1].Name, guarantor)
	}
	if strings.Contains(gone.Title, "Sven") || strings.Contains(gone.Title, "Säljare") || !strings.Contains(gone.Title, "ABC 12D") {
		t.Errorf("the title after erasure: %q", gone.Title)
	}
	if s := gone.Parties[0].Signature; s == nil || s.EvidenceKey != "" || s.Method != workshop.SignedOnPaper {
		t.Errorf("the signature after erasure: %+v; want it kept, without its scan", s)
	}
	if len(files.removed) != 1 || files.removed[0] != "scan-sven.jpg" {
		t.Errorf("files removed: %v; want the scan", files.removed)
	}

	if err := workshop.ErasePerson(ctx, pool, advisor(), files, customerPersonID, "Begäran"); err != nil {
		t.Fatal(err)
	}
	if again, _ := workshop.DocumentRecordByID(ctx, pool, buyerScope(), id); again.Parties[1].Name != workshop.ErasedName {
		t.Errorf("the guarantor erased later is still named on the erased contract: %+v", again.Parties[1])
	}
}
