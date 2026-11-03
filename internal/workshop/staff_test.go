package workshop_test

import (
	"context"
	"errors"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/auth"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

const firstPassword = "a first password to hand over"

// An owner adds a technician, and the technician signs in as a technician.
// Nothing could make anybody but the owner before.
func TestAnOwnerAddsATechnicianWhoSignsIn(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	id, err := workshop.AddStaff(ctx, pool, owner(), "Erik Mekaniker", "erik@verkstaden.test", access.RoleTechnician, firstPassword)
	if err != nil {
		t.Fatalf("AddStaff: %v", err)
	}
	_, s, err := auth.Login(ctx, pool, shopID, "erik@verkstaden.test", firstPassword, "test")
	if err != nil {
		t.Fatalf("the new technician cannot sign in: %v", err)
	}
	if s.Scope.UserID != id || s.Scope.Role.Main() != access.RoleTechnician {
		t.Errorf("signed in as %+v, want the technician just added", s.Scope)
	}

	staff, _ := workshop.Staff(ctx, pool, owner())
	var found bool
	for _, m := range staff {
		found = found || (m.UserID == id && m.Active && m.Name == "Erik Mekaniker")
	}
	if !found {
		t.Errorf("the staff list %+v does not show the technician", staff)
	}

	if _, err := workshop.AddStaff(ctx, pool, owner(), "Another Erik", "erik@verkstaden.test", access.RoleParts, firstPassword); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a second person with the same email: %v, want refused", err)
	}
	if _, err := workshop.AddStaff(ctx, pool, owner(), "Kort", "kort@verkstaden.test", access.RoleParts, "short"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a short first password: %v, want refused", err)
	}
}

// Switched off: signed out at once and unable to sign in, until switched
// back on. A new password ends their sessions too.
func TestSwitchingStaffOffAndOn(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	id, _ := workshop.AddStaff(ctx, pool, owner(), "Erik", "erik@verkstaden.test", access.RoleTechnician, firstPassword)
	token, _, _ := auth.Login(ctx, pool, shopID, "erik@verkstaden.test", firstPassword, "test")

	if err := workshop.SetStaffActive(ctx, pool, owner(), id, false); err != nil {
		t.Fatalf("switch off: %v", err)
	}
	if _, err := auth.Authenticate(ctx, pool, shopID, token); !errors.Is(err, auth.ErrNoSession) {
		t.Errorf("still signed in after being switched off: %v", err)
	}
	if _, _, err := auth.Login(ctx, pool, shopID, "erik@verkstaden.test", firstPassword, "x"); err == nil {
		t.Error("signed in while switched off")
	}
	if err := workshop.SetStaffActive(ctx, pool, owner(), id, true); err != nil {
		t.Fatalf("switch on: %v", err)
	}
	token, _, err := auth.Login(ctx, pool, shopID, "erik@verkstaden.test", firstPassword, "x")
	if err != nil {
		t.Fatalf("cannot sign in after being switched back on: %v", err)
	}

	if err := workshop.SetStaffPassword(ctx, pool, owner(), id, "a new password entirely"); err != nil {
		t.Fatalf("SetStaffPassword: %v", err)
	}
	if _, err := auth.Authenticate(ctx, pool, shopID, token); !errors.Is(err, auth.ErrNoSession) {
		t.Error("a new password left the old session working")
	}
	if _, _, err := auth.Login(ctx, pool, shopID, "erik@verkstaden.test", "a new password entirely", "x"); err != nil {
		t.Errorf("the new password does not sign in: %v", err)
	}
}

// The last owner who can sign in cannot be switched off: a workshop with no
// owner has nobody who can add one back.
func TestTheLastOwnerStays(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	first, _ := workshop.AddStaff(ctx, pool, owner(), "Första Ägaren", "first@verkstaden.test", access.RoleOwner, firstPassword)
	second, _ := workshop.AddStaff(ctx, pool, owner(), "Andra Ägaren", "second@verkstaden.test", access.RoleOwner, firstPassword)
	asFirst := access.Scope{ShopID: shopID, UserID: first, Role: access.Of(access.RoleOwner), Modules: workshopOnly}
	asSecond := access.Scope{ShopID: shopID, UserID: second, Role: access.Of(access.RoleOwner), Modules: workshopOnly}

	if err := workshop.SetStaffActive(ctx, pool, asFirst, second, false); err != nil {
		t.Fatalf("one owner switching the other off: %v", err)
	}
	// The fixture's own user is a front desk, so the first owner is the last.
	if err := workshop.SetStaffActive(ctx, pool, asSecond, first, false); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("switching off the last owner: %v, want refused", err)
	}
	if err := workshop.SetStaffActive(ctx, pool, asFirst, first, false); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("switching yourself off: %v, want refused", err)
	}
}

// Managing staff is for whoever runs the shop.
func TestOnlyThoseWhoRunTheShopManageStaff(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	for _, s := range []access.Scope{technician(), advisor(), partsDeskScope()} {
		if _, err := workshop.Staff(ctx, pool, s); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("Staff as %s: %v", s.Role, err)
		}
		if _, err := workshop.AddStaff(ctx, pool, s, "X", "x@verkstaden.test", access.RoleOwner, firstPassword); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("AddStaff as %s: %v", s.Role, err)
		}
		if err := workshop.SetStaffActive(ctx, pool, s, techUserID, false); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("SetStaffActive as %s: %v", s.Role, err)
		}
		if err := workshop.SetStaffPassword(ctx, pool, s, techUserID, firstPassword); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("SetStaffPassword as %s: %v", s.Role, err)
		}
	}
}

