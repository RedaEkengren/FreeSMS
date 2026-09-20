// Package database opens the connection pool and applies migrations.
//
// Queries elsewhere are plain SQL. There is no ORM: the schema is the most
// expensive thing in this project to get wrong, and it should be readable as
// SQL rather than inferred from struct tags.
package database

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Open returns a pool that has proved it can reach the database.
//
// It verifies rather than assuming, because pgxpool.New is lazy: it parses the
// URL and returns, so a completely unreachable database produces a healthy
// looking start and a failure on the first request instead.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
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

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("reach database: %w", err)
	}
	return pool, nil
}
