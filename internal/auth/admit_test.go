package auth

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fill takes every slot and returns a function that gives them back.
func fill(t *testing.T) func() {
	t.Helper()
	var releases []func()
	for i := 0; i < cap(slots); i++ {
		r, err := Admit(context.Background())
		if err != nil {
			t.Fatalf("taking slot %d of %d: %v", i+1, cap(slots), err)
		}
		releases = append(releases, r)
	}
	return func() {
		for _, r := range releases {
			r()
		}
	}
}

// However many anonymous requests arrive, only so many password checks run at
// once; the rest are told to try again rather than stacking up memory.
func TestPasswordChecksAreBounded(t *testing.T) {
	release := fill(t)
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := Admit(ctx); !errors.Is(err, ErrBusy) {
		t.Errorf("a check beyond the bound = %v, want ErrBusy", err)
	}
}

// A request whose client has gone stops waiting, and leaves nothing held.
func TestAnAbandonedWaitHoldsNoSlot(t *testing.T) {
	release := fill(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := Admit(ctx)
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrBusy) {
			t.Errorf("cancelled wait = %v, want ErrBusy", err)
		}
	case <-time.After(time.Second):
		t.Fatal("a cancelled request kept waiting for a slot")
	}

	release()
	if len(slots) != 0 {
		t.Errorf("%d slots still held after everything was released", len(slots))
	}
	// And every slot is usable again.
	again := fill(t)
	again()
}

// Login takes its slot before it touches the database, so a full house turns
// it away promptly instead of holding a pooled connection while it queues.
func TestLoginIsTurnedAwayWhenEveryCheckIsBusy(t *testing.T) {
	release := fill(t)
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	// No pool: if Login reached the database before asking for a slot, this
	// would panic rather than return ErrBusy.
	_, _, err := Login(ctx, nil, "", "a@b.test", "whatever", "")
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("Login with every slot taken = %v, want ErrBusy", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("Login waited %s for a slot; a client that gave up should be let go", time.Since(start))
	}
}
