package workshop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/money"
)

// The rental agreement, its drivers and their licence checks (#193). The
// agreement is a document record (#150): frozen from a template version and
// the booking, accepted on paper or in person first, a new version for any
// change after. Each driver is a party, and nobody hands over a unit whose
// profile requires a licence check until every driver listed has shown a
// valid licence for this rental -- handing over a car without that check is
// an offence (lag 1998:492 16 och 21 §§), so nothing overrides it. What was
// shown is an identity record (#153), a reference to the check and never a
// copy of the licence; here is only whether it passed.

// The shipped templates, consumer and business, in the shop's language.
// They are the shop's words once made: the shop edits them. Each is marked
// as needing legal review, and the consumer one holds the renter liable for
// damage only through negligence (PMÖD 2019:11), which a test keeps so.
const (
	RentalTemplateConsumer = "Rental agreement, consumer (requires legal review)"
	RentalTemplateBusiness = "Rental agreement, business (requires legal review)"
)

// shippedRentalTemplates are the templates by language and name. Blanks
// (____) are the shop's to fill in before use.
var shippedRentalTemplates = map[string]map[string]string{
	"en": {
		RentalTemplateConsumer: `RENTAL AGREEMENT
This template comes with FreeSMS. It requires legal review before use and is not legal advice.

Lessor: {lessor}, organisation number {lessor_org}. Rental permit: {permit}.
Renter: {renter}.
Drivers: {drivers}.
Unit: {unit}.
Rental period: {period}.

PRICE
{lines}
Total including VAT: {total}.
Kilometres included: {included_km}. Each further kilometre: {extra_km}.
Deposit: {deposit}.

CONDITION AND DAMAGE
The unit is handed over in the condition the check-out protocol records, and is to be returned in the same condition apart from ordinary wear.
The renter is liable for damage to or loss of the unit caused by the negligence of the renter or of a driver. The renter is not liable for damage the renter shows was not caused by such negligence.
Where the renter is liable, the renter pays at most the excess: ____.
Damage found at the return is assessed by the lessor. Before anything is charged or set against the deposit, the renter is told what was found, the assessment and the amount, and how to object.

FUEL AND USE
Fuel or charge: ____. Use outside ____ requires the lessor's written consent. The unit may not be lent or sub-let.

FINES, FEES AND CONGESTION TAX
Congestion tax, parking fees, fines and tolls arising during the rental period are passed on to the renter as charged, after the renter has been told of each and given time to object. An administration fee of ____ is added to each.

CANCELLATION
____

DISPUTES
A consumer may bring a dispute to Allmänna reklamationsnämnden (ARN), www.arn.se.`,
		RentalTemplateBusiness: `RENTAL AGREEMENT (BUSINESS)
This template comes with FreeSMS. It requires legal review before use and is not legal advice.

Lessor: {lessor}, organisation number {lessor_org}. Rental permit: {permit}.
Renter: {renter}.
Drivers: {drivers}.
Unit: {unit}.
Rental period: {period}.

PRICE
{lines}
Total including VAT: {total}.
Kilometres included: {included_km}. Each further kilometre: {extra_km}.
Deposit: {deposit}.

CONDITION AND DAMAGE
The unit is handed over in the condition the check-out protocol records, and is to be returned in the same condition apart from ordinary wear. Liability for damage and loss: ____. Excess: ____.
Damage found at the return is assessed by the lessor, and the renter is told what was found, the assessment and the amount before anything is charged.

FUEL AND USE
Fuel or charge: ____. Use outside ____ requires the lessor's written consent. The unit may not be lent or sub-let.

FINES, FEES AND CONGESTION TAX
Congestion tax, parking fees, fines and tolls arising during the rental period are passed on to the renter as charged. An administration fee of ____ is added to each.

CANCELLATION
____`,
	},
	"sv": {
		RentalTemplateConsumer: `HYRESAVTAL
Mallen följer med FreeSMS. Den behöver granskas juridiskt före användning och är inte juridisk rådgivning.

Uthyrare: {lessor}, organisationsnummer {lessor_org}. Biluthyrningstillstånd: {permit}.
Hyrestagare: {renter}.
Förare: {drivers}.
Enhet: {unit}.
Hyrestid: {period}.

PRIS
{lines}
Totalt inklusive moms: {total}.
Kilometer som ingår: {included_km}. Varje ytterligare kilometer: {extra_km}.
Deposition: {deposit}.

SKICK OCH SKADOR
Enheten lämnas ut i det skick som utlämningsprotokollet anger och ska lämnas tillbaka i samma skick, bortsett från normalt slitage.
Hyrestagaren ansvarar för skada på eller förlust av enheten som orsakats av hyrestagarens eller en förares oaktsamhet. Hyrestagaren ansvarar inte för skada som hyrestagaren visar inte har orsakats av sådan oaktsamhet.
När hyrestagaren ansvarar betalar hyrestagaren högst självrisken: ____.
Skador som upptäcks vid återlämningen bedöms av uthyraren. Innan något debiteras eller avräknas mot depositionen får hyrestagaren veta vad som hittats, bedömningen och beloppet, och hur man invänder.

DRIVMEDEL OCH ANVÄNDNING
Drivmedel eller laddning: ____. Användning utanför ____ kräver uthyrarens skriftliga medgivande. Enheten får inte lånas ut eller hyras ut i andra hand.

BÖTER, AVGIFTER OCH TRÄNGSELSKATT
Trängselskatt, parkeringsavgifter, böter och vägavgifter som uppstår under hyrestiden vidarefaktureras hyrestagaren som de debiterats, sedan hyrestagaren fått veta om var och en och haft tid att invända. En administrationsavgift på ____ läggs till på var och en.

AVBOKNING
____

TVISTER
En konsument kan vända sig till Allmänna reklamationsnämnden (ARN), www.arn.se.`,
		RentalTemplateBusiness: `HYRESAVTAL (FÖRETAG)
Mallen följer med FreeSMS. Den behöver granskas juridiskt före användning och är inte juridisk rådgivning.

Uthyrare: {lessor}, organisationsnummer {lessor_org}. Biluthyrningstillstånd: {permit}.
Hyrestagare: {renter}.
Förare: {drivers}.
Enhet: {unit}.
Hyrestid: {period}.

PRIS
{lines}
Totalt inklusive moms: {total}.
Kilometer som ingår: {included_km}. Varje ytterligare kilometer: {extra_km}.
Deposition: {deposit}.

SKICK OCH SKADOR
Enheten lämnas ut i det skick som utlämningsprotokollet anger och ska lämnas tillbaka i samma skick, bortsett från normalt slitage. Ansvar för skada och förlust: ____. Självrisk: ____.
Skador som upptäcks vid återlämningen bedöms av uthyraren, och hyrestagaren får veta vad som hittats, bedömningen och beloppet innan något debiteras.

DRIVMEDEL OCH ANVÄNDNING
Drivmedel eller laddning: ____. Användning utanför ____ kräver uthyrarens skriftliga medgivande. Enheten får inte lånas ut eller hyras ut i andra hand.

BÖTER, AVGIFTER OCH TRÄNGSELSKATT
Trängselskatt, parkeringsavgifter, böter och vägavgifter som uppstår under hyrestiden vidarefaktureras hyrestagaren som de debiterats. En administrationsavgift på ____ läggs till på var och en.

AVBOKNING
____`,
	},
}

