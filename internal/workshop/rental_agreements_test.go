package workshop_test

import (
	"context"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5/pgxpool"
)

// strictLiability is wording that makes a consumer renter answer for damage
// whatever their fault, which PMÖD 2019:11 held unfair: in either language.
var strictLiability = regexp.MustCompile(`(?i)regardless of (fault|negligence|cause)|irrespective of fault|whether or not .{0,40}(fault|negligen)|strict(ly)? liab|` +
	`oavsett (vållande|fel|skuld|oaktsamhet|orsak)|strikt ansvar|vare sig .{0,40}(vållande|oaktsam)|även om .{0,40}inte .{0,20}(vållat|oaktsam)`)

// reversedBurden is wording that makes a consumer renter prove they were
// not negligent, which presumes negligence for any damage and works close
// to the strict liability PMÖD 2019:11 held unfair (review #9).
var reversedBurden = regexp.MustCompile(`(?i)(the )?renter (shows|proves|can show|must (show|prove))|unless the renter|` +
	`hyrestagaren (visar|kan visa|bevisar|måste (visa|bevisa)|ska (visa|bevisa))|om inte hyrestagaren`)

// The shipped consumer agreement holds the renter liable through
// negligence only, says it needs legal review, and fails this the moment
// a clause of strict liability is added to it.
func TestTheShippedConsumerAgreementHasNoStrictLiability(t *testing.T) {
	for _, locale := range []string{"en", "sv"} {
		for _, name := range []string{workshop.RentalTemplateConsumer, workshop.RentalTemplateBusiness} {
			body := workshop.ShippedRentalTemplate(locale, name)
			if body == "" {
				t.Fatalf("no %s template %q", locale, name)
			}
			if !strings.Contains(strings.ToLower(body), map[string]string{"en": "requires legal review", "sv": "granskas juridiskt"}[locale]) {
				t.Errorf("%s %q does not say it needs legal review", locale, name)
			}
			if name == workshop.RentalTemplateConsumer && strictLiability.MatchString(body) {
				t.Errorf("%s consumer template holds the renter strictly liable: %q", locale, strictLiability.FindString(body))
			}
			if name == workshop.RentalTemplateConsumer && reversedBurden.MatchString(body) {
				t.Errorf("%s consumer template puts the burden of proof on the renter: %q", locale, reversedBurden.FindString(body))
			}
		}
		consumer := workshop.ShippedRentalTemplate(locale, workshop.RentalTemplateConsumer)
		if !strings.Contains(consumer, map[string]string{"en": "negligence", "sv": "oaktsamhet"}[locale]) {
			t.Errorf("the %s consumer template does not tie liability to negligence", locale)
		}
		if !strings.Contains(consumer, map[string]string{"en": "for the lessor to show", "sv": "uthyraren som ska visa"}[locale]) {
			t.Errorf("the %s consumer template does not put the burden of proof on the lessor", locale)
		}
	}
	// The test itself catches the clause it is for.
	if !reversedBurden.MatchString("The renter is not liable for damage the renter shows was not caused by negligence.") ||
		!reversedBurden.MatchString("Hyrestagaren ansvarar inte om inte hyrestagaren visar att skadan inte orsakats av oaktsamhet.") {
		t.Error("the pattern misses a reversed burden of proof")
	}
	if !strictLiability.MatchString("Hyrestagaren ansvarar för skada oavsett vållande.") ||
		!strictLiability.MatchString("The renter is liable for all damage regardless of fault.") {
		t.Error("the pattern misses a strict-liability clause")
	}
}

