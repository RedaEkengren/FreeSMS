package database

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestLoadOrdersByVersionNotFilename(t *testing.T) {
	files := fstest.MapFS{
		"0010_tenth.sql":  {Data: []byte("SELECT 10;")},
		"0002_second.sql": {Data: []byte("SELECT 2;")},
		"0001_first.sql":  {Data: []byte("SELECT 1;")},
	}
	got, err := load(files)
	if err != nil {
		t.Fatalf("load() = %v, want nil", err)
	}
	want := []int64{1, 2, 10}
	if len(got) != len(want) {
		t.Fatalf("load() returned %d migrations, want %d", len(got), len(want))
	}
	for i, v := range want {
		if got[i].version != v {
			t.Errorf("position %d is version %d, want %d", i, got[i].version, v)
		}
	}
}

// Two branches merged without renumbering is the realistic way this happens,
// and applying one of the two silently is the worst possible response.
func TestLoadRejectsDuplicateVersions(t *testing.T) {
	files := fstest.MapFS{
		"0001_one.sql": {Data: []byte("SELECT 1;")},
		"0001_two.sql": {Data: []byte("SELECT 2;")},
	}
	_, err := load(files)
	if err == nil || !strings.Contains(err.Error(), "share version") {
		t.Fatalf("load() = %v, want a duplicate version error", err)
	}
}

func TestLoadRejectsBadNames(t *testing.T) {
	for _, name := range []string{
		"1_short_number.sql",
		"0001-hyphenated.sql",
		"0001_CamelCase.sql",
		"init.sql",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := load(fstest.MapFS{name: {Data: []byte("SELECT 1;")}})
			if err == nil {
				t.Fatalf("load(%q) = nil, want an error", name)
			}
		})
	}
}

// An empty migration is almost always a file someone meant to fill in. Failing
// at start is cheaper than discovering the missing table later.
func TestLoadRejectsEmptyMigration(t *testing.T) {
	_, err := load(fstest.MapFS{"0001_empty.sql": {Data: []byte("   \n\t\n")}})
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("load() = %v, want an empty-migration error", err)
	}
}

func TestLoadChecksumChangesWithContent(t *testing.T) {
	first, err := load(fstest.MapFS{"0001_x.sql": {Data: []byte("SELECT 1;")}})
	if err != nil {
		t.Fatalf("load() = %v", err)
	}
	second, err := load(fstest.MapFS{"0001_x.sql": {Data: []byte("SELECT 2;")}})
	if err != nil {
		t.Fatalf("load() = %v", err)
	}
	if first[0].checksum == second[0].checksum {
		t.Error("checksum did not change with content; edited migrations would go unnoticed")
	}
}

func TestLoadIgnoresNonSQLFiles(t *testing.T) {
	files := fstest.MapFS{
		"0001_first.sql": {Data: []byte("SELECT 1;")},
		"README.md":      {Data: []byte("# notes")},
	}
	got, err := load(files)
	if err != nil {
		t.Fatalf("load() = %v, want nil", err)
	}
	if len(got) != 1 {
		t.Fatalf("load() returned %d migrations, want 1", len(got))
	}
}
