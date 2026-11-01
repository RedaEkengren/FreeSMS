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
	"slices"
)

// Role is what a user may do. Four workshop roles, three for car sales and
// rental (#145), and an administrator. A person holds one or more: see
// Roles.
type Role string

const (
	RoleOwner          Role = "owner"
	RoleServiceAdvisor Role = "service_advisor"
	RoleTechnician     Role = "technician"
	RoleParts          Role = "parts"
	RoleAdmin          Role = "admin"
	// Sells the dealer's cars: the customer, the deal, the delivery. Not
	// what the car was bought for.
	RoleSales Role = "sales"
	// Buys cars into the dealer's stock, and so knows what they cost.
	RoleBuyer Role = "buyer"
	// The rental desk: bookings, hand-overs and returns.
	RoleRentalAgent Role = "rental_agent"
)

// AllRoles is every role there is.
var AllRoles = []Role{
	RoleOwner, RoleServiceAdvisor, RoleTechnician, RoleParts, RoleAdmin,
	RoleSales, RoleBuyer, RoleRentalAgent,
}

// Valid reports whether r is a role the schema accepts. The same list appears
// as a CHECK constraint on users.role and user_roles.role; both are
// deliberate, because one of them is the last line of defence when the other
// is wrong.
func (r Role) Valid() bool { return slices.Contains(AllRoles, r) }