// A car's rental is handed over only once every driver named has shown a
// licence valid for it, in a category the profile accepts, for this rental:
// a check from another rental does not count, and there is no override.
func TestEveryDriverShowsALicenceForEachRental(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool)
	reported(t, pool, v)
	if _, err := workshop.RecordRentalPermit(ctx, pool, rentalOwner(), workshop.NewRentalPermit{
		Reference: "TSV 1", DecidedOn: day("2020-01-01"), Responsible: "Anna"}); err != nil {
		t.Fatal(err)
	}
	id, err := v.book(pool, in2031(10, 9), in2031(12, 9))
	if err != nil {
		t.Fatal(err)
	}
	codes := func(booking string) []string {
		t.Helper()
		gaps, err := workshop.RentalBookingGaps(ctx, pool, rentalDesk(), booking)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, g := range gaps {
			if g.Overridable {
				t.Errorf("%s can be overridden", g.Code)
			}
			out = append(out, g.Code+":"+g.Who)
		}
		return out
	}
	if got := codes(id); len(got) != 1 || got[0] != workshop.GapNoDriver+":" {
		t.Errorf("nobody named to drive: %v", got)
	}
	main, err := workshop.AddRentalDriver(ctx, pool, rentalDesk(), id, v.renter, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := codes(id); len(got) != 1 || got[0] != workshop.GapLicence+":Rita Hyrare" {
		t.Errorf("a driver not checked: %v", got)
	}
	check := func(driver string, l workshop.LicenceShown) bool {
		t.Helper()
		ok, err := workshop.RecordLicenceCheck(ctx, pool, rentalDesk(), driver, l)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if check(main, workshop.LicenceShown{Shown: true, Categories: []string{"C"}, ValidUntil: day("2035-01-01"), Method: workshop.LicenceCardInspected}) {
		t.Error("a C licence passed for a B car")
	}
	if check(main, workshop.LicenceShown{Shown: true, Categories: []string{"B"}, ValidUntil: in2031(11, 0), Method: workshop.LicenceCardInspected}) {
		t.Error("a licence that runs out during the rental passed")
	}
	if !check(main, workshop.LicenceShown{Shown: true, Categories: []string{"am", "B"}, ValidUntil: day("2035-01-01"), Method: workshop.LicenceCardInspected}) {
		t.Error("a valid B licence did not pass")
	}
	if got := codes(id); len(got) != 0 {
		t.Errorf("everything shown: %v", got)
	}
	// An extra driver at the counter needs a check of their own.
	extra, err := workshop.CreateParty(ctx, pool, rentalDesk(), workshop.NewParty{Kind: "private", Name: "Erik Extra", Role: workshop.PartyRenter})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.AddRentalDriver(ctx, pool, rentalDesk(), id, extra, true); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a second main driver: %v", err)
	}
	if _, err := workshop.AddRentalDriver(ctx, pool, rentalDesk(), id, extra, false); err != nil {
		t.Fatal(err)
	}
	if got := codes(id); len(got) != 1 || got[0] != workshop.GapLicence+":Erik Extra" {
		t.Errorf("an extra driver not checked: %v", got)
	}
	// The next rental checks again.
	next, err := v.book(pool, in2031(20, 9), in2031(21, 9))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.AddRentalDriver(ctx, pool, rentalDesk(), next, v.renter, true); err != nil {
		t.Fatal(err)
	}
	if got := codes(next); len(got) != 1 || got[0] != workshop.GapLicence+":Rita Hyrare" {
		t.Errorf("a check carried from another rental: %v", got)
	}
	// What was shown is an identity record, a reference and no value.
	records, err := workshop.IdentityRecordsFor(ctx, pool, rentalDesk(), v.renter)
	if err != nil || len(records) != 3 || records[0].Kind != workshop.IdentityLicence || records[0].HasValue || records[0].Reference == "" {
		t.Errorf("the identity records: %+v, %v", records, err)
	}
	for _, s := range []access.Scope{renting(access.RoleTechnician), advisor()} {
		if _, err := workshop.RentalDrivers(ctx, pool, s, id); !errors.Is(err, access.ErrForbidden) && !errors.Is(err, access.ErrModuleOff) {
			t.Errorf("%v reading the drivers: %v", s.Role, err)
		}
	}
}

// A trailer whose profile needs no licence check is not held up for one.
func TestATrailerNeedsNoLicenceCheck(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	equipment := profileFor(t, pool, workshop.ProfileEquipment)
	tent := addUnit(t, pool, workshop.NewRentalUnit{Name: "Tält", FleetNumber: "T1", Category: "Tent", ProfileID: equipment.ID})
	if _, err := workshop.StartFleetMembership(ctx, pool, rentalDesk(), tent, time.Now().AddDate(-1, 0, 0), "bought", ""); err != nil {
		t.Fatal(err)
	}
	dayPlan(t, pool, "Tent", 10000, "at_start")
	renter, _ := workshop.CreateParty(ctx, pool, rentalDesk(), workshop.NewParty{Kind: "private", Name: "Tina", Role: workshop.PartyRenter})
	id, err := workshop.BookRental(ctx, pool, rentalDesk(), workshop.NewRentalBooking{UnitID: tent, RenterID: renter, From: in2031(10, 9), To: in2031(11, 9)})
	if err != nil {
		t.Fatal(err)
	}
	if gaps, err := workshop.RentalBookingGaps(ctx, pool, rentalDesk(), id); err != nil || len(gaps) != 0 {
		t.Errorf("a tent: %+v, %v", gaps, err)
	}
	d, _ := workshop.AddRentalDriver(ctx, pool, rentalDesk(), id, renter, true)
	if _, err := workshop.RecordLicenceCheck(ctx, pool, rentalDesk(), d, workshop.LicenceShown{Shown: true, Categories: []string{"B"},
		ValidUntil: day("2035-01-01"), Method: workshop.LicenceCardInspected}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a licence check nobody needs: %v", err)
	}
}

