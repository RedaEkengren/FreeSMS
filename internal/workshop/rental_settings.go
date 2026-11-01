package workshop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// Rental's settings (#189): the shop's own rental permit, and a rule
// profile for each kind of thing it rents out. The permit
// (biluthyrningstillstånd, lag 1998:492) is the shop's, recorded here; FreeSMS
// holds none and needs none. Every later rental step asks the profile what a
// unit needs -- RentalHandoverGaps is the one place that answers -- instead
// of asking whether the unit is a car.

// inRental is database.InScope for an operation of the rental module: it
// refuses with access.ErrModuleOff, before a row is read, when the shop does
// not run Rental. Each operation still checks its own role.
func inRental(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, fn func(context.Context, pgx.Tx) error) error {
	if err := scope.Require(access.Rental); err != nil {
		return err
	}
	return database.InScope(ctx, pool, scope, fn)
}

// RentalPermit is one permit decision, as Transportstyrelsen made it.
type RentalPermit struct {
	ID          string
	Reference   string
	DecidedOn   time.Time
	ExpiresOn   *time.Time
	Responsible string
	Note        string
	RevokedOn   *time.Time
	Revocation  string
	RecordedAt  time.Time
	RecordedBy  string
}

// ValidOn reports whether the permit stands on day, a date in the shop's
// zone: decided by then, not expired, not revoked.
func (p RentalPermit) ValidOn(day time.Time) bool {
	d := dateOnly(day)
	return !d.Before(dateOnly(p.DecidedOn)) &&
		(p.ExpiresOn == nil || !d.After(dateOnly(*p.ExpiresOn))) &&
		(p.RevokedOn == nil || d.Before(dateOnly(*p.RevokedOn)))
}

func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// NewRentalPermit is a permit decision to record.
type NewRentalPermit struct {
	Reference   string
	DecidedOn   time.Time
	ExpiresOn   *time.Time
	Responsible string
	Note        string
}

var (
	errPermitReference   = fmt.Errorf("%w: give the permit's reference from Transportstyrelsen, in 100 characters or fewer", ErrInvalid)
	errPermitResponsible = fmt.Errorf("%w: name the person the permit makes responsible, in 200 characters or fewer", ErrInvalid)
	errPermitDates       = fmt.Errorf("%w: a permit expires on or after the day it was decided", ErrInvalid)
	errPermitDecided     = fmt.Errorf("%w: say when the permit was decided", ErrInvalid)
	errPermitRevokeWhy   = fmt.Errorf("%w: say why the permit was revoked, in 300 characters or fewer", ErrInvalid)
	errPermitRevokedOn   = fmt.Errorf("%w: a permit is revoked on or after the day it was decided", ErrInvalid)
	errPermitRevokedTwic = fmt.Errorf("%w: that permit is already recorded as revoked", ErrInvalid)
)

// RecordRentalPermit records a permit decision: a first permit or a renewal,
// each its own row. Whoever runs the shop, whose permit it is.
func RecordRentalPermit(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, n NewRentalPermit) (string, error) {
	if !scope.Role.RunsTheShop() {
		return "", access.ErrForbidden
	}
	n.Reference, n.Responsible = strings.TrimSpace(n.Reference), strings.TrimSpace(n.Responsible)
	switch {
	case n.Reference == "" || utf8.RuneCountInString(n.Reference) > 100:
		return "", errPermitReference
	case n.Responsible == "" || utf8.RuneCountInString(n.Responsible) > 200:
		return "", errPermitResponsible
	case n.DecidedOn.IsZero():
		return "", errPermitDecided
	case n.ExpiresOn != nil && dateOnly(*n.ExpiresOn).Before(dateOnly(n.DecidedOn)):
		return "", errPermitDates
	}
	note, err := checkNote(n.Note)
	if err != nil {
		return "", err
	}
	var id string
	err = inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var expires any
		if n.ExpiresOn != nil {
			expires = n.ExpiresOn.Format("2006-01-02")
		}
		return tx.QueryRow(ctx, `
			INSERT INTO rental_permits (shop_id, reference, decided_on, expires_on, responsible, note, recorded_by)
			VALUES ($1, $2, $3::date, $4::date, $5, nullif($6, ''), NULLIF($7, '')::uuid) RETURNING id`,
			scope.ShopID, n.Reference, n.DecidedOn.Format("2006-01-02"), expires, n.Responsible, note, scope.UserID).Scan(&id)
	})
	return id, err
}

