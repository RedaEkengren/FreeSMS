// Package testsupport holds the setup the integration tests share.
//
// It is imported only from _test files, so nothing here reaches a production
// binary.
package testsupport

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// EnvDatabaseURL names the variable that points at a database these tests may
// wipe. It must match what the CI workflow sets.
const EnvDatabaseURL = "REDASMS_TEST_DATABASE_URL"

// testLockKey serialises the packages that reset the schema.
const testLockKey int64 = 991_147_003

// FreshPool returns a pool against an empty schema, ready to be migrated.
//
// It stops short of applying the migrations so that this package does not
// import internal/database -- whose own tests use this helper, and would
// otherwise form an import cycle. The caller runs database.Migrate.
//
// Locally, an unset database URL skips the test: not everyone wants a
// container running to work on a parser.
//
// In CI it fails instead. Skipping there is a hole rather than a convenience:
// a typo in a workflow input would make the half of the suite that proves
// tenant isolation disappear, and the run would stay green. The evidence that
// these ran would be a step duration nobody reads.
func FreshPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv(EnvDatabaseURL)
	if url == "" {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s is not set, but this is CI: the integration tests would "+
				"skip silently and the run would still be green", EnvDatabaseURL)
		}
		t.Skipf("%s not set", EnvDatabaseURL)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	// Test packages run in parallel, and each of these resets the schema of
	// the one database it was given. Without this they drop it out from under
	// each other, which surfaces as "relation schema_migrations does not
	// exist" in whichever package lost the race.
	//
	// A Postgres advisory lock serialises them across processes, which a Go
	// mutex cannot do. It is held on its own connection and released before
	// the pool closes -- t.Cleanup runs in reverse, so this registration must
	// come after the pool's.
	lockConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire lock connection: %v", err)
	}
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_lock($1)`, testLockKey); err != nil {
		t.Fatalf("take test lock: %v", err)
	}
	t.Cleanup(func() {
		_, _ = lockConn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, testLockKey)
		lockConn.Release()
	})

	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	return pool
}