// The agreement is frozen from the shipped template and the booking; a
// change after is a new version that supersedes it, and the first stays
// readable; acceptance is recorded by hand against what was shown.
func TestTheRentalAgreementIsFrozenAndVersioned(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool)
	id, err := v.book(pool, in2031(10, 9), in2031(12, 9))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.AddRentalDriver(ctx, pool, rentalDesk(), id, v.renter, true); err != nil {
		t.Fatal(err)
	}
	templates, err := workshop.RentalAgreementTemplates(ctx, pool, rentalDesk())
	if err != nil || len(templates) != 2 {
		t.Fatalf("the shipped templates: %+v, %v", templates, err)
	}
	var consumer workshop.DocumentTemplate
	for _, tp := range templates {
		if tp.Name == workshop.RentalTemplateConsumer {
			consumer = tp
		}
	}
	first, err := workshop.ProduceRentalAgreement(ctx, pool, rentalDesk(), id, consumer.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := workshop.DocumentRecordByID(ctx, pool, rentalDesk(), first)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Rita Hyrare", "V1, ABC 12D", "1000.00", "negligence"} {
		if !strings.Contains(doc.Body, want) {
			t.Errorf("the agreement lacks %q:\n%s", want, doc.Body)
		}
	}
	if len(doc.Parties) != 2 || doc.Parties[0].Role != "renter" || doc.Parties[1].Role != "driver" {
		t.Errorf("the parties: %+v", doc.Parties)
	}
	if err := workshop.RecordSignature(ctx, pool, rentalDesk(), first, doc.Parties[0].ID, workshop.SignatureEvidence{
		Method: workshop.SignedInPerson, Hash: hex.EncodeToString(mustHex(t, doc.Hash))}); err != nil {
		t.Errorf("accepting in person: %v", err)
	}
	if err := workshop.ExtendRentalBooking(ctx, pool, rentalDesk(), id, in2031(13, 9), "En dag till"); err != nil {
		t.Fatal(err)
	}
	second, err := workshop.ProduceRentalAgreement(ctx, pool, rentalDesk(), id, consumer.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	all, err := workshop.RentalAgreements(ctx, pool, rentalDesk(), id)
	if err != nil || len(all) != 2 || all[0].ID != second || all[0].SupersedesID != first || all[1].SupersededByID != second || all[1].Signing() != "partly signed" {
		t.Errorf("two versions, the first kept: %+v, %v", all, err)
	}
	if !strings.Contains(all[0].Body, "1500.00") {
		t.Errorf("the new version does not carry the extension:\n%s", all[0].Body)
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A plan at 6 % VAT is 6 % everywhere: the price a person agrees including
// VAT is stored net at that rate, and the agreement prints the kilometre
// price at it (review #5).
func TestAPlansVATRateIsUsedEverywhere(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	unit := addUnit(t, pool, workshop.NewRentalUnit{Name: "Cykel", FleetNumber: "C1", Category: "Cykel"})
	if _, err := workshop.StartFleetMembership(ctx, pool, rentalDesk(), unit, time.Now().AddDate(-1, 0, 0), "bought", ""); err != nil {
		t.Fatal(err)
	}
	plan, err := workshop.CreateRatePlan(ctx, pool, rentalOwner(), workshop.NewRatePlan{Name: "Cykel", Category: "Cykel", VATRateBp: 600, SeasonRule: "at_start"})
	if err != nil {
		t.Fatal(err)
	}
	km := 10
	if _, err := workshop.AddRateSeason(ctx, pool, rentalOwner(), plan, workshop.RateSeason{ValidFrom: day("2020-01-01"), Day: kr(10000),
		IncludedKmPerDay: &km, ExtraKmMinor: kr(1000)}); err != nil {
		t.Fatal(err)
	}
	renter, _ := workshop.CreateParty(ctx, pool, rentalDesk(), workshop.NewParty{Kind: "private", Name: "Cyklist", Role: workshop.PartyRenter})
	id, err := workshop.BookRental(ctx, pool, rentalDesk(), workshop.NewRentalBooking{UnitID: unit, RenterID: renter, From: in2031(10, 9), To: in2031(11, 9),
		Overrides: []workshop.PriceOverride{{Position: 1, UnitGrossMinor: kr(10600), Reason: "Avtalat"}}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := workshop.RentalBookingByID(ctx, pool, rentalDesk(), id)
	if b.Lines[0].UnitPriceMinor != 10000 || b.Lines[0].VATRateBp != 600 {
		t.Errorf("106 kr including 6 %% VAT stored as %+v", b.Lines[0])
	}
	// The rate the booking froze is the rate, whatever the plan says later
	// (review 2 #8): no operation changes it; the database owner can.
	if _, err := pool.Exec(ctx, `UPDATE rate_plans SET vat_rate_bp = 2500`); err != nil {
		t.Fatal(err)
	}
	templates, _ := workshop.RentalAgreementTemplates(ctx, pool, rentalDesk())
	doc, err := workshop.ProduceRentalAgreement(ctx, pool, rentalDesk(), id, templates[0].VersionID)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := workshop.DocumentRecordByID(ctx, pool, rentalDesk(), doc)
	if d.Data["extra_km"] != "10.60" {
		t.Errorf("the extra kilometre at 6 %% VAT is printed %q", d.Data["extra_km"])
	}
}

// How a licence was checked is one of a list, with a short note that may
// not hold a licence number or a personal identity number: nothing typed
// there reaches the identity record as an identifier (review #10).
func TestALicenceCheckKeepsNoNumber(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool)
	id, err := v.book(pool, in2031(10, 9), in2031(12, 9))
	if err != nil {
		t.Fatal(err)
	}
	driver, err := workshop.AddRentalDriver(ctx, pool, rentalDesk(), id, v.renter, true)
	if err != nil {
		t.Fatal(err)
	}
	shown := func(method, note string) error {
		_, err := workshop.RecordLicenceCheck(ctx, pool, rentalDesk(), driver, workshop.LicenceShown{Shown: true,
			Categories: []string{"B"}, ValidUntil: day("2035-01-01"), Method: method, Note: note})
		return err
	}
	for _, note := range []string{"Kort 12345678", "pnr 850101-1234", "8501011234", "19850101 1234"} {
		if err := shown(workshop.LicenceCardInspected, note); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("a note %q: %v", note, err)
		}
	}
	if err := shown("looked closely", ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a method not on the list: %v", err)
	}
	if err := shown(workshop.LicenceForeignInspected, "Tyskt körkort, giltigt i Sverige"); err != nil {
		t.Errorf("a note with no number: %v", err)
	}
}

// A passed check is asked again against the rental as it is when the gaps
// are read: extended past the licence's end, the driver has a gap again;
// and the day a rental ends is the shop's day, not UTC's (review #2).
func TestALicenceCheckIsAskedAgainstTheRentalAsItIsNow(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool)
	reported(t, pool, v)
	if _, err := workshop.RecordRentalPermit(ctx, pool, rentalOwner(), workshop.NewRentalPermit{
		Reference: "TSV 1", DecidedOn: day("2020-01-01"), Responsible: "Anna"}); err != nil {
		t.Fatal(err)
	}
	id, err := v.book(pool, wallAt("2031-06-08 09:00"), wallAt("2031-06-09 09:00"))
	if err != nil {
		t.Fatal(err)
	}
	driver, _ := workshop.AddRentalDriver(ctx, pool, rentalDesk(), id, v.renter, true)
	if ok, err := workshop.RecordLicenceCheck(ctx, pool, rentalDesk(), driver, workshop.LicenceShown{Shown: true,
		Categories: []string{"B"}, ValidUntil: day("2031-06-10"), Method: workshop.LicenceCardInspected}); err != nil || !ok {
		t.Fatalf("a licence valid past the end: %v, %v", ok, err)
	}
	if err := workshop.ExtendRentalBooking(ctx, pool, rentalDesk(), id, wallAt("2031-06-20 09:00"), ""); err != nil {
		t.Fatal(err)
	}
	gaps, _ := workshop.RentalBookingGaps(ctx, pool, rentalDesk(), id)
	if len(gaps) != 1 || gaps[0].Code != workshop.GapLicence {
		t.Errorf("extended past the licence's end: %+v", gaps)
	}
	// Ends at half past midnight on 12 July, Stockholm: 22:30 UTC on the
	// 11th. A licence valid until the 11th does not last the rental --
	// whatever zone the server's clock is in, as in a container: UTC.
	was := time.Local
	time.Local = time.UTC
	defer func() { time.Local = was }()
	late, err := v.book(pool, wallAt("2031-07-10 09:00"), wallAt("2031-07-12 00:30"))
	if err != nil {
		t.Fatal(err)
	}
	d2, _ := workshop.AddRentalDriver(ctx, pool, rentalDesk(), late, v.renter, true)
	if ok, err := workshop.RecordLicenceCheck(ctx, pool, rentalDesk(), d2, workshop.LicenceShown{Shown: true,
		Categories: []string{"B"}, ValidUntil: day("2031-07-11"), Method: workshop.LicenceCardInspected}); err != nil || ok {
		t.Errorf("a licence that ends the day before the rental does, by the shop's clock: %v, %v", ok, err)
	}
}

// A rental already out is not extended past a driver's licence: the desk
// is told to check the licence again first (review 2 #3).
func TestARentalOutIsNotExtendedPastADriversLicence(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool)
	id, err := v.book(pool, wallAt("2031-06-08 09:00"), wallAt("2031-06-09 09:00"))
	if err != nil {
		t.Fatal(err)
	}
	driver, _ := workshop.AddRentalDriver(ctx, pool, rentalDesk(), id, v.renter, true)
	if _, err := workshop.RecordLicenceCheck(ctx, pool, rentalDesk(), driver, workshop.LicenceShown{Shown: true,
		Categories: []string{"B"}, ValidUntil: day("2031-06-10"), Method: workshop.LicenceCardInspected}); err != nil {
		t.Fatal(err)
	}
	if err := asApp(ctx, pool, `INSERT INTO rental_booking_events (shop_id, booking_id, kind) VALUES ('`+shopID+`', '`+id+`', 'checked_out')`); err != nil {
		t.Fatal(err)
	}
	if err := workshop.ExtendRentalBooking(ctx, pool, rentalDesk(), id, wallAt("2031-06-20 09:00"), ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("out, and extended past the licence: %v", err)
	}
	if err := workshop.ExtendRentalBooking(ctx, pool, rentalDesk(), id, wallAt("2031-06-10 09:00"), ""); err != nil {
		t.Errorf("out, and extended within the licence: %v", err)
	}
}