// RevokeRentalPermit records that a permit was revoked from a day, and why.
// The permit stays as it was decided; the revocation is a row of its own.
func RevokeRentalPermit(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, permitID string, on time.Time, reason string) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(permitID) {
		return ErrNotFound
	}
	reason, err := checkReason(reason, errPermitRevokeWhy)
	if err != nil {
		return err
	}
	if on.IsZero() {
		return errPermitRevokedOn
	}
	return inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var decided time.Time
		err := tx.QueryRow(ctx, `SELECT decided_on FROM rental_permits WHERE id = $1`, permitID).Scan(&decided)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read permit: %w", err)
		}
		if dateOnly(on).Before(dateOnly(decided)) {
			return errPermitRevokedOn
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO rental_permit_revocations (shop_id, permit_id, revoked_on, reason, recorded_by)
			VALUES ($1, $2, $3::date, $4, NULLIF($5, '')::uuid)`,
			scope.ShopID, permitID, on.Format("2006-01-02"), reason, scope.UserID)
		if isUniqueViolation(err, "rental_permit_revocations_permit_id_key") {
			return errPermitRevokedTwic
		}
		return err
	})
}

// RentalPermits lists every permit decision, newest first, revoked or not:
// the rental desk reads it, whoever runs the shop records it.
func RentalPermits(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]RentalPermit, error) {
	if !scope.Role.HandsOverRentals() {
		return nil, access.ErrForbidden
	}
	var out []RentalPermit
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = rentalPermitsTx(ctx, tx)
		return err
	})
	return out, err
}

func rentalPermitsTx(ctx context.Context, tx pgx.Tx) ([]RentalPermit, error) {
	rows, err := tx.Query(ctx, `
		SELECT p.id, p.reference, p.decided_on, p.expires_on, p.responsible, coalesce(p.note, ''),
		       r.revoked_on, coalesce(r.reason, ''), p.recorded_at, coalesce(pe.display_name, '')
		FROM rental_permits p
		LEFT JOIN rental_permit_revocations r ON r.permit_id = p.id
		LEFT JOIN users u ON u.id = p.recorded_by
		LEFT JOIN people pe ON pe.id = u.person_id
		ORDER BY p.decided_on DESC, p.recorded_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("read permits: %w", err)
	}
	defer rows.Close()
	var out []RentalPermit
	for rows.Next() {
		var p RentalPermit
		if err := rows.Scan(&p.ID, &p.Reference, &p.DecidedOn, &p.ExpiresOn, &p.Responsible, &p.Note,
			&p.RevokedOn, &p.Revocation, &p.RecordedAt, &p.RecordedBy); err != nil {
			return nil, fmt.Errorf("scan permit: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// permitCovers reports whether the permits between them stand on every day
// from first to last, in the shop's zone: a renewal that starts the day
// after the old one ends covers the change.
func permitCovers(permits []RentalPermit, first, last time.Time) bool {
	for d := dateOnly(first); !d.After(dateOnly(last)); d = d.AddDate(0, 0, 1) {
		if !slices.ContainsFunc(permits, func(p RentalPermit) bool { return p.ValidOn(d) }) {
			return false
		}
	}
	return true
}

// The shipped profiles' bases.
const (
	ProfileCar       = "car"
	ProfileTrailer   = "trailer"
	ProfileMachine   = "machine"
	ProfileEquipment = "equipment"
)

// RentalProfileBases are the shipped profiles, in the order they are offered.
var RentalProfileBases = []string{ProfileCar, ProfileTrailer, ProfileMachine, ProfileEquipment}

// LicenceCategories are the Swedish driving licence categories a profile can
// accept.
var LicenceCategories = []string{"AM", "A1", "A2", "A", "B", "BE", "C1", "C1E", "C", "CE", "D1", "D1E", "D", "DE"}

// The rules a profile has, by name: the names unconfirmed lists.
const (
	RulePermit       = "permit_required"
	RuleRegistration = "registration_required"
	RuleLicenceCheck = "licence_check_required"
	RuleCategories   = "licence_categories"
	RuleReporting    = "reporting_required"
)

// RentalRules are one version of a profile's rules.
type RentalRules struct {
	// Law, as far as it is known.
	PermitRequired       bool
	RegistrationRequired bool
	LicenceCheckRequired bool
	LicenceCategories    []string
	ReportingRequired    bool
	// How the shop works.
	CheckOutTemplateID  string
	CheckInTemplateID   string
	CleaningBuffer      time.Duration
	MileageAllowance    bool
	DefaultDepositMinor int64
	// The contract's: the shop's choice, not the law's.
	MinimumAge   int // 0: none
	SubletBanned bool
}

// shippedRules are the defaults a shop starts with, and which of them rest
// on a legal reading nobody has confirmed (#221). Safe where the law is
// unclear, except where requiring a permit would stop a shop that rents only
// trailers or machines from trading at all; that is marked unconfirmed.
func shippedRules(base string) (RentalRules, []string) {
	switch base {
	case ProfileCar:
		return RentalRules{PermitRequired: true, RegistrationRequired: true, LicenceCheckRequired: true,
			LicenceCategories: []string{"B"}, ReportingRequired: true, CleaningBuffer: time.Hour,
			MileageAllowance: true, MinimumAge: 18, SubletBanned: true}, []string{RuleReporting}
	case ProfileTrailer:
		return RentalRules{RegistrationRequired: true, LicenceCheckRequired: true, LicenceCategories: []string{"B", "BE"},
				CleaningBuffer: 30 * time.Minute, MinimumAge: 18, SubletBanned: true},
			[]string{RulePermit, RuleLicenceCheck, RuleCategories, RuleReporting}
	case ProfileMachine:
		return RentalRules{CleaningBuffer: time.Hour, MinimumAge: 18, SubletBanned: true},
			[]string{RulePermit, RuleRegistration, RuleLicenceCheck, RuleReporting}
	}
	return RentalRules{SubletBanned: true}, nil
}

// UnconfirmedReason is why a shipped rule of a profile on base is not
// presented as the law, as a catalogue key; "" for none.
func UnconfirmedReason(base, rule string) string {
	switch {
	case rule == RuleReporting:
		return "Whether the rental marking in the vehicle register is needed before the first rental, and for which kinds of vehicle, is not confirmed."
	case base == ProfileTrailer && rule == RulePermit:
		return "Lag 1998:492 covers cars and off-road vehicles. Whether a trailer counts is not confirmed, so no permit is required by default."
	case base == ProfileMachine && rule == RulePermit:
		return "Lag 1998:492 covers cars and off-road vehicles. Whether a machine or tractor counts is not confirmed, so no permit is required by default."
	case base == ProfileTrailer && (rule == RuleLicenceCheck || rule == RuleCategories):
		return "The duty to check a licence (16 § lag 1998:492) may not apply to a trailer. It is checked by default, because a heavy trailer needs BE."
	case base == ProfileMachine && (rule == RuleLicenceCheck || rule == RuleRegistration):
		return "Whether a machine needs a registration or a licence depends on what it is. Neither is required by default."
	}
	return ""
}

// RentalProfile is a profile and its current rules.
type RentalProfile struct {
	ID   string
	Base string
	// Name is the shop's own words, except for a shipped profile's, which is
	// a catalogue key (Shipped).
	Name      string
	Shipped   bool
	VersionID string
	Version   int
	Rules     RentalRules
	// The rules, by name, whose legal basis is not confirmed.
	Unconfirmed []string
	ChangedAt   time.Time
	ChangedBy   string
}

// IsUnconfirmed reports whether rule rests on an unconfirmed reading.
func (p RentalProfile) IsUnconfirmed(rule string) bool { return slices.Contains(p.Unconfirmed, rule) }

// ensureRentalProfilesTx gives a shop with no profiles the shipped ones, the
// first time anything asks. Two first readers at once make them once.
func ensureRentalProfilesTx(ctx context.Context, tx pgx.Tx, shopID string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('rental_profiles:' || $1::text, 0))`, shopID); err != nil {
		return fmt.Errorf("lock profiles: %w", err)
	}
	var some bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM rental_profiles)`).Scan(&some); err != nil || some {
		return err
	}
	names := map[string]string{ProfileCar: "Car", ProfileTrailer: "Trailer", ProfileMachine: "Machine", ProfileEquipment: "Equipment"}
	for _, base := range RentalProfileBases {
		var id string
		if err := tx.QueryRow(ctx, `INSERT INTO rental_profiles (shop_id, base, name) VALUES ($1, $2, $3) RETURNING id`,
			shopID, base, names[base]).Scan(&id); err != nil {
			return fmt.Errorf("make the shipped profile: %w", err)
		}
		rules, unconfirmed := shippedRules(base)
		if _, err := insertRulesTx(ctx, tx, shopID, "", id, 1, rules, unconfirmed); err != nil {
			return err
		}
	}
	return nil
}

func insertRulesTx(ctx context.Context, tx pgx.Tx, shopID, userID, profileID string, version int, r RentalRules, unconfirmed []string) (string, error) {
	var age any
	if r.MinimumAge > 0 {
		age = r.MinimumAge
	}
	cats := r.LicenceCategories
	if cats == nil {
		cats = []string{}
	}
	if unconfirmed == nil {
		unconfirmed = []string{}
	}
	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO rental_profile_versions (shop_id, profile_id, version, permit_required, registration_required,
			licence_check_required, licence_categories, reporting_required, check_out_template_id, check_in_template_id,
			cleaning_buffer, mileage_allowance, default_deposit_minor, minimum_age, sublet_banned, unconfirmed, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, '')::uuid, NULLIF($10, '')::uuid, $11, $12, $13, $14, $15, $16, NULLIF($17, '')::uuid)
		RETURNING id`,
		shopID, profileID, version, r.PermitRequired, r.RegistrationRequired, r.LicenceCheckRequired, cats,
		r.ReportingRequired, r.CheckOutTemplateID, r.CheckInTemplateID, r.CleaningBuffer, r.MileageAllowance,
		r.DefaultDepositMinor, age, r.SubletBanned, unconfirmed, userID).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("record the profile's rules: %w", err)
	}
	return id, nil
}

const profileColumns = `
	p.id, p.base, p.name, p.created_by IS NULL, v.id, v.version, v.permit_required, v.registration_required, v.licence_check_required,
	v.licence_categories, v.reporting_required, coalesce(v.check_out_template_id::text, ''),
	coalesce(v.check_in_template_id::text, ''), v.cleaning_buffer, v.mileage_allowance, v.default_deposit_minor,
	coalesce(v.minimum_age, 0), v.sublet_banned, v.unconfirmed, v.created_at, coalesce(pe.display_name, '')`

func scanProfile(row pgx.Row) (RentalProfile, error) {
	var p RentalProfile
	r := &p.Rules
	err := row.Scan(&p.ID, &p.Base, &p.Name, &p.Shipped, &p.VersionID, &p.Version, &r.PermitRequired, &r.RegistrationRequired,
		&r.LicenceCheckRequired, &r.LicenceCategories, &r.ReportingRequired, &r.CheckOutTemplateID, &r.CheckInTemplateID,
		&r.CleaningBuffer, &r.MileageAllowance, &r.DefaultDepositMinor, &r.MinimumAge, &r.SubletBanned,
		&p.Unconfirmed, &p.ChangedAt, &p.ChangedBy)
	return p, err
}

// RentalProfiles lists the shop's profiles with their current rules,
// making the shipped ones the first time. The rental desk reads them;
// whoever runs the shop changes them.
func RentalProfiles(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]RentalProfile, error) {
	if !scope.Role.HandsOverRentals() {
		return nil, access.ErrForbidden
	}
	var out []RentalProfile
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := ensureRentalProfilesTx(ctx, tx, scope.ShopID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT `+profileColumns+`
			FROM rental_profiles p
			JOIN LATERAL (SELECT * FROM rental_profile_versions v WHERE v.profile_id = p.id ORDER BY version DESC LIMIT 1) v ON true
			LEFT JOIN users u ON u.id = v.created_by
			LEFT JOIN people pe ON pe.id = u.person_id
			ORDER BY array_position(ARRAY['car', 'trailer', 'machine', 'equipment'], p.base), p.created_at, p.name`)
		if err != nil {
			return fmt.Errorf("read profiles: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanProfile(rows)
			if err != nil {
				return fmt.Errorf("scan profile: %w", err)
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

// RentalProfileVersion reads one version of a profile's rules: what a
// booking froze, however the profile has changed since.
func RentalProfileVersion(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, versionID string) (RentalProfile, error) {
	if !scope.Role.HandsOverRentals() {
		return RentalProfile{}, access.ErrForbidden
	}
	if !looksLikeUUID(versionID) {
		return RentalProfile{}, ErrNotFound
	}
	var p RentalProfile
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		p, err = profileVersionTx(ctx, tx, versionID)
		return err
	})
	return p, err
}

func profileVersionTx(ctx context.Context, tx pgx.Tx, versionID string) (RentalProfile, error) {
	p, err := scanProfile(tx.QueryRow(ctx, `
		SELECT `+profileColumns+`
		FROM rental_profile_versions v
		JOIN rental_profiles p ON p.id = v.profile_id
		LEFT JOIN users u ON u.id = v.created_by
		LEFT JOIN people pe ON pe.id = u.person_id
		WHERE v.id = $1`, versionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrNotFound
	}
	if err != nil {
		return p, fmt.Errorf("read profile version: %w", err)
	}
	return p, nil
}

var (
	errProfileName     = fmt.Errorf("%w: give the profile a name, in 80 characters or fewer", ErrInvalid)
	errProfileBase     = fmt.Errorf("%w: a profile starts from a car, a trailer, a machine or equipment", ErrInvalid)
	errProfileNameUsed = fmt.Errorf("%w: there is already a profile by that name", ErrInvalid)
	errCategory        = fmt.Errorf("%w: that is not a Swedish licence category", ErrInvalid)
	errNoCategory      = fmt.Errorf("%w: a licence check needs at least one accepted category", ErrInvalid)
	errBuffer          = fmt.Errorf("%w: the cleaning buffer is between nothing and seven days", ErrInvalid)
	errDeposit         = fmt.Errorf("%w: a deposit is nothing or more", ErrInvalid)
	errAge             = fmt.Errorf("%w: a minimum age is between 15 and 99, or none", ErrInvalid)
	errProtocolTmpl    = fmt.Errorf("%w: that checklist is not one for a check-out or a check-in", ErrInvalid)
)

// AddRentalProfile adds a profile of the shop's own -- a minibus, a heavy
// trailer -- starting from the shipped rules of base. Whoever runs the shop.
func AddRentalProfile(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, name, base string) (string, error) {
	if !scope.Role.RunsTheShop() {
		return "", access.ErrForbidden
	}
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 80 {
		return "", errProfileName
	}
	if !slices.Contains(RentalProfileBases, base) {
		return "", errProfileBase
	}
	var id string
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := ensureRentalProfilesTx(ctx, tx, scope.ShopID); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `INSERT INTO rental_profiles (shop_id, base, name, created_by) VALUES ($1, $2, $3, NULLIF($4, '')::uuid) RETURNING id`,
			scope.ShopID, base, name, scope.UserID).Scan(&id)
		if isUniqueViolation(err, "rental_profiles_name_idx") {
			return errProfileNameUsed
		}
		if err != nil {
			return err
		}
		rules, unconfirmed := shippedRules(base)
		_, err = insertRulesTx(ctx, tx, scope.ShopID, scope.UserID, id, 1, rules, unconfirmed)
		return err
	})
	return id, err
}