// shippedRentalNames are the shipped templates' names in the shop's
// language, as it will see and edit them.
var shippedRentalNames = map[string]map[string]string{
	"sv": {
		RentalTemplateConsumer: "Hyresavtal, konsument (behöver juridisk granskning)",
		RentalTemplateBusiness: "Hyresavtal, företag (behöver juridisk granskning)",
	},
}

// ShippedRentalTemplate is a shipped template's text in a language ("en",
// "sv"); "" for none.
func ShippedRentalTemplate(locale, name string) string { return shippedRentalTemplates[locale][name] }

// ensureRentalTemplatesTx gives a shop with no rental agreement template
// the shipped ones, in its language, the first time it is asked for.
func ensureRentalTemplatesTx(ctx context.Context, tx pgx.Tx, shopID string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('rental_templates:' || $1::text, 0))`, shopID); err != nil {
		return fmt.Errorf("lock templates: %w", err)
	}
	var some bool
	var locale string
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM document_templates WHERE kind = 'rental_agreement'), (SELECT locale FROM shops)`).Scan(&some, &locale); err != nil || some {
		return err
	}
	texts, ok := shippedRentalTemplates[locale]
	if !ok {
		texts = shippedRentalTemplates["en"]
	}
	for _, name := range []string{RentalTemplateConsumer, RentalTemplateBusiness} {
		var id string
		shown := name
		if n, ok := shippedRentalNames[locale][name]; ok {
			shown = n
		}
		if err := tx.QueryRow(ctx, `INSERT INTO document_templates (shop_id, kind, name) VALUES ($1, 'rental_agreement', $2) RETURNING id`,
			shopID, shown).Scan(&id); err != nil {
			return fmt.Errorf("make the shipped template: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO document_template_versions (shop_id, template_id, version, body) VALUES ($1, $2, 1, $3)`,
			shopID, id, texts[name]); err != nil {
			return fmt.Errorf("make the shipped template: %w", err)
		}
	}
	return nil
}

