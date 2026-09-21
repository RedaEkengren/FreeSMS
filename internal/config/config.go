// Package config reads the application's configuration from the environment.
//
// There is no config file and no flag that matters. One source means there is
// never a question about which value is in effect, and it is what container
// platforms hand us anyway.
package config

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Config is the whole of the application's configuration. Every field
// corresponds to a variable documented in .env.example; the two must not
// drift apart.
type Config struct {
	DatabaseURL    string
	HTTPAddr       string
	BaseURL        string
	SessionSecret  []byte
	AttachmentsDir string
	DefaultLocale  string
	LogLevel       string
	Release        string

	// Which shop this installation serves. Empty means: resolve it, and
	// require exactly one to exist.
	ShopID string
}

// minSecretBytes is the decoded length a session secret must reach. Below
// this the signing key is the weak link regardless of how it is used.
const minSecretBytes = 32

// Load reads the configuration and reports every problem it finds at once.
//
// Reporting all of them together is deliberate. A server that fails on the
// first missing variable, is restarted, and then fails on the second costs an
// operator one round trip per mistake, which is exactly the situation where
// they are already under pressure.
func Load() (*Config, error) {
	var problems []string

	cfg := &Config{
		DatabaseURL:    os.Getenv("DATABASE_URL"),
		HTTPAddr:       envOr("HTTP_ADDR", ":8080"),
		BaseURL:        os.Getenv("BASE_URL"),
		AttachmentsDir: envOr("ATTACHMENTS_DIR", "/var/lib/redasms/attachments"),
		DefaultLocale:  envOr("DEFAULT_LOCALE", "en"),
		LogLevel:       envOr("LOG_LEVEL", "info"),
		Release:        envOr("RELEASE", "dev"),
		ShopID:         os.Getenv("SHOP_ID"),
	}

	if cfg.DatabaseURL == "" {
		problems = append(problems, "DATABASE_URL is required")
	}

	// BASE_URL is worth validating rather than accepting. It ends up in links
	// sent to customers, so a wrong value produces approval links that fail
	// for the customer while everything looks correct from inside the shop.
	switch {
	case cfg.BaseURL == "":
		problems = append(problems, "BASE_URL is required")
	default:
		u, err := url.Parse(cfg.BaseURL)
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("BASE_URL is not a URL: %v", err))
		case u.Scheme != "http" && u.Scheme != "https":
			problems = append(problems, "BASE_URL must start with http:// or https://")
		case u.Host == "":
			problems = append(problems, "BASE_URL has no host")
		}
	}

	secret := os.Getenv("SESSION_SECRET")
	switch {
	case secret == "":
		problems = append(problems, "SESSION_SECRET is required (generate: openssl rand -base64 32)")
	default:
		// The secret may arrive base64-encoded or as raw characters, and the
		// two interpretations can both be valid for the same string. Thirty-two
		// random hex characters are 32 bytes of material and also happen to be
		// legal base64 that decodes to 24 -- so decoding blindly rejects a
		// perfectly good secret with a message about a length the operator
		// never typed. Take whichever reading yields more key material.
		key := []byte(secret)
		if decoded, err := base64.StdEncoding.DecodeString(secret); err == nil && len(decoded) > len(key) {
			key = decoded
		}
		if len(key) < minSecretBytes {
			problems = append(problems, fmt.Sprintf(
				"SESSION_SECRET gives %d bytes of key material, need at least %d "+
					"(generate: openssl rand -base64 32)", len(key), minSecretBytes))
		}
		cfg.SessionSecret = key
	}

	switch cfg.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		problems = append(problems, fmt.Sprintf(
			"LOG_LEVEL is %q, must be debug, info, warn or error", cfg.LogLevel))
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