func checkRules(r *RentalRules) error {
	seen := map[string]bool{}
	var cats []string
	for _, c := range r.LicenceCategories {
		c = strings.ToUpper(strings.TrimSpace(c))
		if c == "" || seen[c] {
			continue
		}
		if !slices.Contains(LicenceCategories, c) {
			return errCategory
		}
		seen[c] = true
		cats = append(cats, c)
	}
	r.LicenceCategories = cats
	switch {
	case r.LicenceCheckRequired && len(cats) == 0:
		return errNoCategory
	case r.CleaningBuffer < 0 || r.CleaningBuffer > 7*24*time.Hour:
		return errBuffer
	case r.DefaultDepositMinor < 0:
		return errDeposit
	case r.MinimumAge != 0 && (r.MinimumAge < 15 || r.MinimumAge > 99):
		return errAge
	case (r.CheckOutTemplateID != "" && !looksLikeUUID(r.CheckOutTemplateID)) || (r.CheckInTemplateID != "" && !looksLikeUUID(r.CheckInTemplateID)):
		return errProtocolTmpl
	}
	return nil
}

// stillUnconfirmed is which of a version's unconfirmed rules the new rules
// leave as they were: a rule the shop changes is the shop's decision from
// then on, and no longer the shipped reading.
func stillUnconfirmed(old RentalProfile, r RentalRules) []string {
	same := map[string]bool{
		RulePermit:       old.Rules.PermitRequired == r.PermitRequired,
		RuleRegistration: old.Rules.RegistrationRequired == r.RegistrationRequired,
		RuleLicenceCheck: old.Rules.LicenceCheckRequired == r.LicenceCheckRequired,
		RuleCategories:   slices.Equal(old.Rules.LicenceCategories, r.LicenceCategories),
		RuleReporting:    old.Rules.ReportingRequired == r.ReportingRequired,
	}
	var out []string
	for _, u := range old.Unconfirmed {
		if same[u] {
			out = append(out, u)
		}
	}
	return out
}

