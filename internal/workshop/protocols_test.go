package workshop_test

import (
	"context"
	"errors"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/testsupport"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Inspections with a subject other than a job (#147). The modules that
// start them have no pages yet, so the shop here runs every module by its
// scope, as a session in such a shop would carry.

const rentalA = "66666666-0000-0000-0000-000000000001"

func everything(s access.Scope) access.Scope {
	s.Modules = access.Modules
	return s
}

// rentalChecklist is a checklist for handing a rental over and taking it
// back, the same checkpoints for both so the pair can be compared.
func rentalChecklist(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	id, err := workshop.SaveChecklistTemplate(ctx, pool, owner(), "", "Utlämning och återlämning",
		[]string{"Lack och plåt", "Fälgar", "Tankad"})
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.SetTemplateSubjects(ctx, pool, everything(owner()), id,
		[]string{workshop.SubjectCheckOut, workshop.SubjectCheckIn}); err != nil {
		t.Fatalf("SetTemplateSubjects: %v", err)
	}
	return id
}

func checkOut(t *testing.T, pool *pgxpool.Pool, template, key string) string {
	t.Helper()
	id, err := workshop.StartProtocol(context.Background(), pool, otherRoles(access.Of(access.RoleRentalAgent)),
		workshop.ProtocolSubject{Kind: workshop.SubjectCheckOut, RefID: rentalA, VehicleID: vehicleA}, template, "", key)
	if err != nil {
		t.Fatalf("StartProtocol: %v", err)
	}
	return id
}

// A check-out is started from a checklist declared for it, by the rental
// desk in a shop that rents, and a phone's replay of the same request finds
// the protocol it already started.
func TestACheckOutIsStartedOnceFromAChecklistForIt(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	tpl := rentalChecklist(t, pool)
	first := checkOut(t, pool, tpl, "phone-1")
	if again := checkOut(t, pool, tpl, "phone-1"); again != first {
		t.Errorf("a replayed start made a second protocol: %s, then %s", first, again)
	}
	desk := otherRoles(access.Of(access.RoleRentalAgent))
	p, err := workshop.ProtocolByID(ctx, pool, desk, first)
	if err != nil {
		t.Fatal(err)
	}
	if p.Subject != workshop.SubjectCheckOut || p.VehicleID != vehicleA || len(p.Items) != 3 || p.Items[0].Key != "lack och plåt" {
		t.Errorf("the protocol reads %+v", p)
	}

	// A workshop checklist is not offered for a check-out, and this one is
	// not offered to the workshop.
	work, _ := workshop.SaveChecklistTemplate(ctx, pool, owner(), "", "Vårservice", []string{"Bromsar"})
	if _, err := workshop.StartProtocol(ctx, pool, desk, workshop.ProtocolSubject{Kind: workshop.SubjectCheckOut,
		RefID: rentalA, VehicleID: vehicleA}, work, "", ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a check-out from the workshop's checklist: %v, want refused", err)
	}
	tpls, _ := workshop.Templates(ctx, pool, owner())
	for _, x := range tpls {
		if x.ID == tpl {
			t.Error("the workshop is offered the rental's checklist")
		}
	}
	if _, err := workshop.StartInspection(ctx, pool, owner(), newJob(t, pool), tpl); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("a job inspected with the rental's checklist: %v, want not found", err)
	}

	// Who may: the rental desk and the owner, in a shop that rents.
	for name, s := range map[string]access.Scope{
		"sales": otherRoles(access.Of(access.RoleSales)), "technician": everything(technician()), "front desk": everything(advisor()),
	} {
		if _, err := workshop.StartProtocol(ctx, pool, s, workshop.ProtocolSubject{Kind: workshop.SubjectCheckOut,
			RefID: rentalA, VehicleID: vehicleA}, tpl, "", ""); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s starting a check-out: %v, want forbidden", name, err)
		}
	}
	noRental := desk
	noRental.Modules = []access.Module{access.Workshop, access.Dealer}
	if _, err := workshop.StartProtocol(ctx, pool, noRental, workshop.ProtocolSubject{Kind: workshop.SubjectCheckOut,
		RefID: rentalA, VehicleID: vehicleA}, tpl, "", ""); !errors.Is(err, access.ErrModuleOff) {
		t.Errorf("a check-out with rental off: %v, want ErrModuleOff", err)
	}
	if _, err := workshop.StartProtocol(ctx, pool, everything(owner()), workshop.ProtocolSubject{Kind: workshop.SubjectWorkOrder,
		RefID: rentalA, VehicleID: vehicleA}, tpl, "", ""); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a job's inspection through StartProtocol: %v, want refused", err)
	}
}

