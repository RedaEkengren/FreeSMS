package workshop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/i18n"
)

// The language a page is rendered in is the person's, then the shop's, then
// English. Both used to exist as columns that nothing ever wrote: setup
// created every shop in English and no form offered another, so the Swedish
// translation was reachable only by editing the database.

var (
	shipped     *i18n.Catalogues
	shippedOnce sync.Once
)

func catalogues() *i18n.Catalogues {
	shippedOnce.Do(func() {
		if c, err := i18n.Load("en", false); err == nil {
			shipped = c
		}
	})
	return shipped
}

// Language is one that shipped, named in itself: somebody looking for
// Swedish on an English screen is looking for "Svenska".
type Language struct {
	Locale string
	Name   string
}

// Languages lists the ones a shop or a person can choose.
func Languages() []Language {
	c := catalogues()
	if c == nil {
		return []Language{{Locale: "en", Name: "English"}}
	}
	var out []Language
	for _, cat := range c.Available() {
		name := cat.Name
		if name == "" {
			name = cat.Locale
		}
		out = append(out, Language{Locale: cat.Locale, Name: name})
	}
	return out
}

// checkLocale refuses a language that did not ship. Stored, one would fall
// back to English without a word, and look like a hole in the catalogue.
func checkLocale(locale string) error {
	if c := catalogues(); c == nil || !c.Has(locale) {
		return fmt.Errorf("%w: %q is not a language this installation has", ErrInvalid, locale)
	}
	return nil
}

// MyLocale is the language the caller chose for themselves, or "" when they
// follow the workshop's.
func MyLocale(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) (string, error) {
	var locale *string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT locale FROM users WHERE id = $1`, scope.UserID).Scan(&locale)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read own language: %w", err)
	}
	return deref(locale), nil
}

// SetMyLocale sets the caller's own language. "" means the workshop's and is
// stored as null, not as the workshop's language today -- otherwise a person
// who never chose would stop following the shop the day it changes.
func SetMyLocale(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, locale string) error {
	locale = strings.TrimSpace(locale)
	if locale != "" {
		if err := checkLocale(locale); err != nil {
			return err
		}
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE users SET locale = nullif($2, '') WHERE id = $1`, scope.UserID, locale)
		if err != nil {
			return fmt.Errorf("save own language: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return ErrNotFound
		}
		return nil
	})
}

// Themes are the choices a person has besides following their device.
var Themes = []string{"light", "dark"}

// SetMyTheme is the caller's own choice of light or dark; "" follows the
// device, and is stored as null so that it keeps following it.
func SetMyTheme(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, theme string) error {
	if theme != "" && theme != "light" && theme != "dark" {
		return fmt.Errorf("%w: light, dark, or the device's", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE users SET theme = nullif($2, '') WHERE id = $1`, scope.UserID, theme)
		if err != nil {
			return fmt.Errorf("save own theme: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return ErrNotFound
		}
		return nil
	})
}