// SaveRentalProfile records new rules for a profile as its next version.
// Bookings keep the version they were made under. Rules that are as they
// were add no version. Whoever runs the shop.
func SaveRentalProfile(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, profileID string, r RentalRules) (string, error) {
	if !scope.Role.RunsTheShop() {
		return "", access.ErrForbidden
	}
	if !looksLikeUUID(profileID) {
		return "", ErrNotFound
	}
	if err := checkRules(&r); err != nil {
		return "", err
	}
	var id string
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		// One edit at a time: the next version number is read under it.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('rental_profile:' || $1::text, 0))`, profileID); err != nil {
			return fmt.Errorf("lock profile: %w", err)
		}
		var current string
		err := tx.QueryRow(ctx, `SELECT id FROM rental_profile_versions WHERE profile_id = $1 ORDER BY version DESC LIMIT 1`, profileID).Scan(&current)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read profile: %w", err)
		}
		old, err := profileVersionTx(ctx, tx, current)
		if err != nil {
			return err
		}
		if err := protocolTemplateTx(ctx, tx, r.CheckOutTemplateID, "check_out"); err != nil {
			return err
		}
		if err := protocolTemplateTx(ctx, tx, r.CheckInTemplateID, "check_in"); err != nil {
			return err
		}
		if sameRules(old.Rules, r) {
			id = current
			return nil
		}
		id, err = insertRulesTx(ctx, tx, scope.ShopID, scope.UserID, profileID, old.Version+1, r, stillUnconfirmed(old, r))
		return err
	})
	return id, err
}