// RentalAgreementTemplates lists the rental agreement templates, making
// the shipped ones the first time.
func RentalAgreementTemplates(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]DocumentTemplate, error) {
	if !scope.Role.HandsOverRentals() {
		return nil, access.ErrForbidden
	}
	if err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		return ensureRentalTemplatesTx(ctx, tx, scope.ShopID)
	}); err != nil {
		return nil, err
	}
	return DocumentTemplates(ctx, pool, scope, DocRentalAgreement)
}

// RentalDriver is a driver of a rental, with the latest licence check made
// for it. Nothing here is the licence: whether it passed, and why not.
type RentalDriver struct {
	ID      string
	PartyID string
	Name    string
	Main    bool
	AddedAt time.Time
	Removed bool
	Check   *LicenceCheckResult
}

// LicenceCheckResult is a licence check's outcome for one driver.
type LicenceCheckResult struct {
	Passed    bool
	Failure   string
	CheckedAt time.Time
	CheckedBy string
}

var (
	errDriverState   = fmt.Errorf("%w: drivers are changed before the unit is handed over, or added while it is out", ErrInvalid)
	errDriverTwice   = fmt.Errorf("%w: that person is already a driver of this rental", ErrInvalid)
	errDriverMain    = fmt.Errorf("%w: the rental already has a main driver", ErrInvalid)
	errDriverWhy     = fmt.Errorf("%w: say why the driver is taken off, in 300 characters or fewer", ErrInvalid)
	errDriverGone    = fmt.Errorf("%w: that driver has been taken off the rental", ErrInvalid)
	errNoProfileNeed = fmt.Errorf("%w: this rental's profile needs no licence check", ErrInvalid)
	errLicenceDate   = fmt.Errorf("%w: say until when the licence is valid", ErrInvalid)
	errLicenceWhere  = fmt.Errorf("%w: say how the licence was checked, in 200 characters or fewer", ErrInvalid)
)

// AddRentalDriver names a driver for a rental: a party with the renter
// role, the renter themselves or somebody else. Before handover, or while
// the unit is out -- an extra driver added at the counter -- who then needs
// a licence check of their own.
func AddRentalDriver(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, bookingID, partyID string, main bool) (string, error) {
	if !scope.Role.HandsOverRentals() {
		return "", access.ErrForbidden
	}
	if !looksLikeUUID(bookingID) || !looksLikeUUID(partyID) {
		return "", ErrNotFound
	}
	var id string
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		b, err := lockRentalTx(ctx, tx, bookingID)
		if err != nil {
			return err
		}
		if !b.open() {
			return errDriverState
		}
		if err := partySeenTx(ctx, tx, scope, partyID); err != nil {
			return err
		}
		var party string
		err = tx.QueryRow(ctx, `
			SELECT p.id::text FROM parties p WHERE p.id = party_now($1)
			  AND EXISTS (SELECT 1 FROM party_roles r WHERE r.party_id = p.id AND r.role = 'renter' AND r.valid_to IS NULL)`,
			partyID).Scan(&party)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotARenter
		}
		if err != nil {
			return fmt.Errorf("read party: %w", err)
		}
		err = tx.QueryRow(ctx, `INSERT INTO rental_drivers (shop_id, booking_id, party_id, main, added_by) VALUES ($1, $2, $3, $4, NULLIF($5, '')::uuid) RETURNING id`,
			scope.ShopID, bookingID, party, main, scope.UserID).Scan(&id)
		switch {
		case isUniqueViolation(err, "rental_drivers_once"):
			return errDriverTwice
		case isUniqueViolation(err, "rental_drivers_one_main"):
			return errDriverMain
		}
		return err
	})
	return id, err
}

