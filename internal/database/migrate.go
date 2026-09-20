package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// advisoryLockKey is an arbitrary but fixed number identifying the migration
// lock. Two instances starting at the same moment -- a rolling deploy, or a
// compose file that scales the service -- would otherwise both apply the same
// migration. Postgres advisory locks are held for the life of a session and
// released automatically if it dies, which is what makes them safe for a
// process that might be killed mid-run.
const advisoryLockKey int64 = 8_273_401_662

// migrationName matches "0003_add_work_orders.sql". The number is what orders
// them; the rest is for people reading the directory.
var migrationName = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

type migration struct {
	version  int64
	name     string
	body     string
	checksum string
}

// Migrate applies every migration that has not been applied yet, in order.
//
// It refuses to proceed rather than guessing in three situations: a migration
// file that does not parse, a version applied twice, and an applied migration
// whose content has changed since. The last one is the important one -- a
// migration edited after it ran means the database and the repository disagree
// about the schema, and every later assumption is built on that disagreement.
func Migrate(ctx context.Context, pool *pgxpool.Pool, files fs.FS) error {
	migrations, err := load(files)
	if err != nil {
		return err
	}

	// The lock must be taken on one connection and held, so acquire a
	// connection from the pool rather than using the pool directly.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, advisoryLockKey); err != nil {
		return fmt.Errorf("take migration lock: %w", err)
	}
	defer func() {
		// Best effort: if this fails the session is ending anyway, and
		// Postgres releases the lock when it does.
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, advisoryLockKey)
	}()

	// The tracking table is created here rather than in a migration, because
	// it is what records that migrations ran.
	const createTracking = `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    bigint      PRIMARY KEY,
			name       text        NOT NULL,
			checksum   text        NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`
	if _, err := conn.Exec(ctx, createTracking); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := appliedVersions(ctx, conn)
	if err != nil {
		return err
	}

	for _, m := range migrations {
		if prev, ok := applied[m.version]; ok {
			if prev != m.checksum {
				return fmt.Errorf(
					"migration %04d_%s.sql has changed since it was applied "+
						"(recorded %s, file %s); the database and this repository "+
						"disagree about the schema",
					m.version, m.name, short(prev), short(m.checksum))
			}
			continue
		}

		// One transaction per migration. A migration that fails leaves nothing
		// behind, so the next start sees the same work to do rather than a
		// half-applied schema.
		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin %04d: %w", m.version, err)
		}
		if _, err := tx.Exec(ctx, m.body); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply %04d_%s.sql: %w", m.version, m.name, err)
		}
		const record = `INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`
		if _, err := tx.Exec(ctx, record, m.version, m.name, m.checksum); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record %04d: %w", m.version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit %04d: %w", m.version, err)
		}
	}

	// A version in the database with no file left in the repository means a
	// migration was deleted after it ran. Refuse, for the same reason as a
	// changed checksum.
	known := make(map[int64]bool, len(migrations))
	for _, m := range migrations {
		known[m.version] = true
	}
	var orphans []string
	for v := range applied {
		if !known[v] {
			orphans = append(orphans, strconv.FormatInt(v, 10))
		}
	}
	if len(orphans) > 0 {
		sort.Strings(orphans)
		return fmt.Errorf(
			"versions %s are recorded as applied but have no migration file; "+
				"a migration was deleted after it ran", strings.Join(orphans, ", "))
	}

	return nil
}

// load reads and validates the migration files without touching a database, so
// that a malformed set is caught by a test rather than by a deploy.
func load(files fs.FS) ([]migration, error) {
	entries, err := fs.Glob(files, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}

	seen := make(map[int64]string)
	out := make([]migration, 0, len(entries))
	for _, name := range entries {
		match := migrationName.FindStringSubmatch(name)
		if match == nil {
			return nil, fmt.Errorf(
				"migration %q is not named NNNN_lower_snake_case.sql", name)
		}
		version, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("migration %q: %w", name, err)
		}
		if other, dup := seen[version]; dup {
			return nil, fmt.Errorf(
				"migrations %q and %q share version %04d; two branches merged "+
					"without renumbering", other, name, version)
		}
		seen[version] = name

		body, err := fs.ReadFile(files, name)
		if err != nil {
			return nil, fmt.Errorf("read %q: %w", name, err)
		}
		if strings.TrimSpace(string(body)) == "" {
			return nil, fmt.Errorf("migration %q is empty", name)
		}
		sum := sha256.Sum256(body)
		out = append(out, migration{
			version:  version,
			name:     match[2],
			body:     string(body),
			checksum: hex.EncodeToString(sum[:]),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func short(checksum string) string {
	if len(checksum) > 12 {
		return checksum[:12]
	}
	return checksum
}

// appliedVersions returns the checksum of every migration already recorded,
// keyed by version.
func appliedVersions(ctx context.Context, conn *pgxpool.Conn) (map[int64]string, error) {
	rows, err := conn.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[int64]string)
	for rows.Next() {
		var version int64
		var checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		applied[version] = checksum
	}
	return applied, rows.Err()
}