func sameRules(a, b RentalRules) bool {
	return slices.Equal(a.LicenceCategories, b.LicenceCategories) &&
		a.PermitRequired == b.PermitRequired && a.RegistrationRequired == b.RegistrationRequired &&
		a.LicenceCheckRequired == b.LicenceCheckRequired && a.ReportingRequired == b.ReportingRequired &&
		a.CheckOutTemplateID == b.CheckOutTemplateID && a.CheckInTemplateID == b.CheckInTemplateID &&
		a.CleaningBuffer == b.CleaningBuffer && a.MileageAllowance == b.MileageAllowance &&
		a.DefaultDepositMinor == b.DefaultDepositMinor && a.MinimumAge == b.MinimumAge && a.SubletBanned == b.SubletBanned
}

// protocolTemplateTx refuses a checklist that is not active and offered for
// subject.
func protocolTemplateTx(ctx context.Context, tx pgx.Tx, templateID, subject string) error {
	if templateID == "" {
		return nil
	}
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM inspection_templates WHERE id = $1 AND active AND $2 = ANY(subjects))`,
		templateID, subject).Scan(&ok); err != nil {
		return fmt.Errorf("read checklist: %w", err)
	}
	if !ok {
		return errProtocolTmpl
	}
	return nil
}

// RentalProfileHistory lists every version of a profile's rules, newest
// first.
func RentalProfileHistory(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, profileID string) ([]RentalProfile, error) {
	if !scope.Role.HandsOverRentals() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(profileID) {
		return nil, ErrNotFound
	}
	var out []RentalProfile
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+profileColumns+`
			FROM rental_profile_versions v
			JOIN rental_profiles p ON p.id = v.profile_id
			LEFT JOIN users u ON u.id = v.created_by
			LEFT JOIN people pe ON pe.id = u.person_id
			WHERE v.profile_id = $1 ORDER BY v.version DESC`, profileID)
		if err != nil {
			return fmt.Errorf("read profile history: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanProfile(rows)
			if err != nil {
				return fmt.Errorf("scan profile: %w", err)
			}
			out = append(out, p)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(out) == 0 {
			return ErrNotFound
		}
		return nil
	})
	return out, err
}

// HandoverGap is one thing missing before a unit may be handed over, as a
// catalogue key. Some can never be overridden: renting a car out without a
// permit, or to a driver nobody checked, is a crime (lag 1998:492 20-21 §§).
type HandoverGap struct {
	Code        string
	Sentence    string
	Overridable bool
	// Whom it is about, where the sentence names somebody (%s): a driver.
	Who string
}

// The gaps there are.
const (
	GapPermit = "permit"
)

// rentalHandoverGapsTx is the one answer to "what does this profile require
// before handover that is not there", for a rental from..to. Later steps
// add what they own -- a unit, its drivers -- by asking it too; nothing
// else asks whether a unit is a car.
func rentalHandoverGapsTx(ctx context.Context, tx pgx.Tx, profile RentalProfile, from, to time.Time) ([]HandoverGap, error) {
	var gaps []HandoverGap
	if profile.Rules.PermitRequired {
		permits, err := rentalPermitsTx(ctx, tx)
		if err != nil {
			return nil, err
		}
		loc, err := shopLocationTx(ctx, tx)
		if err != nil {
			return nil, err
		}
		last := to
		if last.IsZero() {
			// Open-ended: it has to stand for as long as can be said.
			last = from.AddDate(1, 0, 0)
		} else {
			last = last.Add(-time.Nanosecond)
		}
		if !permitCovers(permits, from.In(loc), last.In(loc)) {
			gaps = append(gaps, HandoverGap{Code: GapPermit,
				Sentence: "There is no rental permit that stands for the whole rental."})
		}
	}
	return gaps, nil
}

// RentalHandoverGaps answers what a profile version requires before
// handover that is missing for a rental from..to (to zero: open-ended).
func RentalHandoverGaps(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, profileVersionID string, from, to time.Time) ([]HandoverGap, error) {
	if !scope.Role.HandsOverRentals() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(profileVersionID) {
		return nil, ErrNotFound
	}
	var gaps []HandoverGap
	err := inRental(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		p, err := profileVersionTx(ctx, tx, profileVersionID)
		if err != nil {
			return err
		}
		gaps, err = rentalHandoverGapsTx(ctx, tx, p, from, to)
		return err
	})
	return gaps, err
}
