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

	// What the release before may do against the schema this leaves:
	// "safe" -- start and run, because the change is one it never notices --
	// or "restore" -- not start, because it would misread the schema. Empty
	// for the migrations from before this was asked.
	rollback string
}

// firstDeclaredRollback is the first version that must say its rollback.
// The ones before cannot: their text is checksummed as applied, and adding a
// line to them would read as a migration changed after it ran.
const firstDeclaredRollback = 24

// rollbackLine is how a migration says it: in its opening comments, so it is
// read with the change it describes.
var rollbackLine = regexp.MustCompile(`(?m)^--\s*rollback:\s*(\S+)\s*$`)

// Result is what Migrate found beyond what it did.
type Result struct {
	// Versions applied by a newer release than this one, all marked safe for
	// it to run under. Empty except after a rollback.
	Ahead []int64
}

// Migrate applies every migration that has not been applied yet, in order.
//
// It refuses to proceed rather than guessing in three situations: a migration
// file that does not parse, a version applied twice, and an applied migration
// whose content has changed since. The last one is the important one -- a
// migration edited after it ran means the database and the repository disagree
// about the schema, and every later assumption is built on that disagreement.
func Migrate(ctx context.Context, pool *pgxpool.Pool, files fs.FS) error {
	_, err := MigrateWith(ctx, pool, files)
	return err
}

// MigrateWith is Migrate, reporting what it found as well.
//
// A version recorded in the database that this release has no file for is
// one of two things. Inside this release's own range it is a migration that
// was deleted after it ran, and that is refused as it always was. Above it,
// it was applied by a newer release -- which is what a rollback to the
// previous image looks like -- and the release starts only if every such
// version declared, when it ran, that the release before may run under it.
// Otherwise it refuses, and says the two ways out: roll forward, or restore
// the dump taken before the deploy.
func MigrateWith(ctx context.Context, pool *pgxpool.Pool, files fs.FS) (Result, error) {
	var res Result
	migrations, err := load(files)
	if err != nil {
		return res, err
	}

	// The lock must be taken on one connection and held, so acquire a
	// connection from the pool rather than using the pool directly.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return res, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, advisoryLockKey); err != nil {
		return res, fmt.Errorf("take migration lock: %w", err)
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
		return res, fmt.Errorf("create schema_migrations: %w", err)
	}
	// Recorded with each migration, because the release that needs to read
	// it is the older one, which does not have the file.
	if _, err := conn.Exec(ctx, `ALTER TABLE schema_migrations ADD COLUMN IF NOT EXISTS rollback text`); err != nil {
		return res, fmt.Errorf("extend schema_migrations: %w", err)
	}

	applied, err := appliedVersions(ctx, conn)
	if err != nil {
		return res, err
	}

	for _, m := range migrations {
		if prev, ok := applied[m.version]; ok {
			if prev.checksum != m.checksum {
				return res, fmt.Errorf(
					"migration %04d_%s.sql has changed since it was applied "+
						"(recorded %s, file %s); the database and this repository "+
						"disagree about the schema",
					m.version, m.name, short(prev.checksum), short(m.checksum))
			}
			continue
		}

		// One transaction per migration. A migration that fails leaves nothing
		// behind, so the next start sees the same work to do rather than a
		// half-applied schema.
		tx, err := conn.Begin(ctx)
		if err != nil {
			return res, fmt.Errorf("begin %04d: %w", m.version, err)
		}
		if _, err := tx.Exec(ctx, m.body); err != nil {
			_ = tx.Rollback(ctx)
			return res, fmt.Errorf("apply %04d_%s.sql: %w", m.version, m.name, err)
		}
		const record = `INSERT INTO schema_migrations (version, name, checksum, rollback) VALUES ($1, $2, $3, nullif($4, ''))`
		if _, err := tx.Exec(ctx, record, m.version, m.name, m.checksum, m.rollback); err != nil {
			_ = tx.Rollback(ctx)
			return res, fmt.Errorf("record %04d: %w", m.version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return res, fmt.Errorf("commit %04d: %w", m.version, err)
		}
	}

	known := make(map[int64]bool, len(migrations))
	var highest int64
	for _, m := range migrations {
		known[m.version] = true
		if m.version > highest {
			highest = m.version
		}
	}
	var deleted, ahead, unsafe []int64
	for v, a := range applied {
		switch {
		case known[v]:
		case v < highest:
			deleted = append(deleted, v)
		default:
			ahead = append(ahead, v)
			if a.rollback != "safe" {
				unsafe = append(unsafe, v)
			}
		}
	}
	sortVersions(deleted)
	sortVersions(ahead)
	sortVersions(unsafe)

	// A version with no file inside this release's own range means a
	// migration was deleted after it ran. Refuse, for the same reason as a
	// changed checksum.
	if len(deleted) > 0 {
		return res, fmt.Errorf(
			"versions %s are recorded as applied but have no migration file; "+
				"a migration was deleted after it ran", versionList(deleted))
	}
	if len(unsafe) > 0 {
		return res, fmt.Errorf(
			"versions %s were applied by a newer release and are not marked safe for "+
				"this one to run under; roll forward to the release that applied them, or "+
				"restore the database dump taken before that deploy", versionList(unsafe))
	}
	res.Ahead = ahead

	return res, nil
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
		var rollback string
		if m := rollbackLine.FindStringSubmatch(string(body)); m != nil {
			rollback = m[1]
		}
		switch {
		case rollback != "" && rollback != "safe" && rollback != "restore":
			return nil, fmt.Errorf("migration %q: rollback %q is neither safe nor restore", name, rollback)
		case rollback == "" && version >= firstDeclaredRollback:
			return nil, fmt.Errorf(
				"migration %q does not say its rollback; start it with "+
					"\"-- rollback: safe\" if the release before can run against the "+
					"schema it leaves, or \"-- rollback: restore\" if it cannot (see DEPLOY.md)", name)
		}
		sum := sha256.Sum256(body)
		out = append(out, migration{
			version:  version,
			name:     match[2],
			body:     string(body),
			checksum: hex.EncodeToString(sum[:]),
			rollback: rollback,
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

type appliedMigration struct {
	checksum string
	rollback string
}

// appliedVersions returns what is recorded for every migration already
// applied, keyed by version.
func appliedVersions(ctx context.Context, conn *pgxpool.Conn) (map[int64]appliedMigration, error) {
	rows, err := conn.Query(ctx, `SELECT version, checksum, coalesce(rollback, '') FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[int64]appliedMigration)
	for rows.Next() {
		var version int64
		var a appliedMigration
		if err := rows.Scan(&version, &a.checksum, &a.rollback); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		applied[version] = a
	}
	return applied, rows.Err()
}

func sortVersions(v []int64) { sort.Slice(v, func(i, j int) bool { return v[i] < v[j] }) }

func versionList(v []int64) string {
	out := make([]string, len(v))
	for i, n := range v {
		out[i] = fmt.Sprintf("%04d", n)
	}
	return strings.Join(out, ", ")
}
