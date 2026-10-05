package config

import (
	"strings"
	"testing"
)

func valid(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/freesms")
	t.Setenv("BASE_URL", "http://localhost:8080")
	t.Setenv("SESSION_SECRET", "")
}

func TestLoadDefaults(t *testing.T) {
	valid(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want :8080", cfg.HTTPAddr)
	}
	if cfg.DefaultLocale != "en" {
		t.Errorf("DefaultLocale = %q, want en", cfg.DefaultLocale)
	}
}

// An operator under pressure should learn about every mistake in one restart,
// not one per restart.
func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("BASE_URL", "")
	t.Setenv("LOG_LEVEL", "chatty")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() = nil, want error")
	}
	for _, want := range []string{"DATABASE_URL", "BASE_URL", "LOG_LEVEL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s:\n%v", want, err)
		}
	}
}

// SESSION_SECRET was required and documented as the way to sign everybody
// out, and nothing read it. It is not required now, and an installation that
// still sets it starts and is told what does that job.
func TestSessionSecretIsNoLongerRequiredAndSaysSo(t *testing.T) {
	valid(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() without SESSION_SECRET = %v, want nil", err)
	}
	if len(cfg.Notices) != 0 {
		t.Errorf("notices without SESSION_SECRET: %v", cfg.Notices)
	}

	t.Setenv("SESSION_SECRET", "short")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load() with an old SESSION_SECRET = %v; an old .env must still start", err)
	}
	if len(cfg.Notices) != 1 || !strings.Contains(cfg.Notices[0], "-revoke-sessions") {
		t.Errorf("notices = %v, want one pointing at -revoke-sessions", cfg.Notices)
	}
}

func TestLoadRejectsBaseURLWithoutScheme(t *testing.T) {
	valid(t)
	t.Setenv("BASE_URL", "shop.example.com")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "BASE_URL") {
		t.Fatalf("Load() = %v, want a complaint about BASE_URL", err)
	}
}
