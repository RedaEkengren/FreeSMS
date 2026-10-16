// Package testsupport holds the setup the integration tests share.
//
// It is imported only from _test files, so nothing here reaches a production
// binary.
package testsupport

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EnvDatabaseURL names the variable that points at a database these tests may
// wipe. It must match what the CI workflow sets.
const EnvDatabaseURL = "FREESMS_TEST_DATABASE_URL"

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
	return FreshPoolWith(t, nil)
}

// FreshPoolWith is FreshPool with a tracer on every connection, for a test
// that counts what reaches the database.
func FreshPoolWith(t *testing.T, tracer pgx.QueryTracer) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv(EnvDatabaseURL)

	// This wipes the schema, so it must never be pointed at a database a
	// running instance is using. Doing so destroys that instance's data and,
	// worse, silently poisons its connection pool: pgx caches type OIDs per
	// connection, and recreating the schema gives citext a new one. Every
	// query touching a citext column then fails with "cache lookup failed for
	// type N", which reads as corruption. docker-compose.yml creates
	// freesms_test for exactly this.
	if strings.HasSuffix(url, "/freesms?sslmode=disable") || strings.HasSuffix(url, "/freesms") {
		t.Fatalf("%s points at the development database; use freesms_test", EnvDatabaseURL)
	}

	if url == "" {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s is not set, but this is CI: the integration tests would "+
				"skip silently and the run would still be green", EnvDatabaseURL)
		}
		t.Skipf("%s not set", EnvDatabaseURL)
	}

	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse %s: %v", EnvDatabaseURL, err)
	}
	if tracer != nil {
		cfg.ConnConfig.Tracer = tracer
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
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

var (
	templateOnce sync.Once
	templateName string
	templateErr  error
	cloneSeq     atomic.Int64
)

// ClonedPool returns a pool on a database of its own, copied from a template
// that has been migrated once for the whole test process.
//
// FreshPool resets the schema and leaves the caller to migrate, which for a
// package with hundreds of tests means applying every migration hundreds of
// times: the workshop package spent nearly all of its ten minutes doing it.
// Postgres can copy a finished database in a fraction of that.
//
// A database per test also needs no lock against other packages, which is
// what FreshPool's advisory lock was for. The database is dropped when the
// test ends.
func ClonedPool(t *testing.T, migrate func(context.Context, *pgxpool.Pool) error) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv(EnvDatabaseURL)
	if url == "" {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s is not set, but this is CI: the integration tests would "+
				"skip silently and the run would still be green", EnvDatabaseURL)
		}
		t.Skipf("%s not set", EnvDatabaseURL)
	}
	if strings.HasSuffix(url, "/freesms?sslmode=disable") || strings.HasSuffix(url, "/freesms") {
		t.Fatalf("%s points at the development database; use freesms_test", EnvDatabaseURL)
	}
	ctx := context.Background()
	base, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse %s: %v", EnvDatabaseURL, err)
	}

	templateOnce.Do(func() {
		templateName = fmt.Sprintf("freesms_tpl_%d", os.Getpid())
		templateErr = buildTemplate(ctx, base, templateName, migrate)
	})
	if templateErr != nil {
		t.Fatalf("build the template database: %v", templateErr)
	}

	name := fmt.Sprintf("freesms_t_%d_%d", os.Getpid(), cloneSeq.Add(1))
	admin, err := pgx.ConnectConfig(ctx, base.ConnConfig)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	_, err = admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s TEMPLATE %s`, name, templateName))
	admin.Close(ctx)
	if err != nil {
		t.Fatalf("clone the template: %v", err)
	}

	cfg := base.Copy()
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect to the clone: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		if c, err := pgx.ConnectConfig(context.Background(), base.ConnConfig); err == nil {
			_, _ = c.Exec(context.Background(), fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, name))
			c.Close(context.Background())
		}
	})
	return pool
}

func buildTemplate(ctx context.Context, base *pgxpool.Config, name string, migrate func(context.Context, *pgxpool.Pool) error) error {
	admin, err := pgx.ConnectConfig(ctx, base.ConnConfig)
	if err != nil {
		return err
	}
	defer admin.Close(ctx)
	dropAbandoned(ctx, admin)
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s`, name)); err != nil {
		return err
	}
	cfg := base.Copy()
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return err
	}
	// Closed before anything is cloned from it: Postgres refuses to copy a
	// database somebody is connected to.
	defer pool.Close()
	return migrate(ctx, pool)
}

// dropAbandoned removes the templates and clones of test processes that are
// no longer running: a run that was killed never reached its cleanup.
func dropAbandoned(ctx context.Context, admin *pgx.Conn) {
	rows, err := admin.Query(ctx, `SELECT datname FROM pg_database WHERE datname ~ '^freesms_(tpl|t)_[0-9]+'`)
	if err != nil {
		return
	}
	var names []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			names = append(names, n)
		}
	}
	rows.Close()
	for _, n := range names {
		var pid int
		if _, err := fmt.Sscanf(strings.TrimPrefix(strings.TrimPrefix(n, "freesms_tpl_"), "freesms_t_"), "%d", &pid); err != nil {
			continue
		}
		if p, err := os.FindProcess(pid); err == nil && p.Signal(syscall.Signal(0)) == nil {
			continue
		}
		_, _ = admin.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, n))
	}
}
