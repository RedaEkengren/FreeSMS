package access

import "testing"

// Who has which capability, for every role (#145). The workshop's five are
// as they were before roles for car sales and rental came; the three new
// ones have none of the workshop's, and only the buyer -- and whoever runs
// the business -- sees what a car cost.
func TestEachRoleHasTheCapabilitiesItsWorkNeedsAndNoMore(t *testing.T) {
	caps := map[string]func(Role) bool{
		"SeesCustomerPersonalData": Role.SeesCustomerPersonalData,
		"RunsTheShop":              Role.RunsTheShop,
		"SeesParts":                Role.SeesParts,
		"HandlesParts":             Role.HandlesParts,
		"FollowsJobs":              Role.FollowsJobs,
		"WorksInTheWorkshop":       Role.WorksInTheWorkshop,
		"SellsVehicles":            Role.SellsVehicles,
		"BuysVehicles":             Role.BuysVehicles,
		"SeesPurchasePrice":        Role.SeesPurchasePrice,
		"SeesMargin":               Role.SeesMargin,
		"HandsOverRentals":         Role.HandsOverRentals,
		"SeesIdentityRecords":      Role.SeesIdentityRecords,
	}
	has := map[Role][]string{
		RoleOwner: {"SeesCustomerPersonalData", "RunsTheShop", "SeesParts", "HandlesParts", "FollowsJobs", "WorksInTheWorkshop",
			"SellsVehicles", "BuysVehicles", "SeesPurchasePrice", "SeesMargin", "HandsOverRentals", "SeesIdentityRecords"},
		RoleAdmin: {"SeesCustomerPersonalData", "RunsTheShop", "SeesParts", "HandlesParts", "FollowsJobs", "WorksInTheWorkshop",
			"SellsVehicles", "BuysVehicles", "SeesPurchasePrice", "SeesMargin", "HandsOverRentals", "SeesIdentityRecords"},
		RoleServiceAdvisor: {"SeesCustomerPersonalData", "SeesParts", "HandlesParts", "FollowsJobs", "WorksInTheWorkshop"},
		RoleTechnician:     {"HandlesParts", "FollowsJobs", "WorksInTheWorkshop"},
		RoleParts:          {"SeesParts", "HandlesParts", "WorksInTheWorkshop"},
		RoleSales:          {"SellsVehicles", "SeesIdentityRecords"},
		RoleBuyer:          {"BuysVehicles", "SeesPurchasePrice", "SeesMargin", "SeesIdentityRecords"},
		RoleRentalAgent:    {"HandsOverRentals", "SeesIdentityRecords"},
	}
	if len(has) != len(AllRoles) {
		t.Fatalf("the table covers %d roles of %d", len(has), len(AllRoles))
	}
	for _, r := range AllRoles {
		want := map[string]bool{}
		for _, c := range has[r] {
			want[c] = true
		}
		for name, c := range caps {
			if c(r) != want[name] {
				t.Errorf("%s.%s() = %v, want %v", r, name, c(r), want[name])
			}
		}
	}
}

// A person with several roles has a capability when any role has it: a
// salesperson who is also the buyer sees purchase prices because the buyer
// does, not by an exception for the pair.
func TestSeveralRolesHaveWhatAnyOfThemHas(t *testing.T) {
	sales := Of(RoleSales)
	if sales.SeesPurchasePrice() || sales.SeesMargin() {
		t.Error("a salesperson sees what a car cost")
	}
	both := Of(RoleSales, RoleBuyer)
	if !both.SeesPurchasePrice() || !both.SellsVehicles() || both.SeesCustomerPersonalData() {
		t.Errorf("sales and buyer: %v %v %v", both.SeesPurchasePrice(), both.SellsVehicles(), both.SeesCustomerPersonalData())
	}
	if both.Main() != RoleSales || both.Label() != "Salesperson" {
		t.Errorf("the main role is %s (%s), want the first given", both.Main(), both.Label())
	}
	if got := Of(RoleParts, RoleTechnician, RoleParts); len(got) != 2 || !got.Has(RoleTechnician) {
		t.Errorf("Of with a repeat = %v", got)
	}
	if (Roles{}).Valid() || Of("superuser").Valid() || Of(RoleOwner, "superuser").Valid() || !Of(RoleRentalAgent).Valid() {
		t.Error("Roles.Valid accepts nothing, or an unknown role, or refuses a known one")
	}
}

// Each role belongs to the module its work is in, or to Core.
func TestARoleBelongsToTheModuleItWorksIn(t *testing.T) {
	want := map[Role]Module{
		RoleOwner: "", RoleAdmin: "", RoleServiceAdvisor: Workshop, RoleTechnician: Workshop, RoleParts: Workshop,
		RoleSales: Dealer, RoleBuyer: Dealer, RoleRentalAgent: Rental,
	}
	for _, r := range AllRoles {
		if r.Module() != want[r] {
			t.Errorf("%s belongs to %q, want %q", r, r.Module(), want[r])
		}
	}
}