// RemoveRentalDriver takes a driver off a rental before it is handed over,
// saying why. The row stays.
func RemoveRentalDriver(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, driverID, reason string) error {
	if !scope.Role.HandsOverRentals() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(driverID) {
		return ErrNotFound
	}
	reason, err := checkReason(reason, errDriverWhy)
	if err != nil {
		return err
	}
	return inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var booking string
		var removed bool
		err := tx.QueryRow(ctx, `SELECT booking_id, removed_at IS NOT NULL FROM rental_drivers WHERE id = $1 FOR UPDATE`, driverID).Scan(&booking, &removed)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read driver: %w", err)
		}
		if removed {
			return nil
		}
		b, err := lockRentalTx(ctx, tx, booking)
		if err != nil {
			return err
		}
		if b.state != RentalHeld && b.state != RentalConfirmed {
			return errDriverState
		}
		_, err = tx.Exec(ctx, `UPDATE rental_drivers SET removed_at = clock_timestamp(), removed_by = NULLIF($2, '')::uuid, removed_reason = $3 WHERE id = $1`,
			driverID, scope.UserID, reason)
		return err
	})
}

// RentalDrivers lists a rental's drivers, taken off ones last, each with
// its latest licence check.
func RentalDrivers(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, bookingID string) ([]RentalDriver, error) {
	if !scope.Role.HandsOverRentals() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(bookingID) {
		return nil, ErrNotFound
	}
	var out []RentalDriver
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = rentalDriversTx(ctx, tx, bookingID)
		return err
	})
	return out, err
}

