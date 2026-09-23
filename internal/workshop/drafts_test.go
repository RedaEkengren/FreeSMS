package workshop_test

import (
	"context"
	"errors"
	"testing"

	"github.com/RedaEkengren/RedaSMS/internal/workshop"
)

// A flaky connection means the same request arrives twice. Without this each
// one opens a second job.
func TestARepeatedRequestIsRecognised(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	const key = "01234567-89ab-cdef-0123-456789abcdef"
	body := []byte("registration=ABC12D&complaint=Knocking")

	first, err := workshop.CheckIdempotency(ctx, pool, advisor(), key, body)
	if err != nil {
		t.Fatalf("CheckIdempotency: %v", err)
	}
	if first != nil {
		t.Fatal("a key nobody has used was treated as a repeat")
	}

	if err := workshop.RecordIdempotency(ctx, pool, advisor(), key, body, 303, "/jobs/abc"); err != nil {
		t.Fatalf("RecordIdempotency: %v", err)
	}

	again, err := workshop.CheckIdempotency(ctx, pool, advisor(), key, body)
	if err != nil {
		t.Fatalf("CheckIdempotency: %v", err)
	}
	if again == nil {
		t.Fatal("the repeat was not recognised")
	}
	if again.Status != 303 || again.Location != "/jobs/abc" {
		t.Errorf("the replay is %+v, want the first answer including where it went", again)
	}
}

// Answering the second question with the first answer would be worse than
// refusing.
func TestAKeyReusedForADifferentRequestIsRefused(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	const key = "01234567-89ab-cdef-0123-456789abcdef"

	if err := workshop.RecordIdempotency(ctx, pool, advisor(), key,
		[]byte("one thing"), 303, "/a"); err != nil {
		t.Fatalf("RecordIdempotency: %v", err)
	}
	if _, err := workshop.CheckIdempotency(ctx, pool, advisor(), key,
		[]byte("a different thing")); !errors.Is(err, workshop.ErrKeyReused) {
		t.Fatalf("CheckIdempotency = %v, want ErrKeyReused", err)
	}
}

// A request with no key behaves exactly as it always did.
func TestNoKeyMeansNoBookkeeping(t *testing.T) {
	pool := setup(t)
	got, err := workshop.CheckIdempotency(context.Background(), pool, advisor(), "", []byte("x"))
	if err != nil || got != nil {
		t.Errorf("CheckIdempotency with no key = %v, %v", got, err)
	}
}

func TestATooShortKeyIsRefused(t *testing.T) {
	pool := setup(t)
	if _, err := workshop.CheckIdempotency(context.Background(), pool, advisor(),
		"short", []byte("x")); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a five-character key = %v, want ErrInvalid", err)
	}
}

// A phone that is lost, wiped or swapped takes its local storage with it.
func TestADraftSurvivesOnTheServer(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	fields := map[string]string{
		"description": "Seized caliper bolt, drilled and re-tapped",
		"quantity":    "1,5",
	}
	if err := workshop.SaveDraft(ctx, pool, advisor(), "job-line:abc", fields); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	got, err := workshop.LoadDraft(ctx, pool, advisor(), "job-line:abc")
	if err != nil {
		t.Fatalf("LoadDraft: %v", err)
	}
	if got["description"] != fields["description"] || got["quantity"] != "1,5" {
		t.Errorf("the draft came back as %+v", got)
	}

	// Saving again replaces rather than accumulating.
	fields["quantity"] = "2"
	if err := workshop.SaveDraft(ctx, pool, advisor(), "job-line:abc", fields); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	got, _ = workshop.LoadDraft(ctx, pool, advisor(), "job-line:abc")
	if got["quantity"] != "2" {
		t.Errorf("quantity = %q, want the newer value", got["quantity"])
	}

	// Submitting is what discards it.
	if err := workshop.DiscardDraft(ctx, pool, advisor(), "job-line:abc"); err != nil {
		t.Fatalf("DiscardDraft: %v", err)
	}
	got, _ = workshop.LoadDraft(ctx, pool, advisor(), "job-line:abc")
	if len(got) != 0 {
		t.Errorf("the draft survived being submitted: %+v", got)
	}
}

// A second technician typing on the same form has their own draft.
func TestDraftsAreOnePerPersonPerForm(t *testing.T) {
	pool := setup(t)
	addTechnician(t, pool)
	ctx := context.Background()

	if err := workshop.SaveDraft(ctx, pool, advisor(), "intake", map[string]string{"registration": "AAA 111"}); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if err := workshop.SaveDraft(ctx, pool, technician(), "intake", map[string]string{"registration": "BBB 222"}); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	mine, _ := workshop.LoadDraft(ctx, pool, advisor(), "intake")
	theirs, _ := workshop.LoadDraft(ctx, pool, technician(), "intake")
	if mine["registration"] != "AAA 111" || theirs["registration"] != "BBB 222" {
		t.Errorf("drafts crossed over: %q and %q", mine["registration"], theirs["registration"])
	}
}

// They exist to survive a bad minute, not to be a record of what somebody was
// typing.
func TestDraftsAndKeysAreSweptUp(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	if err := workshop.RecordIdempotency(ctx, pool, advisor(),
		"01234567-89ab-cdef-0123-456789abcdef", []byte("x"), 303, ""); err != nil {
		t.Fatalf("RecordIdempotency: %v", err)
	}
	if err := workshop.SaveDraft(ctx, pool, advisor(), "intake", map[string]string{"a": "b"}); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	// Nothing is old enough yet, so a sweep leaves them alone.
	if _, err := workshop.Sweep(ctx, pool, shopID); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if got, _ := workshop.CheckIdempotency(ctx, pool, advisor(),
		"01234567-89ab-cdef-0123-456789abcdef", []byte("x")); got == nil {
		t.Error("a fresh idempotency key was swept away")
	}
	if got, _ := workshop.LoadDraft(ctx, pool, advisor(), "intake"); len(got) == 0 {
		t.Error("a fresh draft was swept away")
	}
}
