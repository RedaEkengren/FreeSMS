package workshop_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A unit's figures come from its actual handovers and its standing
// invoices: rented by the hour across the night the clocks go back, its
// rent spread over the days rented, and nothing once the invoice is
// credited. Whoever runs the shop reads them; nobody else (#203).
func TestARentalUnitsFiguresComeFromItsHandoversAndStandingInvoices(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	booking := rentedAcrossTheClockChange(t, pool)
	b, err := workshop.RentalBookingByID(ctx, pool, rentalDesk(), booking)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := workshop.IssueRentalInvoice(ctx, pool, rentalDesk(), booking, "")
	if err != nil {
		t.Fatal(err)
	}
	october := func(from time.Time) workshop.UnitFigures {
		t.Helper()
		figs, err := workshop.RentalFigures(ctx, pool, rentalOwner(), from, time.Date(2026, 11, 1, 0, 0, 0, 0, stockholm))
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(figs, func(f workshop.UnitFigures) bool { return f.UnitID == b.UnitID })
		if i < 0 {
			t.Fatalf("the unit is not in the figures: %+v", figs)
		}
		return figs[i]
	}
	whole := october(time.Date(2026, 10, 1, 0, 0, 0, 0, stockholm))
	// Noon on the 24th to noon on the 26th is 49 hours: the night of the
	// 25th has 25.
	if whole.Rented != 49*time.Hour || whole.RentMinor != b.Totals.NetMinor || whole.Utilisation() <= 0 {
		t.Errorf("October: rented %v, rent %d (booked net %d), utilisation %d", whole.Rented, whole.RentMinor, b.Totals.NetMinor, whole.Utilisation())
	}
	// From midnight on the 25th: 37 of the 49 hours.
	part := october(time.Date(2026, 10, 25, 0, 0, 0, 0, stockholm))
	want := (b.Totals.NetMinor*37 + 49/2) / 49
	if part.Rented != 37*time.Hour || part.RentMinor != want {
		t.Errorf("from the 25th: rented %v, rent %d; want 37h and %d", part.Rented, part.RentMinor, want)
	}
	for _, s := range []access.Scope{rentalDesk(), technician()} {
		if _, err := workshop.RentalFigures(ctx, pool, s, time.Now().AddDate(0, -1, 0), time.Now()); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%v read the figures: %v", s.Role, err)
		}
	}
	if _, err := workshop.CreditNote(ctx, pool, everything(advisor()), inv.ID); err != nil {
		t.Fatal(err)
	}
	if after := october(time.Date(2026, 10, 1, 0, 0, 0, 0, stockholm)); after.RentMinor != 0 || after.Rented != 49*time.Hour {
		t.Errorf("after the credit note: rent %d, rented %v; want no rent, the time still rented", after.RentMinor, after.Rented)
	}
}
