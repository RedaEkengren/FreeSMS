package workshop_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
)

const testKey = "01234567-89ab-cdef-0123-456789abcdef"

func fp(path, body string) []byte {
	return workshop.Fingerprint("POST", path, userID, []byte(body))
}

// A flaky connection means the same request arrives twice. Without this each
// one opens a second job.
func TestARepeatedRequestIsRecognised(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	f := fp("/jobs/new", "registration=ABC12D&complaint=Knocking")

	first, err := workshop.ClaimIdempotency(ctx, pool, advisor(), testKey, f)
	if err != nil || first != nil {
		t.Fatalf("first claim = %v, %v; want ownership", first, err)
	}
	if err := workshop.CompleteIdempotency(ctx, pool, advisor(), testKey, 303, "/jobs/abc"); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	again, err := workshop.ClaimIdempotency(ctx, pool, advisor(), testKey, f)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if again == nil || again.Status != 303 || again.Location != "/jobs/abc" {
		t.Errorf("the replay is %+v, want the first answer including where it went", again)
	}
}

// Answering the second question with the first answer would be worse than
// refusing -- and a different question includes the same body sent somewhere
// else, or by somebody else.
func TestAKeyReusedForADifferentRequestIsRefused(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()

	if _, err := workshop.ClaimIdempotency(ctx, pool, advisor(), testKey, fp("/jobs/a/lines", "x")); err != nil {
		t.Fatalf("claim: %v", err)
	}
	for name, other := range map[string][]byte{
		"a different body":   fp("/jobs/a/lines", "y"),
		"a different path":   fp("/jobs/b/lines", "x"),
		"a different person": workshop.Fingerprint("POST", "/jobs/a/lines", "someone-else", []byte("x")),
		"a different method": workshop.Fingerprint("DELETE", "/jobs/a/lines", userID, []byte("x")),
	} {
		if _, err := workshop.ClaimIdempotency(ctx, pool, advisor(), testKey, other); !errors.Is(err, workshop.ErrKeyReused) {
			t.Errorf("%s: claim = %v, want ErrKeyReused", name, err)
		}
	}
}

// Two copies of the same request arriving together: exactly one may do the
// work. The other is told to ask again, not to do it too.
func TestTwoCopiesAtOnceDoTheWorkOnce(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	f := fp("/jobs/a/lines", "x")

	const n = 8
	var wg, ready sync.WaitGroup
	start := make(chan struct{})
	owners := make([]bool, n)
	errs := make([]error, n)
	ready.Add(n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ready.Done()
			<-start
			replay, err := workshop.ClaimIdempotency(ctx, pool, advisor(), testKey, f)
			owners[i], errs[i] = err == nil && replay == nil, err
		}(i)
	}
	ready.Wait()
	close(start)
	wg.Wait()

	var owned int
	for i := range owners {
		if owners[i] {
			owned++
		} else if !errors.Is(errs[i], workshop.ErrInFlight) {
			t.Errorf("a loser got %v, want ErrInFlight", errs[i])
		}
	}
	if owned != 1 {
		t.Errorf("%d of %d copies were told to do the work; exactly one must", owned, n)
	}
}

// A request that failed gives its key back, so trying again may succeed.
func TestAFailedRequestCanBeTriedAgain(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	f := fp("/jobs/a/lines", "x")

	if _, err := workshop.ClaimIdempotency(ctx, pool, advisor(), testKey, f); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := workshop.ReleaseIdempotency(ctx, pool, advisor(), testKey); err != nil {
		t.Fatalf("Release: %v", err)
	}
	replay, err := workshop.ClaimIdempotency(ctx, pool, advisor(), testKey, f)
	if err != nil || replay != nil {
		t.Errorf("after a release the retry = %v, %v; want ownership", replay, err)
	}
}

// A claim nobody finished, long ago. The process stopped between doing the
// work and writing down that it did, so nobody can say whether it happened.
// It is neither repeated nor replayed: a person has to look.
func TestAnAbandonedClaimIsNeitherRepeatedNorReplayed(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	f := fp("/jobs/a/invoice", "")

	if _, err := workshop.ClaimIdempotency(ctx, pool, advisor(), testKey, f); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE idempotency_keys SET created_at = now() - interval '1 hour' WHERE key = $1`, testKey)
		return err
	}); err != nil {
		t.Fatalf("age the claim: %v", err)
	}
	if _, err := workshop.ClaimIdempotency(ctx, pool, advisor(), testKey, f); !errors.Is(err, workshop.ErrOutcomeUnknown) {
		t.Errorf("an abandoned claim = %v, want ErrOutcomeUnknown", err)
	}
}

func TestATooShortKeyIsRefused(t *testing.T) {
	pool := setup(t)
	if _, err := workshop.ClaimIdempotency(context.Background(), pool, advisor(),
		"short", fp("/x", "x")); !errors.Is(err, workshop.ErrInvalid) {
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

	if _, err := workshop.ClaimIdempotency(ctx, pool, advisor(), testKey, fp("/x", "x")); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := workshop.CompleteIdempotency(ctx, pool, advisor(), testKey, 303, ""); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if err := workshop.SaveDraft(ctx, pool, advisor(), "intake", map[string]string{"a": "b"}); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	// Nothing is old enough yet, so a sweep leaves them alone.
	if _, err := workshop.Sweep(ctx, pool, shopID); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if got, _ := workshop.ClaimIdempotency(ctx, pool, advisor(), testKey, fp("/x", "x")); got == nil {
		t.Error("a fresh idempotency key was swept away")
	}
	if got, _ := workshop.LoadDraft(ctx, pool, advisor(), "intake"); len(got) == 0 {
		t.Error("a fresh draft was swept away")
	}
}
