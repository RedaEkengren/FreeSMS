// Package access describes who is asking.
//
// Authorisation in this system is enforced where data is read, not where it is
// rendered. Hiding a link in a template is not a permission: the row must
// never reach the handler in the first place. Everything here exists to make
// the scope of a request explicit and hard to omit.
package access

import (
	"errors"
	"fmt"
	"regexp"
)

// Role is what a user may do. Four working roles plus an administrator.
type Role string

const (
	RoleOwner          Role = "owner"
	RoleServiceAdvisor Role = "service_advisor"
	RoleTechnician     Role = "technician"
	RoleParts          Role = "parts"
	RoleAdmin          Role = "admin"
)

// Valid reports whether r is a role the schema accepts. The same list appears
// as a CHECK constraint on users.role; both are deliberate, because one of
// them is the last line of defence when the other is wrong.
func (r Role) Valid() bool {
	switch r {
	case RoleOwner, RoleServiceAdvisor, RoleTechnician, RoleParts, RoleAdmin:
		return true
	}
	return false
}

// roleLabels are the words for a role, as a catalogue key. service_advisor is
// the reason this exists: it is the one role whose identifier has an
// underscore in it, and the header printed it.
var roleLabels = map[Role]string{
	RoleOwner:          "Owner",
	RoleServiceAdvisor: "Front desk",
	RoleTechnician:     "Technician",
	RoleParts:          "Parts",
	RoleAdmin:          "Administrator",
}

// Label is the role in words. An unknown role falls back to its value rather
// than to nothing, so a role added to the constraint and forgotten here reads
// oddly instead of leaving a blank where somebody's job title goes.
func (r Role) Label() string {
	if label, ok := roleLabels[r]; ok {
		return label
	}
	return string(r)
}

// SeesCustomerPersonalData reports whether this role may be shown a customer's
// name, address, telephone number or email.
//
// A technician needs the vehicle and the job. They do not need the customer's
// home address to change a clutch, so they are not given it -- not hidden from
// it in the template, not given it. A field nobody fetches cannot leak.
func (r Role) SeesCustomerPersonalData() bool {
	switch r {
	case RoleOwner, RoleServiceAdvisor, RoleAdmin:
		return true
	}
	return false
}

// SeesParts reports whether this role has the parts desk's screen.
//
// The parts person in a small shop is the front desk, and the owner is
// everybody, so this is not exclusive.
func (r Role) SeesParts() bool {
	switch r {
	case RoleParts, RoleServiceAdvisor, RoleOwner, RoleAdmin:
		return true
	}
	return false
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ErrNoScope is returned when a query is attempted without a shop.
//
// It exists so that the mistake surfaces as an error rather than as an empty
// result. Row level security denies everything when the scope is missing,
// which is correct and also indistinguishable from a shop that has no data --
// and an operator reading "no vehicles" will not go looking for a bug.
var ErrNoScope = errors.New("access: no shop in scope")

// ErrForbidden is returned when the caller's role does not permit what was
// asked for.
//
// Distinct from "not found" on purpose, and only ever shown to someone already
// signed in: telling a technician that a page exists but is not theirs is
// useful, while telling an anonymous visitor the same thing is not.
var ErrForbidden = errors.New("access: not permitted for this role")

// Scope is who is asking, and is the only way to reach data belonging to a
// shop. Functions that read or write tenant rows take one.
type Scope struct {
	ShopID string
	UserID string
	Role   Role
}

// Validate rejects a scope before it can reach the database.
func (s Scope) Validate() error {
	if s.ShopID == "" {
		return ErrNoScope
	}
	// The shop id is interpolated into a session setting rather than bound as
	// a parameter, because SET does not take bind parameters. set_config does,
	// and is used instead -- but the shape is checked here as well, so a value
	// that could not possibly be a shop never travels at all.
	if !uuidPattern.MatchString(s.ShopID) {
		return fmt.Errorf("access: shop id %q is not a uuid", s.ShopID)
	}
	if s.UserID != "" && !uuidPattern.MatchString(s.UserID) {
		return fmt.Errorf("access: user id %q is not a uuid", s.UserID)
	}
	if !s.Role.Valid() {
		return fmt.Errorf("access: role %q is not one of owner, service_advisor, technician, parts, admin", s.Role)
	}
	return nil
}