// Completed, a protocol is frozen -- its items, answers and photographs --
// by the database, whatever writes to it. A correction is a new protocol
// naming the one it supersedes. One abandoned is kept and frozen too.
func TestAFinishedProtocolIsFrozenAndCorrectedByANewOne(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	desk := otherRoles(access.Of(access.RoleRentalAgent))
	tpl := rentalChecklist(t, pool)
	id := checkOut(t, pool, tpl, "")
	p, _ := workshop.ProtocolByID(ctx, pool, desk, id)
	item := p.Items[0].ID
	if err := workshop.RecordProtocolItem(ctx, pool, desk, item, "attention", "Repa på vänster dörr"); err != nil {
		t.Fatal(err)
	}
	if err := workshop.AttachProtocolPhoto(ctx, pool, desk, item, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "image/jpeg", 10, "dorr.jpg"); err != nil {
		t.Fatal(err)
	}
	if err := workshop.CompleteProtocol(ctx, pool, desk, id); err != nil {
		t.Fatal(err)
	}

	if err := workshop.RecordProtocolItem(ctx, pool, desk, item, "pass", ""); !errors.Is(err, workshop.ErrProtocolFinished) {
		t.Errorf("changing a finished protocol's item: %v, want ErrProtocolFinished", err)
	}
	if err := workshop.AttachProtocolPhoto(ctx, pool, desk, item, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "image/jpeg", 10, ""); !errors.Is(err, workshop.ErrProtocolFinished) {
		t.Errorf("a photograph added to a finished protocol: %v, want ErrProtocolFinished", err)
	}
	for _, stmt := range []string{
		`UPDATE inspection_items SET note = 'ändrad' WHERE inspection_id = $1`,
		`DELETE FROM inspection_items WHERE inspection_id = $1`,
		`UPDATE inspections SET template_name = 'annan' WHERE id = $1`,
		`DELETE FROM attachments WHERE inspection_item_id IN (SELECT id FROM inspection_items WHERE inspection_id = $1)`,
		`DELETE FROM inspections WHERE id = $1`,
		`DELETE FROM vehicles WHERE id = (SELECT vehicle_id FROM inspections WHERE id = $1)`,
	} {
		if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, stmt, id)
			return err
		}); err == nil {
			t.Errorf("%q was allowed on a finished protocol", stmt)
		}
	}
	// Nor through the workshop's own operations, which are for a job's.
	if err := workshop.SetItem(ctx, pool, everything(technician()), item, "pass", ""); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("the workshop's SetItem on a protocol's item: %v, want not found", err)
	}

	fixed, err := workshop.StartProtocol(ctx, pool, desk, workshop.ProtocolSubject{Kind: workshop.SubjectCheckOut,
		RefID: rentalA, VehicleID: vehicleA}, tpl, id, "")
	if err != nil {
		t.Fatalf("a correction: %v", err)
	}
	if c, _ := workshop.ProtocolByID(ctx, pool, desk, fixed); c.SupersedesID != id {
		t.Errorf("the correction supersedes %q, want %s", c.SupersedesID, id)
	}

	left := checkOut(t, pool, tpl, "")
	if err := workshop.AbandonProtocol(ctx, pool, desk, left, "Kunden ångrade sig"); err != nil {
		t.Fatal(err)
	}
	if err := workshop.CompleteProtocol(ctx, pool, desk, left); !errors.Is(err, workshop.ErrProtocolFinished) {
		t.Errorf("completing an abandoned protocol: %v, want ErrProtocolFinished", err)
	}
}

