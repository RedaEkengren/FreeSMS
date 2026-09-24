package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

func valid(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/freesms")
	t.Setenv("BASE_URL", "http://localhost:8080")
	t.Setenv("SESSION_SECRET", "0123456789abcdef0123456789abcdef")
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
	t.Setenv("SESSION_SECRET", "")
	t.Setenv("LOG_LEVEL", "chatty")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() = nil, want error")
	}
	for _, want := range []string{"DATABASE_URL", "BASE_URL", "SESSION_SECRET", "LOG_LEVEL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s:\n%v", want, err)
		}
	}
}

func TestLoadRejectsShortSecret(t *testing.T) {
	valid(t)
	t.Setenv("SESSION_SECRET", "short")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SESSION_SECRET") {
		t.Fatalf("Load() = %v, want a complaint about SESSION_SECRET", err)
	}
}

// A secret that is not base64 is a formatting mistake, not a security one.
func TestLoadAcceptsRawSecret(t *testing.T) {
	valid(t)
	raw := "this-is-not-base64-but-is-long-enough-anyway"
	t.Setenv("SESSION_SECRET", raw)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}
	if string(cfg.SessionSecret) != raw {
		t.Errorf("SessionSecret = %q, want the raw value", cfg.SessionSecret)
	}
}

// Thirty-two random hex characters are 32 bytes of key material, and are also
// legal base64 that decodes to 24. Decoding blindly would reject this with a
// complaint about a length the operator never typed.
func TestLoadAcceptsHexSecretThatIsAlsoValidBase64(t *testing.T) {
	valid(t)
	hexSecret := "0123456789abcdef0123456789abcdef"
	if _, err := base64.StdEncoding.DecodeString(hexSecret); err != nil {
		t.Fatalf("fixture is no longer valid base64, the case it guards is gone: %v", err)
	}
	t.Setenv("SESSION_SECRET", hexSecret)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}
	if string(cfg.SessionSecret) != hexSecret {
		t.Errorf("SessionSecret = %q, want the raw 32 characters", cfg.SessionSecret)
	}
}

func TestLoadRejectsBaseURLWithoutScheme(t *testing.T) {
	valid(t)
	t.Setenv("BASE_URL", "shop.example.com")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "BASE_URL") {
		t.Fatalf("Load() = %v, want a complaint about BASE_URL", err)
	}
}
