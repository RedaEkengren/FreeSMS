package auth

import (
	"context"
	"errors"
	"runtime"
	"time"
)

// Admit bounds how many password checks run at the same time.
//
// argon2id is expensive on purpose -- 64 MiB and a full core per check is what
// makes a stolen hash costly to guess -- and both places that run one are open
// to anybody who can reach the server: signing in, and setting up a fresh
// installation. Unbounded, a burst of anonymous requests asks for that memory
// and CPU as many times at once as somebody cares to send, and the workshop's
// own staff cannot sign in while it lasts.
//
// So a small number of checks run at once and the rest wait, briefly, for a
// turn. A request that cannot get one in time is told to try again rather
// than queued indefinitely, and one whose client has gone stops waiting.
//
// Callers take a slot before they open a database transaction, not inside
// one: waiting for a slot while holding a pooled connection would let the same
// burst exhaust the connection pool instead.
func Admit(ctx context.Context) (release func(), err error) {
	timer := time.NewTimer(admitWait)
	defer timer.Stop()
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-ctx.Done():
		return nil, ErrBusy
	case <-timer.C:
		return nil, ErrBusy
	}
}

// ErrBusy is returned when no slot became free in time. Not a credentials
// failure: nothing was judged, so it says so and invites a retry.
var ErrBusy = errors.New("auth: too many password checks at once; try again in a moment")

// admitWait is how long a request queues for a slot. Long enough that an
// ordinary morning rush of sign-ins never sees a refusal, short enough that a
// flood is shed rather than piled up.
const admitWait = 3 * time.Second

// slots is sized by memory as much as by cores: four checks at 64 MiB is a
// quarter of a gigabyte, which a small back-office machine can spare, and two
// is the floor so one slow check never blocks every other sign-in.
var slots = make(chan struct{}, concurrentChecks())

func concurrentChecks() int {
	n := runtime.NumCPU()
	if n > 4 {
		n = 4
	}
	if n < 2 {
		n = 2
	}
	return n
}
