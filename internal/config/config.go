// Package config reads the application's configuration from the environment.
//
// There is no config file and no flag that matters. One source means there is
// never a question about which value is in effect, and it is what container
// platforms hand us anyway.
package config

import (
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
	AttachmentsDir string
	// Where a whole-shop export is written. It holds every customer the shop
	// has had, so the operator chooses where, and it is never served from
	// anywhere but the owner's own download.
	ExportDir     string
	DefaultLocale string
	LogLevel      string
	Release       string

	// Which shop this installation serves. Empty means: resolve it, and
	// require exactly one to exist.
	ShopID string

	// Where to look a registration up, if the shop has an arrangement with
	// anybody. Empty means no lookup, which is the default: no provider ships
	// with this project, because Swedish vehicle data comes under an agreement
	// that is the shop's to hold.
	VehicleLookupURL   string
	VehicleLookupToken string

	// Telling customers from the system. Empty providers are the default:
	// the front desk sends from its own phone and records that it did.
	EmailProvider   string // "", "smtp" or "lettermint"
	EmailFrom       string
	SMTPURL         string
	LettermintToken string
	LettermintURL   string
	SMSProvider     string // "" or "46elks"
	SMSFrom         string
	ElksUsername    string
	ElksPassword    string
	ElksURL         string
	// Part of the address 46elks reports delivery to, so nobody else can.
	SMSCallbackSecret string
	// When messages may go, "08-20", in the shop's zone.
	SendFrom, SendUntil int

	// Things worth telling the operator that are not reasons to refuse to
	// start, logged once the logger exists.
	Notices []string
}

// Load reads the configuration and reports every problem it finds at once.
//
// Reporting all of them together is deliberate. A server that fails on the
// first missing variable, is restarted, and then fails on the second costs an
// operator one round trip per mistake, which is exactly the situation where
// they are already under pressure.
func Load() (*Config, error) {
	var problems []string

	cfg := &Config{
		DatabaseURL:        os.Getenv("DATABASE_URL"),
		HTTPAddr:           envOr("HTTP_ADDR", ":8080"),
		BaseURL:            os.Getenv("BASE_URL"),
		AttachmentsDir:     envOr("ATTACHMENTS_DIR", "/var/lib/freesms/attachments"),
		ExportDir:          envOr("EXPORT_DIR", "/var/lib/freesms/exports"),
		DefaultLocale:      envOr("DEFAULT_LOCALE", "en"),
		LogLevel:           envOr("LOG_LEVEL", "info"),
		Release:            envOr("RELEASE", "dev"),
		ShopID:             os.Getenv("SHOP_ID"),
		VehicleLookupURL:   os.Getenv("VEHICLE_LOOKUP_URL"),
		VehicleLookupToken: os.Getenv("VEHICLE_LOOKUP_TOKEN"),

		EmailProvider:     os.Getenv("EMAIL_PROVIDER"),
		EmailFrom:         os.Getenv("EMAIL_FROM"),
		SMTPURL:           os.Getenv("SMTP_URL"),
		LettermintToken:   os.Getenv("LETTERMINT_TOKEN"),
		LettermintURL:     envOr("LETTERMINT_URL", "https://api.lettermint.co"),
		SMSProvider:       os.Getenv("SMS_PROVIDER"),
		SMSFrom:           os.Getenv("SMS_FROM"),
		ElksUsername:      os.Getenv("ELKS_USERNAME"),
		ElksPassword:      os.Getenv("ELKS_PASSWORD"),
		ElksURL:           envOr("ELKS_URL", "https://api.46elks.com"),
		SMSCallbackSecret: os.Getenv("SMS_CALLBACK_SECRET"),
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

	// Gone, and said so rather than silently ignored. It was required and
	// documented as the way to sign everybody out, and nothing ever read it.
	// An installation that still sets it starts normally and is told what
	// does the job instead.
	if os.Getenv("SESSION_SECRET") != "" {
		cfg.Notices = append(cfg.Notices,
			"SESSION_SECRET is no longer used and can be removed; to sign everybody out, run: freesms -revoke-sessions")
	}

	// A URL with nowhere to put the registration would silently look up the
	// same thing every time.
	if cfg.VehicleLookupURL != "" && !strings.Contains(cfg.VehicleLookupURL, "{registration}") {
		problems = append(problems,
			"VEHICLE_LOOKUP_URL must contain {registration} where the plate goes")
	}

	// A provider named without what it needs fails at start, not at the
	// first customer. The messages name variables, never their values.
	switch cfg.EmailProvider {
	case "":
	case "smtp", "lettermint":
		if cfg.EmailFrom == "" {
			problems = append(problems, "EMAIL_FROM is required with EMAIL_PROVIDER, as \"Verkstaden <hej@verkstad.se>\"")
		}
		if cfg.EmailProvider == "smtp" && !strings.HasPrefix(cfg.SMTPURL, "smtp://") {
			problems = append(problems, "SMTP_URL is required with EMAIL_PROVIDER=smtp, as smtp://user:password@host:port")
		}
		if cfg.EmailProvider == "lettermint" && cfg.LettermintToken == "" {
			problems = append(problems, "LETTERMINT_TOKEN is required with EMAIL_PROVIDER=lettermint")
		}
	default:
		problems = append(problems, fmt.Sprintf("EMAIL_PROVIDER is %q, must be empty, smtp or lettermint", cfg.EmailProvider))
	}
	switch cfg.SMSProvider {
	case "":
	case "46elks":
		if cfg.ElksUsername == "" || cfg.ElksPassword == "" {
			problems = append(problems, "ELKS_USERNAME and ELKS_PASSWORD are required with SMS_PROVIDER=46elks")
		}
		if !smsSender(cfg.SMSFrom) {
			problems = append(problems, "SMS_FROM is required with SMS_PROVIDER: up to 11 letters and digits, or a number as +46...")
		}
	default:
		problems = append(problems, fmt.Sprintf("SMS_PROVIDER is %q, must be empty or 46elks", cfg.SMSProvider))
	}

	if _, err := fmt.Sscanf(envOr("SEND_HOURS", "08-20"), "%d-%d", &cfg.SendFrom, &cfg.SendUntil); err != nil ||
		cfg.SendFrom < 0 || cfg.SendUntil > 24 || cfg.SendFrom >= cfg.SendUntil {
		problems = append(problems, "SEND_HOURS must be two hours, from before until, as 08-20")
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

// smsSender is a text's sender as a network takes it: a name of up to eleven
// letters and digits, starting with a letter, or a number. A name cannot be
// replied to; a number can.
func smsSender(s string) bool {
	if strings.HasPrefix(s, "+") {
		return len(s) >= 9 && len(s) <= 16 && strings.Trim(s[1:], "0123456789") == ""
	}
	if s == "" || len(s) > 11 || !(s[0] >= 'A' && s[0] <= 'Z' || s[0] >= 'a' && s[0] <= 'z') {
		return false
	}
	for _, r := range s {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}
