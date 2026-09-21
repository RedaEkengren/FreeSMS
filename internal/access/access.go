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

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ErrNoScope is returned when a query is attempted without a shop.
//
// It exists so that the mistake surfaces as an error rather than as an empty
// result. Row level security denies everything when the scope is missing,
// which is correct and also indistinguishable from a shop that has no data --
// and an operator reading "no vehicles" will not go looking for a bug.
var ErrNoScope = errors.New("access: no shop in scope")

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
