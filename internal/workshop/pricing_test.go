package workshop_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

func lineFromShelf(partID string) workshop.NewLine {
	return workshop.NewLine{Kind: "part", Description: "From the shelf", QuantityMilli: 1000,
		VATRateBasis: 2500, CostBearer: "customer", PartID: partID, PriceFromPart: true}
}

// A part with a cost and no price of its own is sold at its cost with the
// shop's markup -- on the shelf and on the line, the same figure -- and a
// change to the bands changes the next line, not the last one.
func TestTheMarkupBandsPriceAPartOnAJob(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	upTo := int64(100000)
	workshop.SavePriceBand(ctx, pool, advisor(), &upTo, 5000) // under 1000.00: half again
	workshop.SavePriceBand(ctx, pool, advisor(), nil, 2000)

	id, err := workshop.SavePart(ctx, pool, partsDeskScope(), workshop.CatalogueEntry{
		Number: "BP-1", Name: "Brake pads", Unit: "each", CostMinor: price(40000)})
	if err != nil {
		t.Fatalf("SavePart: %v", err)
	}
	job := newJob(t, pool)

	offered, _ := workshop.PartsForJob(ctx, pool, advisor(), job, "BP-1")
	if len(offered) != 1 || offered[0].OfferedMinor == nil || *offered[0].OfferedMinor != 60000 {
		t.Fatalf("the shelf offers %+v, want 600.00", offered)
	}
	if err := workshop.AddLine(ctx, pool, advisor(), job, lineFromShelf(id)); err != nil {
		t.Fatalf("AddLine: %v", err)
	}

	// The shop changes its markup.
	workshop.SavePriceBand(ctx, pool, advisor(), &upTo, 6000)
	offered, _ = workshop.PartsForJob(ctx, pool, advisor(), job, "BP-1")
	if *offered[0].OfferedMinor != 64000 {
		t.Errorf("after the change the shelf offers %d, want 640.00", *offered[0].OfferedMinor)
	}
	if err := workshop.AddLine(ctx, pool, advisor(), job, lineFromShelf(id)); err != nil {
		t.Fatalf("AddLine: %v", err)
	}

	_, lines, _ := workshop.JobByID(ctx, pool, advisor(), job)
	if len(lines) != 2 || lines[0].UnitPriceMinor != 60000 || lines[1].UnitPriceMinor != 64000 {
		t.Errorf("line prices = %v, want 600.00 then 640.00: the first kept, the second new",
			[]int64{lines[0].UnitPriceMinor, lines[1].UnitPriceMinor})
	}
}

// The part's own price comes first, even zero: setting it is a decision.
// Neither a price nor a cost is not zero, and the line is refused, naming
// the part, with nothing reserved.
func TestAPartsOwnPriceComesFirstAndNothingIsNotZero(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	workshop.SavePriceBand(ctx, pool, advisor(), nil, 5000)
	job := newJob(t, pool)

	priced, _ := workshop.SavePart(ctx, pool, partsDeskScope(), workshop.CatalogueEntry{
		Number: "P-1", Name: "Priced", Unit: "each", CostMinor: price(40000), PriceMinor: price(50000)})
	free, _ := workshop.SavePart(ctx, pool, partsDeskScope(), workshop.CatalogueEntry{
		Number: "P-2", Name: "Given away", Unit: "each", CostMinor: price(100), PriceMinor: price(0)})
	unpriced, _ := workshop.SavePart(ctx, pool, partsDeskScope(), workshop.CatalogueEntry{
		Number: "P-3", Name: "Nobody priced this", Unit: "each"})

	for _, id := range []string{priced, free} {
		if err := workshop.AddLine(ctx, pool, advisor(), job, lineFromShelf(id)); err != nil {
			t.Fatalf("AddLine: %v", err)
		}
	}
	err := workshop.AddLine(ctx, pool, advisor(), job, lineFromShelf(unpriced))
	if !errors.Is(err, workshop.ErrInvalid) || !strings.Contains(err.Error(), "P-3") {
		t.Errorf("a part with no price and no cost: %v, want refused naming P-3", err)
	}

	_, lines, _ := workshop.JobByID(ctx, pool, advisor(), job)
	if len(lines) != 2 || lines[0].UnitPriceMinor != 50000 || lines[1].UnitPriceMinor != 0 {
		t.Errorf("lines = %+v; want 500.00 (its price, not cost plus half) and 0 (set on purpose)", lines)
	}
	if p := onHand(t, pool, unpriced); p.Reserved != 0 {
		t.Errorf("the refused line still reserved %v", p.Reserved)
	}
	if offered, _ := workshop.PartsForJob(ctx, pool, advisor(), job, "P-3"); offered[0].OfferedMinor != nil {
		t.Errorf("the shelf offers a price for a part that has none: %d", *offered[0].OfferedMinor)
	}
}