// Module is the module a role works in, or "" for one that belongs to Core
// and so to every shop: the owner and the administrator. A role whose module
// the shop does not run cannot be given to anybody -- a rental desk in a
// shop that does not rent has nothing to do and is a door left open for
// when it does.
func (r Role) Module() Module {
	switch r {
	case RoleServiceAdvisor, RoleTechnician, RoleParts:
		return Workshop
	case RoleSales, RoleBuyer:
		return Dealer
	case RoleRentalAgent:
		return Rental
	}
	return ""
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
	RoleSales:          "Salesperson",
	RoleBuyer:          "Buyer",
	RoleRentalAgent:    "Rental desk",
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

// Every capability below says which roles have it. The owner and the
// administrator have every one: they run the business, whichever part of it.
// A person with several roles has a capability when any of them has it
// (Roles), so a salesperson who is also the buyer sees purchase prices
// because the buyer does, not by an exception for the pair.

func (r Role) runsIt() bool { return r == RoleOwner || r == RoleAdmin }

// SeesCustomerPersonalData reports whether this role may be shown a customer's
// name, address, telephone number or email.
//
// A technician needs the vehicle and the job. They do not need the customer's
// home address to change a clutch, so they are not given it -- not hidden from
// it in the template, not given it. A field nobody fetches cannot leak.
//
// Not yet the dealer's or the rental desk's: what they are shown of a party
// is decided with the parties (#148), and this capability also lets the
// front desk invoice and move any job, which theirs must not.
func (r Role) SeesCustomerPersonalData() bool {
	return r.runsIt() || r == RoleServiceAdvisor
}

// RunsTheShop reports whether this role may see and change the business's own
// particulars: its organisation number, its VAT registration, its bank
// details.
//
// Narrower than SeesCustomerPersonalData on purpose. A service advisor needs a
// customer's address to invoice them; they do not need to be able to change
// the account the money is paid into.
func (r Role) RunsTheShop() bool { return r.runsIt() }

// SeesParts reports whether this role has the parts desk's screen.
//
// The parts person in a small shop is the front desk, and the owner is
// everybody, so this is not exclusive.
func (r Role) SeesParts() bool {
	return r == RoleParts || r.SeesCustomerPersonalData()
}

// HandlesParts reports whether this role takes parts off the shelf to a job
// and puts them back: the technician fitting them, and whoever runs stock.
// Not pricing them, which stays with the counter.
func (r Role) HandlesParts() bool { return r == RoleTechnician || r.SeesParts() }

// FollowsJobs reports whether this role moves a job along and reads how it
// is going: the technician holding the spanner, and the counter. The parts
// desk sees a job through its parts, not its steps.
func (r Role) FollowsJobs() bool { return r == RoleTechnician || r.SeesCustomerPersonalData() }

// WorksInTheWorkshop reports whether this role has any part in the
// workshop's work. The roles for car sales and rental have none: a
// salesperson whose stock car is in for service follows it through the car,
// not by opening the workshop's jobs, inspections and clocks.
func (r Role) WorksInTheWorkshop() bool { return r.runsIt() || r.Module() == Workshop }

// SellsVehicles reports whether this role sells the dealer's cars: makes a
// deal, a contract and a delivery.
func (r Role) SellsVehicles() bool { return r.runsIt() || r == RoleSales }

// BuysVehicles reports whether this role buys cars into stock: appraises
// them, agrees a price, takes them in.
func (r Role) BuysVehicles() bool { return r.runsIt() || r == RoleBuyer }

// SeesPurchasePrice reports whether this role may be shown what the business
// paid for a car in stock, its purchase price for tax and the basis for VAT
// on it, and the costs put on it since. The buyer agreed the price, so knows
// it; a salesperson is not shown it, because the first number a customer
// haggles towards is the one the seller knows the car cost. The workshop's
// parts costs are the parts desk's (SeesParts) and are not this.
func (r Role) SeesPurchasePrice() bool { return r.BuysVehicles() }

// SeesMargin reports whether this role may be shown what a car made or will
// make. The selling price is on the deal, so the margin and the selling
// price together are the purchase price: whoever may not see that may not
// see this.
func (r Role) SeesMargin() bool { return r.SeesPurchasePrice() }

// HandsOverRentals reports whether this role books rentals, hands the
// vehicle or the equipment over and takes it back.
func (r Role) HandsOverRentals() bool { return r.runsIt() || r == RoleRentalAgent }

// SeesIdentityRecords reports whether this role may read a person's identity
// number or driving licence, held apart from the party with a purpose and an
// expiry (#153). Whoever signs a person up to a contract checks who they are:
// the seller and the buyer of a car, the rental desk handing one over. The
// workshop never needs it to mend a car.
func (r Role) SeesIdentityRecords() bool {
	return r.runsIt() || r == RoleSales || r == RoleBuyer || r == RoleRentalAgent
}

// Roles is the roles one person holds: their main role first, then any
// others. It has every capability Role has, answering yes when any role
// held has it, so an operation asks scope.Role.SeesX() whatever the person
// holds.
type Roles []Role

// Of is the roles held, main role first, duplicates dropped.
func Of(main Role, others ...Role) Roles {
	out := Roles{main}
	for _, r := range others {
		if !out.Has(r) {
			out = append(out, r)
		}
	}
	return out
}

// Main is the role the person is known by: the header names it and their
// home page starts from it. "" for none.
func (rs Roles) Main() Role {
	if len(rs) == 0 {
		return ""
	}
	return rs[0]
}

// Has reports whether r is one of the roles held, exactly. For a question a
// capability answers, ask the capability.
func (rs Roles) Has(r Role) bool { return slices.Contains(rs, r) }

// Valid reports whether there is at least one role and every one is known.
func (rs Roles) Valid() bool {
	if len(rs) == 0 {
		return false
	}
	for _, r := range rs {
		if !r.Valid() {
			return false
		}
	}
	return true
}

// Label is the main role in words.
func (rs Roles) Label() string { return rs.Main().Label() }

func (rs Roles) any(has func(Role) bool) bool {
	for _, r := range rs {
		if has(r) {
			return true
		}
	}
	return false
}

func (rs Roles) SeesCustomerPersonalData() bool { return rs.any(Role.SeesCustomerPersonalData) }
func (rs Roles) RunsTheShop() bool              { return rs.any(Role.RunsTheShop) }
func (rs Roles) SeesParts() bool                { return rs.any(Role.SeesParts) }
func (rs Roles) HandlesParts() bool             { return rs.any(Role.HandlesParts) }
func (rs Roles) FollowsJobs() bool              { return rs.any(Role.FollowsJobs) }
func (rs Roles) WorksInTheWorkshop() bool       { return rs.any(Role.WorksInTheWorkshop) }
func (rs Roles) SellsVehicles() bool            { return rs.any(Role.SellsVehicles) }
func (rs Roles) BuysVehicles() bool             { return rs.any(Role.BuysVehicles) }
func (rs Roles) SeesPurchasePrice() bool        { return rs.any(Role.SeesPurchasePrice) }
func (rs Roles) SeesMargin() bool               { return rs.any(Role.SeesMargin) }
func (rs Roles) HandsOverRentals() bool         { return rs.any(Role.HandsOverRentals) }
func (rs Roles) SeesIdentityRecords() bool      { return rs.any(Role.SeesIdentityRecords) }

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// IsUUID reports whether s is shaped like an identifier. A path segment that
// is not one cannot name a row, and asking Postgres to cast it is an error
// rather than "not found".
func IsUUID(s string) bool { return uuidPattern.MatchString(s) }

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

// Module is a part of FreeSMS a shop chooses to run (#143). Core -- customers,
// vehicles, documents, payments, people -- is not a module: every shop has it.
type Module string

const (
	Workshop Module = "workshop"
	Dealer   Module = "dealer"
	Rental   Module = "rental"
)

// Modules is every module there is, in the order they are offered. The same
// list is a CHECK constraint on shop_module_changes.module.
var Modules = []Module{Workshop, Dealer, Rental}

// Valid reports whether m is a module the schema accepts.
func (m Module) Valid() bool { return slices.Contains(Modules, m) }

var moduleLabels = map[Module]string{
	Workshop: "Workshop",
	Dealer:   "Car sales",
	Rental:   "Rental",
}

// Label is the module in words, as a catalogue key.
func (m Module) Label() string {
	if label, ok := moduleLabels[m]; ok {
		return label
	}
	return string(m)
}

// ErrModuleOff is returned by an operation of a module the shop does not run.
//
// Not ErrForbidden: nobody's role is wrong. The shop has switched the module
// off, and what it wrote while it was on is still read through Core. Over
// HTTP a switched-off module's routes are not found; this is what an
// operation reached any other way answers.
var ErrModuleOff = errors.New("access: the module is switched off for this shop")

// Scope is who is asking, and is the only way to reach data belonging to a
// shop. Functions that read or write tenant rows take one.
type Scope struct {
	ShopID string
	UserID string
	// Every role the person holds, read with the session on every request,
	// so a change takes effect on the next click. Named Role, not Roles,
	// because every call site asks it a capability -- scope.Role.SeesParts()
	// -- and the answer is the same question whether one role is held or
	// three.
	Role Roles
	// Given the planning of people by whoever runs the shop. Read with
	// PlansStaff, which also answers yes for those who run it.
	Planner bool
	// The modules the shop runs, loaded with the session so that an
	// operation can check without another query. Empty runs none: a scope
	// somebody forgot to fill in refuses every module's operations rather
	// than allowing them.
	Modules []Module
}

// Uses reports whether the shop runs every one of these modules. A feature
// across modules -- a rental's damage as a workshop job -- asks for both.
func (s Scope) Uses(modules ...Module) bool {
	for _, want := range modules {
		if !slices.Contains(s.Modules, want) {
			return false
		}
	}
	return true
}

// Offers reports whether the shop runs m, for deciding what to offer --
// which roles to give, which subjects a checklist may have, which tasks
// Getting started lists, what an import may bring in. It refuses nothing,
// so the permission contract does not take it for a gate (Uses and Require
// it does): an operation that is Core's may ask it and stay Core's.
func (s Scope) Offers(m Module) bool { return slices.Contains(s.Modules, m) }

// Require is Uses as the error an operation returns: nil, or ErrModuleOff
// naming the module that is off.
func (s Scope) Require(modules ...Module) error {
	for _, m := range modules {
		if !s.Uses(m) {
			return fmt.Errorf("%w: %s", ErrModuleOff, m)
		}
	}
	return nil
}

// PlansStaff reports whether this person may set rotas and absences, and
// read why somebody is away. Whoever runs the shop may; anybody else only
// when they were given it -- in a larger workshop, the workshop manager.
//
// Why somebody is away can be their health, which GDPR article 9 puts in a
// category of its own. The front desk needs to know Erik is not in, not why.
func (s Scope) PlansStaff() bool { return s.Role.RunsTheShop() || s.Planner }

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
		return fmt.Errorf("access: roles %q are not all known roles", s.Role)
	}
	return nil
}
