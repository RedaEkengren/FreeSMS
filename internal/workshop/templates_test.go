package workshop_test

import (
	"context"
	"errors"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A shop with nothing set up makes a checklist and starts an inspection from
// it, with no database access by hand. #11 asked for this and it was never
// built: the only templates were the ones tests inserted.
func TestAShopMakesItsOwnChecklist(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)

	id, err := workshop.SaveChecklistTemplate(ctx, pool, owner(), "", "Vårservice",
		workshop.ParseCheckpoints("Bromsar fram\r\n\n  Däck  \nTorkarblad\n"))
	if err != nil {
		t.Fatalf("SaveChecklistTemplate: %v", err)
	}
	got, err := workshop.ChecklistTemplateByID(ctx, pool, owner(), id)
	if err != nil {
		t.Fatalf("ChecklistTemplateByID: %v", err)
	}
	if want := []string{"Bromsar fram", "Däck", "Torkarblad"}; !equal(got.Checkpoints, want) {
		t.Errorf("checkpoints = %q, want %q (blank lines and spaces dropped, order kept)", got.Checkpoints, want)
	}

	inspectionID, err := workshop.StartInspection(ctx, pool, technician(), working(t, pool), id)
	if err != nil {
		t.Fatalf("StartInspection from a shop-made checklist: %v", err)
	}
	insp, _ := workshop.InspectionsForID(ctx, pool, technician(), inspectionID)
	if len(insp.Items) != 3 || insp.Items[1].Label != "Däck" {
		t.Errorf("the inspection did not get the checklist: %+v", insp.Items)
	}
}

// Editing a checklist changes the next inspection and not the last one. The
// customer's link to an old inspection shows what was checked then.
func TestEditingAChecklistLeavesPastInspectionsAlone(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	jobID, before := startedInspection(t, pool)

	// Reordered, one renamed, one removed, one added.
	if _, err := workshop.SaveChecklistTemplate(ctx, pool, owner(), templateID, "Service check, 2027",
		[]string{"Tyres", "Front brakes and discs", "Wiper blades"}); err != nil {
		t.Fatalf("save: %v", err)
	}

	old, _ := workshop.InspectionsForID(ctx, pool, technician(), before)
	if len(old.Items) != 3 || old.Items[0].Label != "Front brakes" || old.Items[2].Label != "Tyres" {
		t.Errorf("an edit reached a past inspection: %+v", old.Items)
	}
	if old.TemplateName != "Service check" {
		t.Errorf("past inspection's name = %q, want the name it was made under", old.TemplateName)
	}

	after, err := workshop.StartInspection(ctx, pool, technician(), jobID, templateID)
	if err != nil {
		t.Fatalf("StartInspection: %v", err)
	}
	next, _ := workshop.InspectionsForID(ctx, pool, technician(), after)
	if len(next.Items) != 3 || next.Items[0].Label != "Tyres" || next.Items[1].Label != "Front brakes and discs" {
		t.Errorf("the next inspection did not get the edit: %+v", next.Items)
	}
}

// A retired checklist is not offered and cannot be started, and its history
// stays. It can be brought back.
func TestARetiredChecklistIsNotOffered(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	jobID, past := startedInspection(t, pool)

	if err := workshop.SetChecklistActive(ctx, pool, owner(), templateID, false); err != nil {
		t.Fatalf("retire: %v", err)
	}
	offered, _ := workshop.Templates(ctx, pool, technician())
	if len(offered) != 0 {
		t.Errorf("a retired checklist is still offered: %+v", offered)
	}
	if _, err := workshop.StartInspection(ctx, pool, technician(), jobID, templateID); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("starting a retired checklist: %v, want not found", err)
	}
	if insp, err := workshop.InspectionsForID(ctx, pool, technician(), past); err != nil || len(insp.Items) != 3 {
		t.Errorf("retiring took a past inspection with it: %v %+v", err, insp.Items)
	}
	all, _ := workshop.ChecklistTemplates(ctx, pool, owner())
	if len(all) != 1 || all[0].Active || all[0].Used != 1 {
		t.Errorf("the owner's list = %+v, want the one retired checklist, used once", all)
	}

	if err := workshop.SetChecklistActive(ctx, pool, owner(), templateID, true); err != nil {
		t.Fatalf("bring back: %v", err)
	}
	if offered, _ := workshop.Templates(ctx, pool, technician()); len(offered) != 1 {
		t.Errorf("a checklist brought back is not offered")
	}
}

// Only the people who run the shop change the checklists; everybody else
// uses them. Refused before anything is read or written.
func TestOnlyThoseWhoRunTheShopEditChecklists(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTemplate(t, pool)

	for name, scope := range map[string]access.Scope{"technician": technician(), "front desk": advisor()} {
		if _, err := workshop.SaveChecklistTemplate(ctx, pool, scope, "", "Mine", []string{"A"}); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s creating: %v, want forbidden", name, err)
		}
		if _, err := workshop.SaveChecklistTemplate(ctx, pool, scope, templateID, "Mine", []string{"A"}); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s editing: %v, want forbidden", name, err)
		}
		if err := workshop.SetChecklistActive(ctx, pool, scope, templateID, false); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s retiring: %v, want forbidden", name, err)
		}
		if _, err := workshop.ChecklistTemplates(ctx, pool, scope); !errors.Is(err, access.ErrForbidden) {
			t.Errorf("%s listing for editing: %v, want forbidden", name, err)
		}
	}
	got, _ := workshop.ChecklistTemplateByID(ctx, pool, owner(), templateID)
	if got.Name != "Service check" || len(got.Checkpoints) != 3 {
		t.Errorf("a refused edit changed the checklist: %+v", got)
	}
}

// What a checklist may not be.
func TestAChecklistMustBeUsable(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTemplate(t, pool)

	long := make([]rune, 201)
	for i := range long {
		long[i] = 'x'
	}
	many := make([]string, 101)
	for i := range many {
		many[i] = string(rune('A'+i%26)) + string(rune('a'+i/26))
	}
	for name, c := range map[string]struct {
		name   string
		points []string
	}{
		"no name":          {"  ", []string{"Brakes"}},
		"nothing to check": {"Empty", workshop.ParseCheckpoints("\n \n")},
		"twice":            {"Twice", []string{"Brakes", "brakes"}},
		"too long":         {"Long", []string{string(long)}},
		"too many":         {"Many", many},
	} {
		if _, err := workshop.SaveChecklistTemplate(ctx, pool, owner(), templateID, c.name, c.points); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("%s: %v, want invalid", name, err)
		}
	}
	// None of those got halfway: the checklist is as it was.
	got, _ := workshop.ChecklistTemplateByID(ctx, pool, owner(), templateID)
	if got.Name != "Service check" || len(got.Checkpoints) != 3 {
		t.Errorf("a refused save changed the checklist: %+v", got)
	}
	if _, err := workshop.ChecklistTemplateByID(ctx, pool, owner(), "not-an-id"); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("a malformed id: %v, want not found", err)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
