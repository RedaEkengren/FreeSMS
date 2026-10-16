package workshop_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A colleague taking over a job reads one list, built from the records that
// already hold what happened, plus the notes nothing else could hold. These are
// the handover, a corrected note, and what each role is never even sent.

func kinds(es []workshop.Event) map[string]int {
	m := map[string]int{}
	for _, e := range es {
		m[e.Kind]++
	}
	return m
}

func TestAShiftHandoverShowsWhatHappenedWhatIsLeftAndWhatWasWritten(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	job := inProgress(t, pool, "Replace brake discs", "Replace exhaust")
	if _, err := workshop.TasksFromLines(ctx, pool, advisor(), job); err != nil {
		t.Fatal(err)
	}
	tasks := tasksOf(t, pool, job)
	taskMove(t, pool, technician(), tasks[0].ID, workshop.TaskMove{To: workshop.TaskDone})
	taskMove(t, pool, technician(), tasks[1].ID, workshop.TaskMove{To: workshop.TaskBlocked, Blocker: workshop.BlockerPart})
	if err := workshop.ReportFinding(ctx, pool, technician(), job, "Other track rod end is going"); err != nil {
		t.Fatal(err)
	}
	if err := workshop.RequestPart(ctx, pool, technician(), job, "Exhaust clamp"); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.AddNote(ctx, pool, technician(), job, workshop.NoteInput{Kind: "note", Body: "Disc bolts were seized, used heat"}); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.AddNote(ctx, pool, advisor(), job, workshop.NoteInput{Kind: "conversation", Channel: "phone", Body: "She wants it by Friday, rang from work", DeskOnly: true}); err != nil {
		t.Fatal(err)
	}

	// The counter's list has all of it, in order, with money.
	desk, err := workshop.TimelineFor(ctx, pool, advisor(), job)
	if err != nil {
		t.Fatal(err)
	}
	k := kinds(desk)
	for _, want := range []string{"intake", "state", "scope", "work", "parts", "price", "approval", "note", "conversation"} {
		if k[want] == 0 {
			t.Errorf("the counter's timeline has no %q event: %v", want, k)
		}
	}
	for i := 1; i < len(desk); i++ {
		if desk[i].At.Before(desk[i-1].At) {
			t.Fatalf("the timeline is out of order at %d", i)
		}
	}
	var money bool
	for _, e := range desk {
		if e.AmountMinor != nil {
			money = true
		}
	}
	if !money {
		t.Error("the counter sees no amount anywhere")
	}

	// The technician's has the work and the shop floor's notes, and none of
	// the customer's money, words or documents.
	tech, err := workshop.TimelineFor(ctx, pool, technician(), job)
	if err != nil {
		t.Fatal(err)
	}
	tk := kinds(tech)
	for _, banned := range []string{"price", "approval", "contact", "message", "money", "conversation"} {
		if tk[banned] != 0 {
			t.Errorf("a technician's timeline carries %d %q events", tk[banned], banned)
		}
	}
	for _, e := range tech {
		if e.AmountMinor != nil {
			t.Errorf("a technician's timeline carries an amount: %+v", e)
		}
		if strings.Contains(e.Detail, "Friday") {
			t.Errorf("a technician reads the counter's conversation: %q", e.Detail)
		}
	}
	if tk["note"] != 1 || tk["work"] == 0 {
		t.Errorf("the technician's timeline: %v", tk)
	}

	// The handover says what the next shift needs first.
	h, err := workshop.HandoverFor(ctx, pool, technician(), job)
	if err != nil {
		t.Fatal(err)
	}
	if h.State != workshop.StateAwaitingParts || len(h.Unfinished) != 1 || h.Unfinished[0].Title != "Replace exhaust" || h.Blocked != 1 {
		t.Errorf("handover: state %s, unfinished %+v, blocked %d", h.State, h.Unfinished, h.Blocked)
	}
	if h.OpenFindings != 1 || h.OpenParts != 1 || h.Step.Action == "" || h.LastNote == nil || !strings.Contains(h.LastNote.Detail, "seized") {
		t.Errorf("handover: findings %d, parts %d, next %q, note %v", h.OpenFindings, h.OpenParts, h.Step.Action, h.LastNote)
	}
}