// A customer's answer, and the link they give it on, are for a job's
// findings: a check-in has nobody to approve a price.
func TestOnlyAJobsInspectionIsPutToTheCustomer(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	id := checkOut(t, pool, rentalChecklist(t, pool), "")
	if _, err := workshop.CreateShare(ctx, pool, everything(advisor()), id); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("a customer link to a check-out: %v, want not found", err)
	}
	err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO inspection_decisions (shop_id, item_id, decision, decided_by)
			SELECT shop_id, id, 'approved', $2 FROM inspection_items WHERE inspection_id = $1 LIMIT 1`, id, userID)
		return err
	})
	if err == nil {
		t.Error("a customer decision on a check-out's item was written")
	}
}

// Who reads a protocol: the roles of its subject's module, and the
// workshop's people who follow jobs -- the car they repair is the one it is
// of. Never with a party on it: a technician reading a check-in is handed
// the vehicle and the findings, and no field could carry the renter.
func TestATechnicianReadsACheckInWithNoRenterOnIt(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()
	id := checkOut(t, pool, rentalChecklist(t, pool), "")
	if fields := testsupport.FieldsLike(workshop.Protocol{}, `(?i)customer|renter|party|owner|phone|email|address|person`); len(fields) > 0 {
		t.Errorf("a protocol has fields that could carry a person: %v", fields)
	}
	if fields := testsupport.FieldsLike(workshop.ProtocolItem{}, `(?i)customer|renter|party|phone|email|address|person`); len(fields) > 0 {
		t.Errorf("a protocol's item has fields that could carry a person: %v", fields)
	}
	if p, err := workshop.ProtocolByID(ctx, pool, everything(technician()), id); err != nil || p.ID != id {
		t.Errorf("the technician reading the check-out: %v", err)
	}
	if list, err := workshop.ProtocolsForVehicle(ctx, pool, everything(technician()), vehicleA); err != nil || len(list) != 1 {
		t.Errorf("the vehicle's protocols for the technician: %d, %v", len(list), err)
	}
	for name, s := range map[string]access.Scope{
		"sales":                      otherRoles(access.Of(access.RoleSales)),
		"the parts desk":             everything(partsDeskScope()),
		"a technician, workshop off": func() access.Scope { s := technician(); s.Modules = []access.Module{access.Rental}; return s }(),
	} {
		p, err := workshop.ProtocolByID(ctx, pool, s, id)
		if !errors.Is(err, access.ErrForbidden) || p.ID != "" {
			t.Errorf("%s reading a check-out: %v, want forbidden and nothing handed", name, err)
		}
		if list, err := workshop.ProtocolsForVehicle(ctx, pool, s, vehicleA); err != nil || len(list) != 0 {
			t.Errorf("%s is listed %d protocols (%v)", name, len(list), err)
		}
	}
	off := otherRoles(access.Of(access.RoleRentalAgent))
	off.Modules = []access.Module{access.Workshop}
	if _, err := workshop.ProtocolByID(ctx, pool, off, id); !errors.Is(err, access.ErrModuleOff) {
		t.Errorf("the rental desk with rental off: %v, want ErrModuleOff", err)
	}
}

// Checklists are Core (owner decision on #143): kept whatever the shop
// runs, and tagged with what they are for. A shop without a workshop makes
// them for what its modules do.
func TestChecklistsAreCoreAndSayWhatTheyAreFor(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	dealerOnly := owner()
	dealerOnly.Modules = []access.Module{access.Dealer}
	id, err := workshop.SaveChecklistTemplate(ctx, pool, dealerOnly, "", "Inbyteskontroll", []string{"Lack", "Däck"})
	if err != nil {
		t.Fatalf("a checklist in a shop without a workshop: %v", err)
	}
	c, err := workshop.ChecklistTemplateByID(ctx, pool, dealerOnly, id)
	if err != nil || !c.UsedFor(workshop.SubjectPurchase) || !c.UsedFor(workshop.SubjectDelivery) || c.UsedFor(workshop.SubjectWorkOrder) {
		t.Errorf("a dealer's new checklist is for %v (%v); want the purchase and the delivery", c.Subjects, err)
	}
	if _, err := workshop.ChecklistTemplates(ctx, pool, runningNothing(owner())); err != nil {
		t.Errorf("the checklists with every module off: %v", err)
	}
	if err := workshop.SetTemplateSubjects(ctx, pool, dealerOnly, id, []string{workshop.SubjectCheckOut}); !errors.Is(err, access.ErrModuleOff) {
		t.Errorf("tagging a checklist for rental in a shop that does not rent: %v, want ErrModuleOff", err)
	}
	if got := workshop.SubjectsOffered(owner()); len(got) != 1 || got[0] != workshop.SubjectWorkOrder {
		t.Errorf("a workshop-only shop is offered %v", got)
	}
}

// A photograph on a finished protocol is still personal data, and the
// privacy deletion removes it -- but the protocol is evidence, so it keeps a
// record that it had one: from which checkpoint, when, by whom and why.
// Without a reason it is refused, readably, and the database still refuses
// a deletion that recorded nothing.
func TestAPhotoIsRemovedFromAFinishedProtocolAndTheProtocolSaysSo(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	desk := otherRoles(access.Of(access.RoleRentalAgent))
	id := checkOut(t, pool, rentalChecklist(t, pool), "")
	p, _ := workshop.ProtocolByID(ctx, pool, desk, id)
	item := p.Items[0].ID
	const key = "cccccccccccccccccccccccccccccccc"
	if err := workshop.AttachProtocolPhoto(ctx, pool, desk, item, key, "image/jpeg", 10, "dorr.jpg"); err != nil {
		t.Fatal(err)
	}
	if err := workshop.CompleteProtocol(ctx, pool, desk, id); err != nil {
		t.Fatal(err)
	}
	var attachment string
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM attachments WHERE storage_key = $1`, key).Scan(&attachment)
	}); err != nil {
		t.Fatal(err)
	}

	remover := &recordingRemover{}
	if err := workshop.DeleteAttachment(ctx, pool, advisor(), remover, attachment, " "); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("removed from a protocol with no reason: %v, want refused", err)
	}
	if err := workshop.DeleteAttachment(ctx, pool, advisor(), remover, attachment, "En förbipasserande syns i bild"); err != nil {
		t.Fatalf("removing a photograph from a finished protocol: %v", err)
	}
	if len(remover.removed) != 1 || remover.removed[0] != key {
		t.Errorf("the file was not removed: %v", remover.removed)
	}
	p, err := workshop.ProtocolByID(ctx, pool, desk, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Items[0].Photos) != 0 {
		t.Error("the photograph is still on the protocol")
	}
	if len(p.Removals) != 1 {
		t.Fatalf("the protocol records %d removals, want 1", len(p.Removals))
	}
	r := p.Removals[0]
	if r.ItemID != item || r.Reason != "En förbipasserande syns i bild" || r.RemovedBy == "" || r.RemovedAt.IsZero() {
		t.Errorf("the removal reads %+v", r)
	}
	if err := workshop.DeleteAttachment(ctx, pool, advisor(), remover, attachment, "Igen"); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("removing it a second time: %v, want not found", err)
	}
}
