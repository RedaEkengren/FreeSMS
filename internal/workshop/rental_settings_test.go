package workshop_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5/pgxpool"
)

// renting is a person with these roles in a shop that runs the workshop
// and Rental.
func renting(roles ...access.Role) access.Scope {
	s := advisor()
	s.Role = access.Of(roles[0], roles[1:]...)
	s.Modules = []access.Module{access.Workshop, access.Rental}
	return s
}

func rentalOwner() access.Scope { return renting(access.RoleOwner) }

func day(s string) time.Time {
	d, err := time.ParseInLocation("2006-01-02", s, time.UTC)
	if err != nil {
		panic(err)
	}
	return d
}

func wallAt(s string) time.Time {
	loc, _ := time.LoadLocation("Europe/Stockholm")
	d, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
	if err != nil {
		panic(err)
	}
	return d
}

func profileFor(t *testing.T, pool *pgxpool.Pool, base string) workshop.RentalProfile {
	t.Helper()
	ps, err := workshop.RentalProfiles(context.Background(), pool, rentalDesk())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.Base == base {
			return p
		}
	}
	t.Fatalf("no %s profile in %+v", base, ps)
	return workshop.RentalProfile{}
}

// A shop that rents only trailers trades without a permit; a car's profile
// reports the handover blocked until one stands for the whole rental.
func TestATrailerNeedsNoPermitAndACarDoes(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	trailer, car := profileFor(t, pool, workshop.ProfileTrailer), profileFor(t, pool, workshop.ProfileCar)
	from, to := wallAt("2026-11-10 09:00"), wallAt("2026-11-12 09:00")

	gaps, err := workshop.RentalHandoverGaps(ctx, pool, rentalDesk(), trailer.VersionID, from, to)
	if err != nil || len(gaps) != 0 {
		t.Errorf("a trailer with no permit: %+v, %v", gaps, err)
	}
	blocked := func(why string, want bool) {
		t.Helper()
		gaps, err := workshop.RentalHandoverGaps(ctx, pool, rentalDesk(), car.VersionID, from, to)
		if err != nil {
			t.Fatal(err)
		}
		got := slices.ContainsFunc(gaps, func(g workshop.HandoverGap) bool { return g.Code == workshop.GapPermit && !g.Overridable })
		if got != want {
			t.Errorf("%s: blocked %v, want %v (%+v)", why, got, want, gaps)
		}
	}
	blocked("a car with no permit", true)

	ends := day("2026-11-11")
	first, err := workshop.RecordRentalPermit(ctx, pool, rentalOwner(), workshop.NewRentalPermit{
		Reference: "TSV 2024-1234", DecidedOn: day("2024-01-15"), ExpiresOn: &ends, Responsible: "Anna Andersson"})
	if err != nil {
		t.Fatal(err)
	}
	blocked("a permit that expires during the rental", true)

	// The renewal is a row of its own, from the day after; between them
	// they stand for every day of the rental.
	if _, err := workshop.RecordRentalPermit(ctx, pool, rentalOwner(), workshop.NewRentalPermit{
		Reference: "TSV 2026-9876", DecidedOn: day("2026-11-12"), Responsible: "Anna Andersson"}); err != nil {
		t.Fatal(err)
	}
	blocked("a permit renewed the day after the old one ends", false)
	permits, err := workshop.RentalPermits(ctx, pool, rentalDesk())
	if err != nil || len(permits) != 2 || permits[1].ID != first || permits[1].Reference != "TSV 2024-1234" {
		t.Errorf("both permits, the old one as it was: %+v, %v", permits, err)
	}

	if err := workshop.RevokeRentalPermit(ctx, pool, rentalOwner(), permits[0].ID, day("2026-11-12"), "Återkallat av Transportstyrelsen"); err != nil {
		t.Fatal(err)
	}
	blocked("a permit revoked on the rental's last day", true)
	if err := workshop.RevokeRentalPermit(ctx, pool, rentalOwner(), permits[0].ID, day("2026-11-13"), "Igen"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("revoked twice: %v", err)
	}
}

// A profile's rules are versions: editing adds one, and the version a
// booking froze reads as it was. A rule the shop changes is the shop's
// decision, no longer marked as an unconfirmed reading of the law; one it
// leaves keeps its mark.
func TestEditingAProfileAddsAVersionAndLeavesTheOldOne(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	trailer := profileFor(t, pool, workshop.ProfileTrailer)
	if !trailer.IsUnconfirmed(workshop.RulePermit) || workshop.UnconfirmedReason(trailer.Base, workshop.RulePermit) == "" {
		t.Fatalf("the trailer's permit default is not marked unconfirmed: %+v", trailer)
	}
	rules := trailer.Rules
	rules.PermitRequired = true
	rules.CleaningBuffer = 45 * time.Minute
	v2, err := workshop.SaveRentalProfile(ctx, pool, rentalOwner(), trailer.ID, rules)
	if err != nil || v2 == trailer.VersionID {
		t.Fatalf("saving: %s, %v", v2, err)
	}
	again, err := workshop.SaveRentalProfile(ctx, pool, rentalOwner(), trailer.ID, rules)
	if err != nil || again != v2 {
		t.Errorf("the same rules again added a version: %s, %v", again, err)
	}
	old, err := workshop.RentalProfileVersion(ctx, pool, rentalDesk(), trailer.VersionID)
	if err != nil || old.Rules.PermitRequired || old.Rules.CleaningBuffer != 30*time.Minute || old.Version != 1 {
		t.Errorf("the first version changed: %+v, %v", old, err)
	}
	now, err := workshop.RentalProfileVersion(ctx, pool, rentalDesk(), v2)
	if err != nil || !now.Rules.PermitRequired || now.Version != 2 || now.IsUnconfirmed(workshop.RulePermit) || !now.IsUnconfirmed(workshop.RuleReporting) {
		t.Errorf("the second version: %+v, %v", now, err)
	}
	history, err := workshop.RentalProfileHistory(ctx, pool, rentalDesk(), trailer.ID)
	if err != nil || len(history) != 2 {
		t.Errorf("history: %+v, %v", history, err)
	}

	rules.LicenceCategories = []string{"Z"}
	if _, err := workshop.SaveRentalProfile(ctx, pool, rentalOwner(), trailer.ID, rules); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("an unknown licence category: %v", err)
	}
	rules.LicenceCategories, rules.LicenceCheckRequired = nil, true
	if _, err := workshop.SaveRentalProfile(ctx, pool, rentalOwner(), trailer.ID, rules); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a licence check with no category: %v", err)
	}

	minibus, err := workshop.AddRentalProfile(ctx, pool, rentalOwner(), "Minibuss", workshop.ProfileCar)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.AddRentalProfile(ctx, pool, rentalOwner(), " minibuss ", workshop.ProfileCar); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a second profile by the same name: %v", err)
	}
	all, _ := workshop.RentalProfiles(ctx, pool, rentalDesk())
	if len(all) != 5 || all[1].ID != minibus || !all[1].Rules.PermitRequired {
		t.Errorf("the shop's own profile after the car's: %+v", all)
	}
}