func TestACorrectedNoteKeepsItsEarlierWording(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	job := newJob(t, pool)
	id, err := workshop.AddNote(ctx, pool, technician(), job, workshop.NoteInput{Kind: "note", Body: "Left rear is worn to 2 mm"})
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.CorrectNote(ctx, pool, technician(), id, "Left FRONT is worn to 2 mm"); err != nil {
		t.Fatal(err)
	}
	notes := func() []workshop.Event {
		var out []workshop.Event
		es, _ := workshop.TimelineFor(ctx, pool, technician(), job)
		for _, e := range es {
			if e.Kind == "note" {
				out = append(out, e)
			}
		}
		return out
	}
	got := notes()
	if len(got) != 1 || got[0].Detail != "Left FRONT is worn to 2 mm" || !got[0].Corrected || len(got[0].Earlier) != 1 || got[0].Earlier[0].Body != "Left rear is worn to 2 mm" {
		t.Fatalf("after a correction: %+v", got)
	}
	// The old version cannot be corrected again: somebody already did.
	if err := workshop.CorrectNote(ctx, pool, advisor(), id, "Something else"); !errors.Is(err, workshop.ErrRecordChanged) {
		t.Errorf("correcting a version that was already corrected = %v, want ErrRecordChanged", err)
	}
	// The newest can be, by the counter, and the chain stays one note.
	if err := workshop.CorrectNote(ctx, pool, advisor(), got[0].NoteID, "Left front worn to 2 mm; right is fine"); err != nil {
		t.Fatal(err)
	}
	if got = notes(); len(got) != 1 || len(got[0].Earlier) != 2 {
		t.Errorf("after the second correction: %+v", got)
	}
	// A technician corrects their own and nobody else's, and not the counter's.
	other, _ := workshop.AddNote(ctx, pool, advisor(), job, workshop.NoteInput{Kind: "note", Body: "Counter's note"})
	if err := workshop.CorrectNote(ctx, pool, technician(), other, "Mine now"); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician corrected the counter's note: %v", err)
	}
	if err := workshop.CorrectNote(ctx, pool, partsDesk(), id, "x"); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the parts desk corrected a note: %v", err)
	}
}

func TestANoteIsNotAnApprovalAndAConversationIsTheCountersToWrite(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	job := newJob(t, pool)
	quote(t, pool, job, "Replace gearbox", 8000)
	move(t, pool, job, workshop.StateEstimated, workshop.StateAwaitingApproval)
	if _, err := workshop.AddNote(ctx, pool, advisor(), job, workshop.NoteInput{Kind: "conversation", Channel: "phone", Body: "Customer agreed to everything"}); err != nil {
		t.Fatal(err)
	}
	if err := workshop.SetState(ctx, pool, advisor(), job, workshop.StateApproved); err == nil {
		t.Fatal("a note that says the customer agreed approved the job")
	}
	if _, err := workshop.AddNote(ctx, pool, technician(), job, workshop.NoteInput{Kind: "conversation", Channel: "phone", Body: "x"}); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician recorded a conversation with the customer: %v", err)
	}
	for name, in := range map[string]workshop.NoteInput{
		"empty":               {Kind: "note", Body: "  "},
		"no channel":          {Kind: "conversation", Body: "x"},
		"future":              {Kind: "note", Body: "x", Occurred: time.Now().Add(time.Hour)},
		"long before the job": {Kind: "note", Body: "x", Occurred: time.Now().Add(-60 * 24 * time.Hour)},
	} {
		if _, err := workshop.AddNote(ctx, pool, advisor(), job, in); !errors.Is(err, workshop.ErrInvalid) {
			t.Errorf("%s = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := workshop.AddNote(ctx, pool, partsDesk(), job, workshop.NoteInput{Kind: "note", Body: "x"}); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the parts desk wrote a note: %v", err)
	}
}