// A role belongs to a module, and a shop gives only the roles of the modules
// it runs (#145): a rental desk in a shop that does not rent cannot be made.
// Core's -- owner, administrator -- are always there.
func TestARoleIsGivenOnlyIfTheShopRunsItsModule(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	for _, r := range []access.Role{access.RoleRentalAgent, access.RoleSales, access.RoleBuyer} {
		if _, err := workshop.AddStaff(ctx, pool, owner(), "Nobody", string(r)+"@verkstaden.test", r, firstPassword); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("a %s in a workshop-only shop: %v, want refused", r, err)
		}
	}
	offered := workshop.RolesOffered(owner())
	for _, r := range offered {
		if r.Module() != "" && r.Module() != access.Workshop {
			t.Errorf("a workshop-only shop is offered %s", r)
		}
	}
	all := owner()
	all.Modules = access.Modules
	if got := workshop.RolesOffered(all); len(got) != len(access.AllRoles) {
		t.Errorf("a shop running everything is offered %v", got)
	}
	if _, err := workshop.AddStaff(ctx, pool, all, "Hyra", "hyra@verkstaden.test", access.RoleRentalAgent, firstPassword); err != nil {
		t.Errorf("a rental desk in a shop that rents: %v", err)
	}
}

// One person, several roles (#145). A salesperson given the buyer's role
// sees purchase prices from their next request, through the buyer's
// capability; nobody changes their own roles; the last owner keeps theirs.
func TestAPersonHoldsSeveralRolesFromTheirNextRequest(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	all := owner()
	all.Modules = access.Modules
	id, err := workshop.AddStaff(ctx, pool, all, "Sälj", "salj@verkstaden.test", access.RoleSales, firstPassword)
	if err != nil {
		t.Fatal(err)
	}
	token, s, err := auth.Login(ctx, pool, shopID, "salj@verkstaden.test", firstPassword, "test")
	if err != nil || s.Scope.Role.SeesPurchasePrice() {
		t.Fatalf("the salesperson signed in as %v (%v), seeing purchase prices", s.Scope.Role, err)
	}

	if err := workshop.SetStaffRoles(ctx, pool, all, id, access.RoleSales, []access.Role{access.RoleBuyer}); err != nil {
		t.Fatalf("SetStaffRoles: %v", err)
	}
	s, err = auth.Authenticate(ctx, pool, shopID, token)
	if err != nil || s.Scope.Role.Main() != access.RoleSales || !s.Scope.Role.SeesPurchasePrice() {
		t.Errorf("the next request is %v (%v); want sales first, and the buyer's purchase prices", s.Scope.Role, err)
	}
	staff, _ := workshop.Staff(ctx, pool, all)
	for _, m := range staff {
		if m.UserID == id && (m.Role != access.RoleSales || len(m.Others) != 1 || m.Others[0] != access.RoleBuyer) {
			t.Errorf("the staff list shows %s and %v", m.Role, m.Others)
		}
	}

	// Taken away again: from the next request.
	if err := workshop.SetStaffRoles(ctx, pool, all, id, access.RoleSales, nil); err != nil {
		t.Fatal(err)
	}
	if s, _ = auth.Authenticate(ctx, pool, shopID, token); s.Scope.Role.SeesPurchasePrice() {
		t.Error("the buyer's role taken away still shows purchase prices")
	}

	// In a shop that runs only the workshop, a dealer's role is not given --
	// but one already held is kept when the rest change.
	if err := workshop.SetStaffRoles(ctx, pool, owner(), id, access.RoleSales, []access.Role{access.RoleBuyer}); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("giving the buyer's role with no dealer: %v, want refused", err)
	}
	if err := workshop.SetStaffRoles(ctx, pool, owner(), id, access.RoleSales, []access.Role{access.RoleTechnician}); err != nil {
		t.Errorf("adding a workshop role to a salesperson the shop already has: %v", err)
	}

	if err := workshop.SetStaffRoles(ctx, pool, owner(), userID, access.RoleTechnician, nil); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("changing one's own roles: %v, want refused", err)
	}
	if err := workshop.SetStaffRoles(ctx, pool, advisor(), id, access.RoleSales, nil); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the front desk changing roles: %v, want forbidden", err)
	}
}

// The shop is never left without an owner who can sign in, whether the
// owner's role is somebody's main one or another they hold.
func TestTheLastOwnerKeepsTheirRole(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	second, err := workshop.AddStaff(ctx, pool, owner(), "Andra", "andra@verkstaden.test", access.RoleServiceAdvisor, firstPassword)
	if err != nil {
		t.Fatal(err)
	}
	// The second person is made an owner as another role; then the first
	// owner, by the second, loses theirs -- allowed, the second is one.
	if err := workshop.SetStaffRoles(ctx, pool, owner(), second, access.RoleServiceAdvisor, []access.Role{access.RoleOwner}); err != nil {
		t.Fatal(err)
	}
	asSecond := access.Scope{ShopID: shopID, UserID: second, Role: access.Of(access.RoleServiceAdvisor, access.RoleOwner), Modules: workshopOnly}
	if err := workshop.SetStaffRoles(ctx, pool, asSecond, owner().UserID, access.RoleServiceAdvisor, nil); err != nil {
		t.Fatalf("taking the owner's role from one of two owners: %v", err)
	}
	// Now the second is the only owner, and nobody may take it from them.
	asFirst := access.Scope{ShopID: shopID, UserID: owner().UserID, Role: access.Of(access.RoleOwner), Modules: workshopOnly}
	if err := workshop.SetStaffRoles(ctx, pool, asFirst, second, access.RoleServiceAdvisor, nil); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("taking the role of the last owner: %v, want refused", err)
	}
	if err := workshop.SetStaffActive(ctx, pool, asFirst, second, false); err == nil {
		t.Error("the last owner, an owner by another role, was switched off")
	}
}