func rentalDriversTx(ctx context.Context, tx pgx.Tx, bookingID string) ([]RentalDriver, error) {
	rows, err := tx.Query(ctx, `
		SELECT d.id, d.party_id,
		       coalesce(CASE WHEN pa.kind = 'company' THEN coalesce(c.company_name, pa.company_name) END, cp.display_name, pp.display_name, ''),
		       d.main, d.added_at, d.removed_at IS NOT NULL,
		       k.passed, coalesce(k.failure, ''), k.checked_at, coalesce(kp.display_name, '')
		FROM rental_drivers d
		JOIN parties pa ON pa.id = d.party_id
		LEFT JOIN customers c ON c.id = pa.customer_id
		LEFT JOIN people cp ON cp.id = c.person_id
		LEFT JOIN people pp ON pp.id = pa.person_id
		LEFT JOIN LATERAL (SELECT * FROM rental_licence_checks k WHERE k.driver_id = d.id ORDER BY k.checked_at DESC LIMIT 1) k ON true
		LEFT JOIN users ku ON ku.id = k.checked_by LEFT JOIN people kp ON kp.id = ku.person_id
		WHERE d.booking_id = $1
		ORDER BY d.removed_at IS NOT NULL, NOT d.main, d.added_at`, bookingID)
	if err != nil {
		return nil, fmt.Errorf("read drivers: %w", err)
	}
	defer rows.Close()
	var out []RentalDriver
	for rows.Next() {
		var d RentalDriver
		var passed *bool
		var failure, by string
		var at *time.Time
		if err := rows.Scan(&d.ID, &d.PartyID, &d.Name, &d.Main, &d.AddedAt, &d.Removed, &passed, &failure, &at, &by); err != nil {
			return nil, err
		}
		if passed != nil {
			d.Check = &LicenceCheckResult{Passed: *passed, Failure: failure, CheckedAt: *at, CheckedBy: by}
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// LicenceShown is what a driver's licence showed when it was checked: the
// categories it holds, until when it is valid, and how it was checked -- by
// looking at the card, through a service. No number and no picture.
type LicenceShown struct {
	Shown      bool
	Categories []string
	ValidUntil time.Time
	How        string
}

// Why a licence check did not pass, as catalogue keys.
const (
	licenceNotShown = "No licence was shown."
	licenceCategory = "The licence does not hold a category this unit needs."
	licenceExpires  = "The licence is not valid for the whole rental."
)

// RecordLicenceCheck records a driver's licence check for this rental: the
// details as an identity record for the licence check's purpose (#153),
// kept as a reference to the check and removed when the purpose ends; here,
// whether it passed under the profile the rental was booked under, and why
// not. A licence that runs out before the rental ends does not pass.
func RecordLicenceCheck(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, driverID string, l LicenceShown) (bool, error) {
	if !scope.Role.HandsOverRentals() {
		return false, access.ErrForbidden
	}
	if !looksLikeUUID(driverID) {
		return false, ErrNotFound
	}
	l.How = strings.TrimSpace(l.How)
	switch {
	case l.Shown && l.ValidUntil.IsZero():
		return false, errLicenceDate
	case l.How == "" || len([]rune(l.How)) > 200:
		return false, errLicenceWhere
	}
	cats := make([]string, 0, len(l.Categories))
	for _, c := range l.Categories {
		c = strings.ToUpper(strings.TrimSpace(c))
		if c == "" {
			continue
		}
		if !slices.Contains(LicenceCategories, c) {
			return false, errCategory
		}
		cats = append(cats, c)
	}
	// Who, and under which rules, read first; refused before anything is
	// recorded.
	var party, booking string
	var accepted []string
	var needed, removed bool
	var ends time.Time
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT d.party_id, d.booking_id, d.removed_at IS NOT NULL, v.licence_check_required, v.licence_categories, b.ends_at
			FROM rental_drivers d JOIN rental_bookings b ON b.id = d.booking_id
			JOIN rental_profile_versions v ON v.id = b.profile_version_id
			WHERE d.id = $1`, driverID).Scan(&party, &booking, &removed, &needed, &accepted, &ends)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read driver: %w", err)
		}
		b, err := lockRentalTx(ctx, tx, booking)
		if err != nil {
			return err
		}
		switch {
		case removed:
			return errDriverGone
		case !b.open():
			return errDriverState
		case !needed:
			return errNoProfileNeed
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	failure := ""
	switch {
	case !l.Shown:
		failure = licenceNotShown
	case !slices.ContainsFunc(cats, func(c string) bool { return slices.Contains(accepted, c) }):
		failure = licenceCategory
	case dateOnly(l.ValidUntil).Before(dateOnly(ends)):
		failure = licenceExpires
	}
	ref := "Licence not shown; checked by: " + l.How
	if l.Shown {
		ref = fmt.Sprintf("Licence shown; categories %s; valid until %s; checked by: %s",
			strings.Join(cats, ", "), l.ValidUntil.Format("2006-01-02"), l.How)
	}
	if len([]rune(ref)) > 200 {
		ref = string([]rune(ref)[:200])
	}
	record, err := RecordIdentity(ctx, pool, scope, nil, NewIdentityRecord{PartyID: party, Kind: IdentityLicence,
		Purpose: "licence_check", Reference: ref})
	if err != nil {
		return false, err
	}
	err = inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO rental_licence_checks (shop_id, booking_id, driver_id, identity_record_id, passed, failure, checked_by)
			VALUES ($1, $2, $3, $4, $5, nullif($6, ''), $7)`,
			scope.ShopID, booking, driverID, record, failure == "", failure, scope.UserID)
		return err
	})
	return failure == "" && err == nil, err
}

// The gaps a booking can have before handover, beyond its profile's.
const (
	GapNoDriver = "no_driver"
	GapLicence  = "licence"
)

