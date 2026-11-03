// Package database opens the connection pool and applies migrations.
//
// Queries elsewhere are plain SQL. There is no ORM: the schema is the most
// expensive thing in this project to get wrong, and it should be readable as
// SQL rather than inferred from struct tags.
package database

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// StartupWait is how long Open keeps trying a database that is not up yet.
//
// A container started beside its database races it: the database can say it
// is ready and then restart once more while it finishes initialising, and an
// application that gives up on the first refused connection exits, and
// `docker compose up --wait` gives up with it. A minute covers a first
// initialisation on a slow disk; a database that is still not there after a
// minute is down, and saying so is better than waiting forever.
const StartupWait = time.Minute

// Open returns a pool that has proved it can reach the database.
//
// It verifies rather than assuming, because pgxpool.New is lazy: it parses the
// URL and returns, so a completely unreachable database produces a healthy
// looking start and a failure on the first request instead.
//
// A database that is not up yet -- nothing listening, or Postgres saying it
// is starting -- is tried again for up to StartupWait. Anything else, such as
// a wrong password or a database that does not exist, fails at once: waiting
// a minute would not change it.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	return openWithin(ctx, url, StartupWait)
}

func openWithin(ctx context.Context, url string, wait time.Duration) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}

	// Every timestamp in this system is timestamptz in UTC. The shop's local
	// time is a presentation concern, applied when rendering. Pinning the
	// session here means a server whose own timezone is wrong cannot quietly
	// change what a stored time means -- which matters most for technician
	// hours clocked across a daylight saving change.
	cfg.ConnConfig.RuntimeParams["timezone"] = "UTC"

	// Recycle connections rather than keeping them for the life of the
	// process. pgx caches type OIDs per connection, so a connection that was
	// open when a type was recreated -- by a restore, or by a migration that
	// drops and recreates an extension -- keeps the stale OID and fails every
	// query touching that type with "cache lookup failed for type N". An hour
	// bounds how long such a connection can stay poisoned.
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 15 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	deadline := time.Now().Add(wait)
	pause := 100 * time.Millisecond
	for {
		pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := pool.Ping(pingCtx)
		cancel()
		if err == nil {
			return pool, nil
		}
		if !notUpYet(err) || time.Now().Add(pause).After(deadline) {
			pool.Close()
			return nil, fmt.Errorf("reach database: %w", err)
		}
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, fmt.Errorf("reach database: %w", ctx.Err())
		case <-time.After(pause):
		}
		pause = min(pause*2, 2*time.Second)
	}
}

// notUpYet says whether an error is a database still starting rather than
// one that is wrongly configured.
func notUpYet(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		// cannot_connect_now: "the database system is starting up", or
		// shutting down during the initialisation's restart.
		return pgErr.Code == "57P03"
	}
	// Nothing listening, or a name the container network does not resolve
	// yet.
	var opErr *net.OpError
	var dnsErr *net.DNSError
	return errors.As(err, &opErr) || errors.As(err, &dnsErr)
}