// Whoever runs the shop changes the settings; the rental desk reads them;
// nobody else does either. Switched off, Rental refuses and keeps the rows.
func TestRentalSettingsByRoleAndModule(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	car := profileFor(t, pool, workshop.ProfileCar)
	permit := workshop.NewRentalPermit{Reference: "TSV 1", DecidedOn: day("2025-01-01"), Responsible: "Anna"}
	if _, err := workshop.RecordRentalPermit(ctx, pool, rentalDesk(), permit); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the rental desk recording a permit: %v", err)
	}
	if _, err := workshop.SaveRentalProfile(ctx, pool, rentalDesk(), car.ID, car.Rules); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the rental desk editing a profile: %v", err)
	}
	if _, err := workshop.AddRentalProfile(ctx, pool, rentalDesk(), "Släp tungt", workshop.ProfileTrailer); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the rental desk adding a profile: %v", err)
	}
	for _, r := range []access.Role{access.RoleTechnician, access.RoleServiceAdvisor, access.RoleSales, access.RoleParts} {
		if _, err := workshop.RentalProfiles(ctx, pool, renting(r)); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s reading the profiles: %v", r, err)
		}
		if _, err := workshop.RentalPermits(ctx, pool, renting(r)); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s reading the permits: %v", r, err)
		}
	}
	if _, err := workshop.RecordRentalPermit(ctx, pool, rentalOwner(), permit); err != nil {
		t.Fatal(err)
	}
	off := rentalOwner()
	off.Modules = workshopOnly
	if _, err := workshop.RentalPermits(ctx, pool, off); !errors.Is(err, access.ErrModuleOff) {
		t.Errorf("reading permits with Rental off: %v", err)
	}
	if _, err := workshop.RecordRentalPermit(ctx, pool, off, permit); !errors.Is(err, access.ErrModuleOff) {
		t.Errorf("recording a permit with Rental off: %v", err)
	}
	if ps, err := workshop.RentalPermits(ctx, pool, rentalDesk()); err != nil || len(ps) != 1 {
		t.Errorf("the permit after Rental was off and on: %+v, %v", ps, err)
	}
}

// The application may add a permit or a profile version, never change or
// remove one: a permit a rental was made under is read as it was.
func TestRentalSettingsAreNeverWrittenOver(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	profileFor(t, pool, workshop.ProfileCar)
	if _, err := workshop.RecordRentalPermit(ctx, pool, rentalOwner(), workshop.NewRentalPermit{
		Reference: "TSV 1", DecidedOn: day("2025-01-01"), Responsible: "Anna"}); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`UPDATE rental_permits SET expires_on = '2030-01-01'`,
		`DELETE FROM rental_permits`,
		`UPDATE rental_profile_versions SET permit_required = false`,
		`DELETE FROM rental_profile_versions`,
		`UPDATE rental_profiles SET name = 'x'`,
	} {
		if err := asApp(ctx, pool, stmt); err == nil {
			t.Errorf("the application could run %s", stmt)
		}
	}
}

// Open-ended from local midnight, a rental must be covered for a whole
// year, the last day included: a permit ending the day before that is
// not enough (review r8, #4).
func TestAnOpenEndedRentalIsCoveredForAWholeYear(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	car := profileFor(t, pool, workshop.ProfileCar)
	from := wallAt("2031-06-01 00:00")
	ends := day("2032-05-31")
	if _, err := workshop.RecordRentalPermit(ctx, pool, rentalOwner(), workshop.NewRentalPermit{
		Reference: "TSV 1", DecidedOn: day("2030-01-01"), ExpiresOn: &ends, Responsible: "Anna"}); err != nil {
		t.Fatal(err)
	}
	gaps, err := workshop.RentalHandoverGaps(ctx, pool, rentalDesk(), car.VersionID, from, time.Time{})
	if err != nil || len(gaps) != 1 || gaps[0].Code != workshop.GapPermit {
		t.Errorf("open-ended from 1 June 2031, a permit to 31 May 2032: %+v, %v; want the permit missing for 1 June 2032", gaps, err)
	}
}