func TestACallRecordedLaterSortsWhereItHappened(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := newJob(t, pool)
	move(t, pool, job, workshop.StateEstimated)
	time.Sleep(20 * time.Millisecond)
	when := time.Now().Add(-2 * time.Millisecond) // after the job was opened, before the move was
	_ = when
	// The call was made before the state change, written down after it.
	var opened time.Time
	if err := pool.QueryRow(ctx, `SELECT opened_at FROM work_orders WHERE id = $1`, job).Scan(&opened); err != nil {
		t.Fatal(err)
	}
	call := opened.Add(time.Millisecond * 5)
	if _, err := workshop.AddNote(ctx, pool, advisor(), job, workshop.NoteInput{Kind: "conversation", Channel: "phone", Body: "Rang about the noise", Occurred: call}); err != nil {
		t.Fatal(err)
	}
	es, _ := workshop.TimelineFor(ctx, pool, advisor(), job)
	var iCall, iMove = -1, -1
	for i, e := range es {
		switch e.Kind {
		case "conversation":
			iCall = i
		case "state":
			iMove = i
		}
	}
	if iCall < 0 || iMove < 0 || iCall > iMove {
		t.Fatalf("the call (%d) should come before the move (%d)", iCall, iMove)
	}
	if !es[iCall].RecordedAt.After(es[iCall].At) {
		t.Error("the entry does not know it was written down after it happened")
	}
}

func TestEventsWrittenAtTheSameTimeAreNeitherLostNorDoubled(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	addTechnician(t, pool)
	job := inProgress(t, pool, "Replace brake discs")
	if _, err := workshop.TasksFromLines(ctx, pool, advisor(), job); err != nil {
		t.Fatal(err)
	}
	task := tasksOf(t, pool, job)[0].ID
	const writers = 12
	var wg sync.WaitGroup
	errs := make(chan error, writers*2)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			who := technician()
			if i%2 == 1 {
				who = advisor()
			}
			if _, err := workshop.AddNote(ctx, pool, who, job, workshop.NoteInput{Kind: "note", Body: "Note number " + string(rune('A'+i))}); err != nil {
				errs <- err
			}
			if i%4 == 0 {
				if err := workshop.MoveTask(ctx, pool, who, task, workshop.TaskMove{To: workshop.TaskUnderway}); err != nil {
					errs <- err
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent write: %v", err)
	}
	for _, who := range []access.Scope{advisor(), technician()} {
		es, err := workshop.TimelineFor(ctx, pool, who, job)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]int{}
		notes := 0
		for _, e := range es {
			if e.Kind == "note" {
				notes++
				seen[e.Detail]++
			}
		}
		if notes != writers {
			t.Errorf("%d notes in the timeline, want %d", notes, writers)
		}
		for body, n := range seen {
			if n != 1 {
				t.Errorf("%q appears %d times", body, n)
			}
		}
		// And the same order every time it is read.
		again, _ := workshop.TimelineFor(ctx, pool, who, job)
		for i := range es {
			if es[i].Detail != again[i].Detail || es[i].Label != again[i].Label {
				t.Fatalf("the order changed between two reads at %d", i)
			}
		}
	}
}

func TestErasingACustomerBlanksWhatWasSaidAboutThem(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	job := newJob(t, pool)
	if _, err := workshop.AddNote(ctx, pool, advisor(), job, workshop.NoteInput{Kind: "conversation", Channel: "phone", Body: "Her mother is ill, be gentle"}); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.AddNote(ctx, pool, advisor(), job, workshop.NoteInput{Kind: "note", Body: "Brake pads on order"}); err != nil {
		t.Fatal(err)
	}
	c := cardOf(t, pool, fixtureCustomer)
	if err := workshop.ErasePerson(ctx, pool, advisor(), c.PersonID, "Asked"); err != nil {
		t.Fatal(err)
	}
	es, _ := workshop.TimelineFor(ctx, pool, advisor(), job)
	var conv, note string
	for _, e := range es {
		switch e.Kind {
		case "conversation":
			conv = e.Detail
		case "note":
			note = e.Detail
		}
	}
	if conv != workshop.ErasedName || note != "Brake pads on order" {
		t.Errorf("after erasure: conversation %q, note %q", conv, note)
	}
}
