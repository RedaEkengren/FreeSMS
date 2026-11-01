package workshop_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/identity"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Identity records (#153). The shop runs every module by its scope.

const seededNumber = "19800101-4321"

func identityKeys(t *testing.T, ids ...string) *identity.Keyring {
	t.Helper()
	var spec []string
	for _, id := range ids {
		k, err := identity.NewKey(id)
		if err != nil {
			t.Fatal(err)
		}
		spec = append(spec, k)
	}
	keys, err := identity.ParseKeys(strings.Join(spec, ","))
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func sales() access.Scope { return otherRoles(access.Of(access.RoleSales)) }

// A number is recorded for a purpose on the list, sealed: nothing in the
// database holds it. Read masked by default, in full on request, by a role
// that may; each read an event in the same transaction. Another role sees
// that a check was made, by whom and when, and nothing more.
func TestAnIdentityNumberIsSealedAndEveryReadIsLogged(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	keys := identityKeys(t, "k1")
	party := partyOf(t, pool, fixtureCustomer)
	if err := workshop.StartPartyRole(ctx, pool, everything(advisor()), party, workshop.PartyBuyer, ""); err != nil {
		t.Fatal(err)
	}
	record := func(s access.Scope, n workshop.NewIdentityRecord) (string, error) {
		n.PartyID = party
		return workshop.RecordIdentity(ctx, pool, s, keys, n)
	}
	if _, err := record(sales(), workshop.NewIdentityRecord{Kind: workshop.IdentityNumber, Purpose: "marketing", Value: seededNumber}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a purpose with no legal basis: %v", err)
	}
	if _, err := record(sales(), workshop.NewIdentityRecord{Kind: workshop.IdentityLicence, Purpose: "aml_due_diligence", Reference: "x"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a kind the purpose does not need: %v", err)
	}
	if _, err := record(advisor(), workshop.NewIdentityRecord{Kind: workshop.IdentityNumber, Purpose: "aml_due_diligence", Value: seededNumber}); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the workshop's front desk recorded a number: %v", err)
	}
	if _, err := workshop.RecordIdentity(ctx, pool, sales(), nil, workshop.NewIdentityRecord{PartyID: party,
		Kind: workshop.IdentityNumber, Purpose: "aml_due_diligence", Value: seededNumber}); !errors.Is(err, workshop.ErrIdentityKey) {
		t.Errorf("recorded with no key: %v", err)
	}
	id, err := record(sales(), workshop.NewIdentityRecord{Kind: workshop.IdentityNumber, Purpose: "aml_due_diligence", Value: seededNumber})
	if err != nil {
		t.Fatal(err)
	}
	// Nothing in the database says what it was.
	var dump string
	if err := pool.QueryRow(ctx, `SELECT to_jsonb(r)::text || encode(sealed, 'escape') FROM identity_records r WHERE id = $1`, id).Scan(&dump); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dump, seededNumber) || strings.Contains(dump, "4321") {
		t.Errorf("the database holds the value: %s", dump)
	}

	masked, err := workshop.ReadIdentityValue(ctx, pool, sales(), keys, id, false)
	if err != nil || masked != "•••••••••4321" {
		t.Errorf("masked: %q, %v", masked, err)
	}
	full, err := workshop.ReadIdentityValue(ctx, pool, sales(), keys, id, true)
	if err != nil || full != seededNumber {
		t.Errorf("in full: %q, %v", full, err)
	}
	events, err := workshop.AuditTrail(ctx, pool, sales(), "identity_record", id)
	if err != nil || len(events) != 2 || events[0].Event != "identity.read" || events[1].Detail["full"] != true ||
		events[1].Detail["purpose"] != "aml_due_diligence" || events[1].Actor == "" {
		t.Errorf("the reads' events: %+v, %v", events, err)
	}
	if _, err := workshop.ReadIdentityValue(ctx, pool, advisor(), keys, id, false); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the front desk read a number: %v", err)
	}
	listed, err := workshop.IdentityRecordsFor(ctx, pool, advisor(), party)
	if err != nil || len(listed) != 1 || !listed[0].HasValue || listed[0].RecordedBy == "" {
		t.Errorf("the front desk's list: %+v, %v", listed, err)
	}
	// Without its key: refused, never in the clear.
	if _, err := workshop.ReadIdentityValue(ctx, pool, sales(), identityKeys(t, "other"), id, true); !errors.Is(err, workshop.ErrIdentityKey) {
		t.Errorf("read without its key: %v", err)
	}
	if _, err := workshop.ReadIdentityValue(ctx, pool, sales(), nil, id, true); !errors.Is(err, workshop.ErrIdentityKey) {
		t.Errorf("read with no key: %v", err)
	}

	// A licence check keeps a reference only.
	desk := otherRoles(access.Of(access.RoleRentalAgent))
	if err := workshop.StartPartyRole(ctx, pool, everything(advisor()), party, workshop.PartyRenter, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.RecordIdentity(ctx, pool, desk, keys, workshop.NewIdentityRecord{PartyID: party,
		Kind: workshop.IdentityLicence, Purpose: "licence_check", Value: "B 1234567"}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a licence copied in full: %v", err)
	}
	if _, err := workshop.RecordIdentity(ctx, pool, desk, keys, workshop.NewIdentityRecord{PartyID: party,
		Kind: workshop.IdentityLicence, Purpose: "licence_check", Reference: "Körkort kontrollerat, giltigt"}); err != nil {
		t.Errorf("a licence check by reference: %v", err)
	}
}

