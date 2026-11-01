package workshop

import (
	"context"
	"errors"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
)

// Only an operation of a module the shop runs issues that module's kind of
// invoice (#143, #146): a vehicle sale needs car sales, a rental needs
// rental, and both are refused before anything is read. A manual invoice is
// Core's; the workshop's goes through Issue, not here.
func TestAnInvoicesSourceNeedsItsModule(t *testing.T) {
	ctx := context.Background()
	workshopOnly := access.Scope{Role: access.Of(access.RoleOwner), Modules: []access.Module{access.Workshop}}
	line := []SourcedLine{{Description: "x", QuantityMilli: 1000, UnitPriceMinor: 100, VATRateBasis: 2500}}
	for source, module := range map[string]access.Module{SourceVehicleSale: access.Dealer, SourceRental: access.Rental} {
		_, err := issueSourcedTx(ctx, nil, workshopOnly, sourced{source: source}, line)
		if !errors.Is(err, access.ErrModuleOff) {
			t.Errorf("a %s invoice in a workshop-only shop: %v, want ErrModuleOff", source, err)
		}
		if m, _ := sourceModule(source); m != module {
			t.Errorf("%s belongs to %q, want %q", source, m, module)
		}
	}
	for _, source := range []string{SourceWorkshop, "gift"} {
		if _, err := issueSourcedTx(ctx, nil, workshopOnly, sourced{source: source}, line); !errors.Is(err, ErrInvalid) {
			t.Errorf("issuing %q here: %v, want refused", source, err)
		}
	}
	if m, ok := sourceModule(SourceManual); !ok || m != "" {
		t.Errorf("a manual invoice belongs to %q, want Core", m)
	}
}