// rentalBookingGapsTx is what a booking still lacks before its unit may be
// handed over: its profile's requirements (rentalHandoverGapsTx) for its
// own period, and a passing licence check this rental for every driver
// listed, when the profile needs one. Check-out (#194) refuses while any is
// missing; none of these can be overridden.
func rentalBookingGapsTx(ctx context.Context, tx pgx.Tx, bookingID string) ([]HandoverGap, error) {
	var version string
	var starts, ends time.Time
	if err := tx.QueryRow(ctx, `SELECT profile_version_id, starts_at, ends_at FROM rental_bookings WHERE id = $1`, bookingID).Scan(&version, &starts, &ends); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read booking: %w", err)
	}
	p, err := profileVersionTx(ctx, tx, version)
	if err != nil {
		return nil, err
	}
	gaps, err := rentalHandoverGapsTx(ctx, tx, p, starts, ends)
	if err != nil {
		return nil, err
	}
	if !p.Rules.LicenceCheckRequired {
		return gaps, nil
	}
	drivers, err := rentalDriversTx(ctx, tx, bookingID)
	if err != nil {
		return nil, err
	}
	listed := 0
	for _, d := range drivers {
		if d.Removed {
			continue
		}
		listed++
		if d.Check == nil || !d.Check.Passed {
			gaps = append(gaps, HandoverGap{Code: GapLicence, Who: d.Name,
				Sentence: "%s has not shown a valid licence for this rental."})
		}
	}
	if listed == 0 {
		gaps = append(gaps, HandoverGap{Code: GapNoDriver, Sentence: "No driver is named for the rental."})
	}
	return gaps, nil
}

// RentalBookingGaps is what a booking still lacks before handover, all at
// once.
func RentalBookingGaps(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, bookingID string) ([]HandoverGap, error) {
	if !scope.Role.HandsOverRentals() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(bookingID) {
		return nil, ErrNotFound
	}
	var gaps []HandoverGap
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		gaps, err = rentalBookingGapsTx(ctx, tx, bookingID)
		return err
	})
	return gaps, err
}

var (
	errAgreementTemplate = fmt.Errorf("%w: that is not a rental agreement template", ErrInvalid)
	errAgreementState    = fmt.Errorf("%w: an agreement is made for a booking that is held, confirmed or out", ErrInvalid)
)