// The value never reaches the shop's export in the clear, the person's
// export, the trail or search: a test reads them all for it.
func TestAnIdentityValueIsInNoExportTrailOrSearch(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	keys := identityKeys(t, "k1")
	party := partyOf(t, pool, fixtureCustomer)
	if err := workshop.StartPartyRole(ctx, pool, everything(advisor()), party, workshop.PartyBuyer, ""); err != nil {
		t.Fatal(err)
	}
	id, err := workshop.RecordIdentity(ctx, pool, sales(), keys, workshop.NewIdentityRecord{PartyID: party,
		Kind: workshop.IdentityNumber, Purpose: "ownership_change", Value: seededNumber})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.ReadIdentityValue(ctx, pool, sales(), keys, id, true); err != nil {
		t.Fatal(err)
	}
	var zipped bytes.Buffer
	if err := workshop.WriteShopExport(ctx, pool, owner(), &zipped, fakePhotos{}, "test"); err != nil {
		t.Fatal(err)
	}
	z, err := zip.NewReader(bytes.NewReader(zipped.Bytes()), int64(zipped.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var sawRecords, sawNote bool
	for _, f := range z.File {
		r, _ := f.Open()
		b, _ := io.ReadAll(r)
		r.Close()
		if bytes.Contains(b, []byte(seededNumber)) || bytes.Contains(b, []byte("19800101")) {
			t.Errorf("%s holds the value", f.Name)
		}
		sawRecords = sawRecords || f.Name == "identity_records.csv"
		sawNote = sawNote || (f.Name == "README.txt" && bytes.Contains(b, []byte("identity_records")))
	}
	if !sawRecords || !sawNote {
		t.Errorf("the export has the records %v and says how to open them %v", sawRecords, sawNote)
	}
	e, err := workshop.ExportPerson(ctx, pool, advisor(), customerPersonID)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := workshop.MarshalExport(e); bytes.Contains(b, []byte(seededNumber)) {
		t.Error("the person's export holds the value")
	}
	events, _ := workshop.AuditTrail(ctx, pool, sales(), "identity_record", id)
	for _, ev := range events {
		for _, v := range ev.Detail {
			if s, ok := v.(string); ok && strings.Contains(s, "4321") {
				t.Errorf("the trail holds the value: %+v", ev)
			}
		}
	}
	if r, _ := workshop.Search(ctx, pool, advisor(), seededNumber); len(r) != 0 {
		t.Errorf("search found %+v by the value", r)
	}
}

// At expiry the value goes and the record that a check was made stays; a
// purpose ended early removes it then. Erasure keeps what the law has the
// shop keep, until it expires, and removes the rest now.
func TestAnIdentityValueGoesWhenItsPurposeDoes(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	keys := identityKeys(t, "k1")
	party := partyOf(t, pool, fixtureCustomer)
	if err := workshop.StartPartyRole(ctx, pool, everything(advisor()), party, workshop.PartyBuyer, ""); err != nil {
		t.Fatal(err)
	}
	rec := func(purpose string) string {
		id, err := workshop.RecordIdentity(ctx, pool, sales(), keys, workshop.NewIdentityRecord{PartyID: party,
			Kind: workshop.IdentityNumber, Purpose: purpose, Value: seededNumber})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	old, ended, legal := rec("ownership_change"), rec("ownership_change"), rec("aml_due_diligence")
	if _, err := pool.Exec(ctx, `ALTER TABLE identity_records DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	_, err := pool.Exec(ctx, `UPDATE identity_records SET recorded_at = now() - interval '1 year', expires_at = now() - interval '1 day' WHERE id = $1`, old)
	if _, e := pool.Exec(ctx, `ALTER TABLE identity_records ENABLE TRIGGER USER`); e != nil || err != nil {
		t.Fatal(err, e)
	}
	if _, err := workshop.Sweep(ctx, pool, shopID); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.ReadIdentityValue(ctx, pool, sales(), keys, old, true); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("an expired value read: %v", err)
	}
	if err := workshop.EndIdentityPurpose(ctx, pool, sales(), ended, ""); !errors.Is(err, workshop.ErrIdentityEndNoReason) {
		t.Errorf("ended with no reason: %v", err)
	}
	if err := workshop.EndIdentityPurpose(ctx, pool, sales(), ended, "Affären blev inte av"); err != nil {
		t.Fatal(err)
	}
	listed, _ := workshop.IdentityRecordsFor(ctx, pool, sales(), party)
	removed := 0
	for _, r := range listed {
		if r.ValueRemoved {
			removed++
		}
	}
	if len(listed) != 3 || removed != 2 {
		t.Errorf("after expiry and an end: %d records, %d removed; want 3 and 2", len(listed), removed)
	}
	if err := workshop.ErasePerson(ctx, pool, advisor(), &recordingRemover{}, customerPersonID, "Begäran"); err != nil {
		t.Fatal(err)
	}
	if v, err := workshop.ReadIdentityValue(ctx, pool, sales(), keys, legal, true); err != nil || v != seededNumber {
		t.Errorf("money-laundering due diligence within its five years, after erasure: %q, %v", v, err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM erasures WHERE kept LIKE '%identity%'`); n != 1 {
		t.Error("the erasure does not say an identity record was kept")
	}
}

// Rotated, every value is sealed again with the new key and reads with it
// alone; the database lets the application do that and nothing else to a
// record.
func TestIdentityKeysRotateAndTheRecordsFactsStay(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	oldKey, _ := identity.NewKey("old")
	oldKeys, _ := identity.ParseKeys(oldKey)
	party := partyOf(t, pool, fixtureCustomer)
	if err := workshop.StartPartyRole(ctx, pool, everything(advisor()), party, workshop.PartyBuyer, ""); err != nil {
		t.Fatal(err)
	}
	id, err := workshop.RecordIdentity(ctx, pool, sales(), oldKeys, workshop.NewIdentityRecord{PartyID: party,
		Kind: workshop.IdentityNumber, Purpose: "aml_due_diligence", Value: seededNumber})
	if err != nil {
		t.Fatal(err)
	}
	newKey, _ := identity.NewKey("new")
	// The rotating keyring: the new key first, the old one still there.
	both, err := identity.ParseKeys(newKey + "," + oldKey)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := workshop.RotateIdentityKeys(ctx, pool, owner(), both); err != nil || n != 1 {
		t.Fatalf("rotated %d, %v", n, err)
	}
	onlyNew, _ := identity.ParseKeys(newKey)
	if v, err := workshop.ReadIdentityValue(ctx, pool, sales(), onlyNew, id, true); err != nil || v != seededNumber {
		t.Errorf("read with the new key alone: %q, %v", v, err)
	}
	for _, stmt := range []string{
		`DELETE FROM identity_records`,
		`UPDATE identity_records SET purpose = 'licence_check'`,
		`UPDATE identity_records SET party_id = party_id`,
		`UPDATE identity_records SET expires_at = now() + interval '50 years'`,
		`DELETE FROM identity_retention_settings`,
	} {
		err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, stmt)
			return err
		})
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "42501" {
			t.Errorf("%q as the application: %v, want refused for want of the privilege", stmt, err)
		}
	}
}

