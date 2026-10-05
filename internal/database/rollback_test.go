package database

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/RedaEkengren/FreeSMS/internal/testsupport"
)

// release builds a migration set: the base everybody has, plus whatever a
// given release added.
func release(extra map[string]string) fstest.MapFS {
	files := fstest.MapFS{
		"0001_base.sql": {Data: []byte("CREATE TABLE things (id int PRIMARY KEY);")},
	}
	for name, body := range extra {
		files[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return files
}

// The release before can start against the schema the release after left,
// when every migration it has not seen says so. That is the documented
// rollback: put the previous image back.
//
// It used to refuse every time: a version in the database with no file was
// read as a deleted migration, so the first deploy that added one made the
// previous image unable to start -- the moment it was needed.
func TestThePreviousReleaseStartsAfterASafeMigration(t *testing.T) {
	pool := testsupport.FreshPool(t)
	ctx := context.Background()
	previous := release(nil)
	next := release(map[string]string{
		"0024_add_a_note.sql": "-- rollback: safe\n-- A nullable column the old code never asks for.\nALTER TABLE things ADD COLUMN note text;",
	})

	if err := Migrate(ctx, pool, next); err != nil {
		t.Fatalf("the new release: %v", err)
	}
	res, err := MigrateWith(ctx, pool, previous)
	if err != nil {
		t.Fatalf("the previous release refused a schema marked safe for it: %v", err)
	}
	if len(res.Ahead) != 1 || res.Ahead[0] != 24 {
		t.Errorf("ahead = %v, want [24] reported so the log can say so", res.Ahead)
	}

	// And forward again: nothing to do, nothing refused.
	if err := Migrate(ctx, pool, next); err != nil {
		t.Errorf("rolling forward again: %v", err)
	}
}

// A migration the old code cannot run under says so, and the old release
// refuses with the two ways out rather than starting against it.
func TestThePreviousReleaseRefusesAMigrationThatNeedsARestore(t *testing.T) {
	pool := testsupport.FreshPool(t)
	ctx := context.Background()
	next := release(map[string]string{
		"0024_rename.sql": "-- rollback: restore\n-- The old code reads the old name.\nALTER TABLE things RENAME COLUMN id TO thing_id;",
	})
	if err := Migrate(ctx, pool, next); err != nil {
		t.Fatalf("the new release: %v", err)
	}
	err := Migrate(ctx, pool, release(nil))
	if err == nil {
		t.Fatal("the previous release started against a schema it cannot use")
	}
	for _, want := range []string{"0024", "roll forward", "restore"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// A version missing from inside a release's own range is still a deleted
// migration, and still refused. Rollback is not a way round the check.
func TestADeletedMigrationIsStillRefused(t *testing.T) {
	pool := testsupport.FreshPool(t)
	ctx := context.Background()
	full := release(map[string]string{
		"0024_one.sql": "-- rollback: safe\nSELECT 1;",
		"0025_two.sql": "-- rollback: safe\nSELECT 1;",
	})
	if err := Migrate(ctx, pool, full); err != nil {
		t.Fatal(err)
	}
	holed := release(map[string]string{"0025_two.sql": "-- rollback: safe\nSELECT 1;"})
	if err := Migrate(ctx, pool, holed); err == nil || !strings.Contains(err.Error(), "deleted") {
		t.Errorf("a release missing 0024 but carrying 0025 started: %v", err)
	}
}

// From 0024 on, a migration says which kind it is. Undeclared is refused by
// the loader, so it is found by a test and not at a two-in-the-morning
// rollback.
func TestANewMigrationMustDeclareItsRollback(t *testing.T) {
	if _, err := load(release(map[string]string{"0024_quiet.sql": "ALTER TABLE things ADD COLUMN x int;"})); err == nil ||
		!strings.Contains(err.Error(), "rollback") {
		t.Errorf("an undeclared 0024 loaded: %v", err)
	}
	if _, err := load(release(map[string]string{"0024_odd.sql": "-- rollback: maybe\nSELECT 1;"})); err == nil {
		t.Error("a rollback that is neither safe nor restore loaded")
	}
	// Before 0024 nothing declares, and nothing can: changing an applied
	// migration's text changes its checksum.
	if _, err := load(release(nil)); err != nil {
		t.Errorf("the base set without declarations: %v", err)
	}
}
