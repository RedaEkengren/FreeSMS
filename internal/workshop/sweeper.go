package workshop

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SweepPeriodically applies the retention policy now, every interval, and
// whenever wake fires, until ctx is cancelled.
//
// It asks shop which shop it is serving each time it runs, rather than being
// handed an identifier once. It used to be handed one at start-up, which on
// an empty installation was empty -- and stayed empty after /setup made the
// shop, so a newly set up workshop never had its retention applied until the
// process happened to restart. The server is the one place that knows which
// shop it serves; this asks it.
//
// wake is how the server says the shop has just been set, so the first sweep
// does not wait a day.
func SweepPeriodically(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger,
	shop func() string, wake <-chan struct{}, every time.Duration) {
	run := func() {
		id := shop()
		if id == "" {
			// Not set up yet. Nothing belongs to anybody, so nothing is due.
			return
		}
		result, err := Sweep(ctx, pool, id)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// Worth logging and not worth stopping for: a sweep that failed
			// today runs again tomorrow, and the data it would have removed is
			// not doing harm in the meantime.
			log.Error("retention sweep", "error", err)
			return
		}
		if result.Empty() {
			// At debug, so that "did it run" has an answer without a day of
			// noise at the default level.
			log.Debug("retention sweep found nothing due", "shop_id", id)
			return
		}
		log.Info("retention sweep",
			"login_attempts", result.LoginAttempts, "shares", result.Shares)
	}

	// Once at start as well, so that a service restarted more often than it
	// is left running still sweeps.
	run()
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
			run()
		case <-ticker.C:
			run()
		}
	}
}
