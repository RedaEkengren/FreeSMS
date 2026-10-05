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
	if s.Scope.UserID != id || s.Scope.Role != access.RoleTechnician {
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
	asFirst := access.Scope{ShopID: shopID, UserID: first, Role: access.RoleOwner}
	asSecond := access.Scope{ShopID: shopID, UserID: second, Role: access.RoleOwner}

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