// ProduceRentalAgreement freezes the booking's agreement from a template
// version: the lessor and its permit, the renter and drivers, the unit, the
// period and the frozen price. A booking that already has one gets a new
// version that supersedes it -- an extra driver, an extension -- and the
// one before stays readable, signed or not.
func ProduceRentalAgreement(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, bookingID, templateVersionID string) (string, error) {
	if !scope.Role.HandsOverRentals() {
		return "", access.ErrForbidden
	}
	if !looksLikeUUID(bookingID) || !looksLikeUUID(templateVersionID) {
		return "", ErrNotFound
	}
	n := NewDocumentRecord{Kind: DocRentalAgreement, SubjectKind: "rental_booking", SubjectID: bookingID,
		TemplateVersionID: templateVersionID, Data: map[string]string{}}
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var kind string
		if err := tx.QueryRow(ctx, `SELECT t.kind FROM document_template_versions v JOIN document_templates t ON t.id = v.template_id WHERE v.id = $1`,
			templateVersionID).Scan(&kind); err != nil || kind != DocRentalAgreement {
			return errAgreementTemplate
		}
		b, err := lockRentalTx(ctx, tx, bookingID)
		if err != nil {
			return err
		}
		if !b.open() {
			return errAgreementState
		}
		var number int
		var renter, unit, lessor, org string
		var km *int
		var extraKm *int64
		var deposit int64
		var vat int
		if err := tx.QueryRow(ctx, `
			SELECT b.number, b.renter_party_id, u.fleet_number || coalesce(', ' || reg.registration, ', ' || r.name, ''),
			       s.name, coalesce(s.org_number, ''), b.included_km, b.extra_km_minor, v.default_deposit_minor,
			       coalesce((SELECT vat_rate_bp FROM rate_plans WHERE id = b.plan_id), 0)
			FROM rental_bookings b JOIN rental_units u ON u.id = b.unit_id JOIN resources r ON r.id = u.resource_id
			LEFT JOIN vehicle_registrations reg ON reg.vehicle_id = r.vehicle_id AND reg.valid_to IS NULL
			JOIN rental_profile_versions v ON v.id = b.profile_version_id
			CROSS JOIN shops s
			WHERE b.id = $1`, bookingID).Scan(&number, &renter, &unit, &lessor, &org, &km, &extraKm, &deposit, &vat); err != nil {
			return fmt.Errorf("read booking: %w", err)
		}
		loc, err := shopLocationTx(ctx, tx)
		if err != nil {
			return err
		}
		permits, err := rentalPermitsTx(ctx, tx)
		if err != nil {
			return err
		}
		permit := "-"
		for _, p := range permits {
			if p.ValidOn(b.starts.In(loc)) {
				permit = p.Reference
				break
			}
		}
		lines, err := rentalLinesTx(ctx, tx, bookingID)
		if err != nil {
			return err
		}
		var text []string
		for _, l := range lines {
			gross := l.Net() + money.VAT(l.Net(), l.VATRateBp)
			text = append(text, fmt.Sprintf("%s, %s x %s: %s", l.Description, formatMilli(l.QuantityMilli), l.Unit, money.Format(gross)))
		}
		drivers, err := rentalDriversTx(ctx, tx, bookingID)
		if err != nil {
			return err
		}
		n.Parties = []DocumentPartyRole{{PartyID: renter, Role: "renter"}}
		var names []string
		for _, d := range drivers {
			if d.Removed {
				continue
			}
			names = append(names, d.Name)
			n.Parties = append(n.Parties, DocumentPartyRole{PartyID: d.PartyID, Role: "driver"})
		}
		includedKm, extra := "-", "-"
		if km != nil {
			includedKm = fmt.Sprint(*km)
		}
		if extraKm != nil {
			extra = money.Format(*extraKm + money.VAT(*extraKm, vat))
		}
		var renterName string
		_ = tx.QueryRow(ctx, `SELECT coalesce(CASE WHEN pa.kind = 'company' THEN coalesce(c.company_name, pa.company_name) END, cp.display_name, pp.display_name, '')`+
			` FROM parties pa LEFT JOIN customers c ON c.id = pa.customer_id LEFT JOIN people cp ON cp.id = c.person_id LEFT JOIN people pp ON pp.id = pa.person_id WHERE pa.id = $1`,
			renter).Scan(&renterName)
		n.Title = fmt.Sprintf("R-%d %s", number, unit)
		n.Data = map[string]string{
			"lessor": lessor, "lessor_org": org, "permit": permit, "renter": renterName, "drivers": strings.Join(names, ", "),
			"unit": unit, "period": b.starts.In(loc).Format("2006-01-02 15:04") + " - " + b.ends.In(loc).Format("2006-01-02 15:04"),
			"lines": strings.Join(text, "\n"), "total": money.Format(rentalTotals(lines).GrossMinor),
			"included_km": includedKm, "extra_km": extra, "deposit": money.Format(deposit),
		}
		return tx.QueryRow(ctx, `
			SELECT coalesce((SELECT id::text FROM document_records WHERE kind = 'rental_agreement' AND subject_id = $1
			                  AND NOT EXISTS (SELECT 1 FROM document_records n WHERE n.supersedes_id = document_records.id)
			                ORDER BY created_at DESC LIMIT 1), '')`, bookingID).Scan(&n.Supersedes)
	})
	if err != nil {
		return "", err
	}
	return ProduceDocument(ctx, pool, scope, n)
}

// RentalAgreements lists a booking's agreements, newest first, each as
// Core reads it.
func RentalAgreements(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, bookingID string) ([]DocumentRecord, error) {
	if !scope.Role.HandsOverRentals() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(bookingID) {
		return nil, ErrNotFound
	}
	var ids []string
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id::text FROM document_records WHERE kind = 'rental_agreement' AND subject_id = $1 ORDER BY created_at DESC`, bookingID)
		if err != nil {
			return fmt.Errorf("read agreements: %w", err)
		}
		ids, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		return nil, err
	}
	out := make([]DocumentRecord, 0, len(ids))
	for _, id := range ids {
		d, err := DocumentRecordByID(ctx, pool, scope, id)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}