// The note refuses an identifier however it is written: digits split by
// any separators or spaces, digits that are not ASCII, a licence number
// of letters and digits, and a date. A free note can never be made fully
// safe; this refuses what a desk would plausibly type (review 2 #7).
// Categories sent twice count once, and nothing a form sends fails on the
// reference's length (review 2 #10).
func TestALicenceNoteRefusesIdentifiersHoweverWritten(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	v := rentableVan(t, pool)
	id, err := v.book(pool, in2031(10, 9), in2031(12, 9))
	if err != nil {
		t.Fatal(err)
	}
	driver, _ := workshop.AddRentalDriver(ctx, pool, rentalDesk(), id, v.renter, true)
	shown := func(note string, cats ...string) error {
		if cats == nil {
			cats = []string{"B"}
		}
		_, err := workshop.RecordLicenceCheck(ctx, pool, rentalDesk(), driver, workshop.LicenceShown{Shown: true,
			Categories: cats, ValidUntil: day("2035-01-01"), Method: workshop.LicenceForeignInspected, Note: note})
		return err
	}
	for _, note := range []string{"DOB 01.01.1985", "no. B072RRE2I55", "85 01 01  1234", "nr ١٢٣٤٥٦٧٨", "född 1/1/85", "12.3.1990",
		"8 5 0 1 0 1", "född 05 mars 1990", "DOB March 05, 1990", "01 jan 1985", "född 5/3", "12.3", "DOB 3/12/1990",
		"1990-03-05", "född 05 mars"} {
		if err := shown(note); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("a note %q: %v", note, err)
		}
	}
	for _, note := range []string{"Tyskt körkort, giltigt i Sverige", "Klass C1E på kortet", "Förnyat 2031",
		"tank 3/4", "tank 1/2", "giltigt till 12/2031", "giltigt till 12/31", "giltigt till mars 2031", "utfärdat 2019-04",
		"kl 14.30", "3-4 dagar"} {
		if err := shown(note); err != nil {
			t.Errorf("a note %q with no identifier: %v", note, err)
		}
	}
	many := make([]string, 30)
	for i := range many {
		many[i] = "C1E"
	}
	if err := shown(strings.Repeat("x", 80), append(many, workshop.LicenceCategories...)...); err != nil && !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("thirty categories and a full note: %v", err)
	}
}

// reported records a rentable van as reported into rental use and
// insured for it (#202), so a test of the drivers sees only theirs.
func reported(t *testing.T, pool *pgxpool.Pool, v rentable) {
	t.Helper()
	u, err := workshop.RentalUnitByID(context.Background(), pool, rentalDesk(), v.unit)
	if err != nil {
		t.Fatal(err)
	}
	inRentalUse(t, pool, u.Memberships[0].ID)
}