// Past its expiry a value is not read, whether or not the sweep has run
// yet: the read itself refuses it, and the list shows it as gone and hands
// no reference. Retention ends at the expiry, not at three in the morning.
func TestAnExpiredIdentityValueIsNotReadBeforeTheSweep(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	keys := identityKeys(t, "k1")
	party := partyOf(t, pool, fixtureCustomer)
	if err := workshop.StartPartyRole(ctx, pool, everything(advisor()), party, workshop.PartyRenter, ""); err != nil {
		t.Fatal(err)
	}
	if err := workshop.StartPartyRole(ctx, pool, everything(advisor()), party, workshop.PartyBuyer, ""); err != nil {
		t.Fatal(err)
	}
	number, err := workshop.RecordIdentity(ctx, pool, sales(), keys, workshop.NewIdentityRecord{PartyID: party,
		Kind: workshop.IdentityNumber, Purpose: "ownership_change", Value: seededNumber})
	if err != nil {
		t.Fatal(err)
	}
	desk := otherRoles(access.Of(access.RoleRentalAgent))
	if _, err := workshop.RecordIdentity(ctx, pool, desk, keys, workshop.NewIdentityRecord{PartyID: party,
		Kind: workshop.IdentityLicence, Purpose: "licence_check", Reference: "Körkort kontrollerat"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE identity_records DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `UPDATE identity_records SET recorded_at = now() - interval '1 year', expires_at = now() - interval '1 second'`)
	if _, e := pool.Exec(ctx, `ALTER TABLE identity_records ENABLE TRIGGER USER`); e != nil || err != nil {
		t.Fatal(err, e)
	}
	// No sweep.
	if _, err := workshop.ReadIdentityValue(ctx, pool, sales(), keys, number, true); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("an expired number read before the sweep: %v", err)
	}
	if _, err := workshop.ReadIdentityValue(ctx, pool, sales(), keys, number, false); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("an expired number read masked before the sweep: %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM audit_events WHERE event = 'identity.read'`); n != 0 {
		t.Errorf("%d reads logged of a value past its expiry", n)
	}
	for _, s := range []access.Scope{sales(), desk} {
		listed, err := workshop.IdentityRecordsFor(ctx, pool, s, party)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range listed {
			if r.HasValue || r.Reference != "" || !r.ValueRemoved {
				t.Errorf("%v lists an expired record as %+v", s.Role, r)
			}
		}
	}
}
