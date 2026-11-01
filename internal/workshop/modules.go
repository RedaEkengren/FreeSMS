package workshop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// inWorkshop is database.InScope for an operation of the workshop module: it
// refuses with access.ErrModuleOff, before a row is read, when the shop does
// not run the workshop (#143). Hiding the menu would be the mistake CLAUDE.md
// forbids for roles, made again for modules.
//
// Core operations in this package -- customers, vehicles, documents,
// payments, people -- use database.InScope and keep working whatever the shop
// runs, which is how a switched-off workshop's invoices stay readable.
func inWorkshop(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, fn func(context.Context, pgx.Tx) error) error {
	if err := scope.Require(access.Workshop); err != nil {
		return err
	}
	return database.InScope(ctx, pool, scope, fn)
}

// Available is the modules this release has something for, in the order
// they are offered. Car sales and rental are known to the schema and to
// access.Scope so that their work can start on a foundation; they are not
// offered at setup, or switched on, until there is a page behind them. A
// shop that chose "rental" and found nothing would be told a thing that is
// not so.
var Available = []access.Module{access.Workshop}

// available reports whether this release offers m.
func available(m access.Module) bool {
	for _, a := range Available {
		if a == m {
			return true
		}
	}
	return false
}

// OpenBusiness is what is still open in a module somebody is switching off:
// a job in the workshop, a booking still to come. Switching off with any of
// it is refused until the person says they mean it, having been shown what.
type OpenBusiness struct {
	Module access.Module
	Items  []OpenItem
}

// OpenItem is one thing still open.
type OpenItem struct {
	// "job" or "booking".
	Kind string
	// What a person recognises it by: a job number and plate, a booking's
	// time and what it is for.
	Number       int
	Registration string
	What         string
	At           *time.Time
	Link         string
}

func (e *OpenBusiness) Error() string {
	return fmt.Sprintf("%d things are still open in the %s module", len(e.Items), e.Module)
}

// ModuleState is one module and whether the shop runs it.
type ModuleState struct {
	Module access.Module
	On     bool
	// When it was last switched on or off, and by whom; nil and empty for a
	// module never touched, or switched on by the migration that brought
	// modules in.
	Since *time.Time
	By    string
}

// ModuleChange is one switching on or off, for the history on the shop page.
type ModuleChange struct {
	Module  access.Module
	Enabled bool
	By      string
	Reason  string
	At      time.Time
}

// The reasons FreeSMS writes itself, as catalogue keys. A person's own
// reason is their words and is shown as they wrote it.
const (
	reasonBeforeModules = "Every shop before modules was a workshop"
	reasonAtSetup       = "Chosen at setup"
	reasonUnrecorded    = "Run since before modules were recorded"
)

// Ours reports whether the reason is one FreeSMS wrote, which is translated,
// rather than a person's, which is not.
func (c ModuleChange) Ours() bool {
	switch c.Reason {
	case reasonBeforeModules, reasonAtSetup, reasonUnrecorded:
		return true
	}
	return false
}

// ModulesFor reads which modules the shop runs, every module this release
// offers listed whether on or not. Whoever runs the shop, who decides it.
func ModulesFor(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]ModuleState, error) {
	if !scope.Role.RunsTheShop() {
		return nil, access.ErrForbidden
	}
	on := map[access.Module]ModuleState{}
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT m.module, m.since, coalesce(p.display_name, '')
			  FROM shop_modules m
			  LEFT JOIN users u ON u.id = m.by_user
			  LEFT JOIN people p ON p.id = u.person_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var st ModuleState
			var since time.Time
			if err := rows.Scan(&st.Module, &since, &st.By); err != nil {
				return err
			}
			st.On, st.Since = true, &since
			on[st.Module] = st
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("read modules: %w", err)
	}
	var out []ModuleState
	for _, m := range access.Modules {
		st, isOn := on[m]
		if !isOn && !available(m) {
			continue
		}
		st.Module = m
		out = append(out, st)
	}
	return out, nil
}

// ModuleChanges lists every switching on and off, newest first.
func ModuleChanges(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]ModuleChange, error) {
	if !scope.Role.RunsTheShop() {
		return nil, access.ErrForbidden
	}
	var out []ModuleChange
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT c.module, c.enabled, coalesce(p.display_name, ''), c.reason, c.at
			  FROM shop_module_changes c
			  LEFT JOIN users u ON u.id = c.by_user
			  LEFT JOIN people p ON p.id = u.person_id
			 ORDER BY c.at DESC, c.id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c ModuleChange
			if err := rows.Scan(&c.Module, &c.Enabled, &c.By, &c.Reason, &c.At); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("read module changes: %w", err)
	}
	return out, nil
}

// SwitchModule switches a module on or off for the shop, and records who and
// why as a row of its own: the change is its own audit record, kept with the
// rest of the shop's and never written over (#151 reads it from there).
//
// Switching off with open business is refused with *OpenBusiness listing
// it, unless confirmed says the person has seen the list and means it. What
// the module wrote stays where it is, readable through Core; only new work
// is refused. Every module may be off: a shop that has stopped trading keeps
// its books, which it must for seven years, and reads them through Core.
//
// Only whoever runs the shop. Two owners switching at once are serialised on
// the shop's row, so each decides on what the other committed.
func SwitchModule(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, m access.Module, on bool, reason string, confirmed bool) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	reason = strings.TrimSpace(reason)
	switch {
	case !m.Valid():
		return fmt.Errorf("%w: there is no such module", ErrInvalid)
	case on && !available(m):
		return fmt.Errorf("%w: that module is not part of this release yet", ErrInvalid)
	case utf8.RuneCountInString(reason) > 500:
		return fmt.Errorf("%w: the reason is longer than 500 characters", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM shops WHERE id = $1 FOR UPDATE`, scope.ShopID); err != nil {
			return fmt.Errorf("lock the shop: %w", err)
		}
		var current []string
		if err := tx.QueryRow(ctx,
			`SELECT ARRAY(SELECT module FROM shop_modules WHERE shop_id = $1)`, scope.ShopID).Scan(&current); err != nil {
			return fmt.Errorf("read modules: %w", err)
		}
		isOn := false
		for _, c := range current {
			if access.Module(c) == m {
				isOn = true
			}
		}
		if isOn == on {
			// Already so. Nothing happened, so nothing is recorded.
			return nil
		}
		if !on {
			open, err := openBusinessTx(ctx, tx, m)
			if err != nil {
				return err
			}
			if len(open.Items) > 0 && !confirmed {
				return open
			}
		}
		// A shop that never recorded a choice runs the workshop by the view's
		// rule, not by a row. Its first change writes that row first, so the
		// rule stops applying without taking the workshop with it.
		if _, err := tx.Exec(ctx, `
			INSERT INTO shop_module_changes (shop_id, module, enabled, reason)
			SELECT $1, 'workshop', true, $2
			 WHERE NOT EXISTS (SELECT 1 FROM shop_module_changes WHERE shop_id = $1)`, scope.ShopID, reasonUnrecorded); err != nil {
			return fmt.Errorf("record the workshop as it was: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO shop_module_changes (shop_id, module, enabled, by_user, reason)
			VALUES ($1, $2, $3, NULLIF($4, '')::uuid, $5)`, scope.ShopID, string(m), on, scope.UserID, reason); err != nil {
			return fmt.Errorf("record the change: %w", err)
		}
		return nil
	})
}

// openBusinessTx lists what is open in a module. The workshop's: every job
// not yet invoiced, closed, declined or cancelled, and every booking still to
// come. Car sales and rental have none until they exist.
func openBusinessTx(ctx context.Context, tx pgx.Tx, m access.Module) (*OpenBusiness, error) {
	open := &OpenBusiness{Module: m}
	if m != access.Workshop {
		return open, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT 'job', w.number, coalesce(r.registration, ''), w.complaint, NULL::timestamptz, w.id::text
		  FROM work_orders w
		  LEFT JOIN vehicle_registrations r ON r.vehicle_id = w.vehicle_id AND r.valid_to IS NULL
		 WHERE w.state NOT IN ('invoiced', 'closed', 'declined', 'cancelled')
		UNION ALL
		SELECT 'booking', 0, coalesce(b.registration, ''), b.what, b.starts_at, b.id::text
		  FROM bookings b
		 WHERE b.status = 'booked' AND b.ends_at > now()
		 ORDER BY 1 DESC, 2, 5`)
	if err != nil {
		return nil, fmt.Errorf("read open business: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var it OpenItem
		var id string
		if err := rows.Scan(&it.Kind, &it.Number, &it.Registration, &it.What, &it.At, &id); err != nil {
			return nil, err
		}
		if it.Kind == "job" {
			it.Link = "/jobs/" + id
		} else {
			it.Link = "/bookings/" + id
		}
		open.Items = append(open.Items, it)
	}
	return open, rows.Err()
}

// AsOpenBusiness unwraps a refusal to switch a module off.
func AsOpenBusiness(err error) (*OpenBusiness, bool) {
	var open *OpenBusiness
	ok := errors.As(err, &open)
	return open, ok
}

// recordSetupModulesTx writes the modules chosen at setup, by the owner it
// created. Nothing chosen is Workshop alone: the default, and what every
// installation was before there was a choice.
func recordSetupModulesTx(ctx context.Context, tx pgx.Tx, shopID, ownerID string, modules []access.Module) error {
	if len(modules) == 0 {
		modules = []access.Module{access.Workshop}
	}
	seen := map[access.Module]bool{}
	for _, m := range modules {
		if !m.Valid() || !available(m) {
			return fmt.Errorf("%w: that module is not part of this release yet", ErrInvalid)
		}
		if seen[m] {
			continue
		}
		seen[m] = true
		if _, err := tx.Exec(ctx, `
			INSERT INTO shop_module_changes (shop_id, module, enabled, by_user, reason)
			VALUES ($1, $2, true, $3, $4)`, shopID, string(m), ownerID, reasonAtSetup); err != nil {
			return fmt.Errorf("record module %s: %w", m, err)
		}
	}
	return nil
}

// ShopRuns reports whether a shop runs a module, for a request with nobody
// signed in: the customer's links. Everything with a session reads the
// modules from its scope instead.
func ShopRuns(ctx context.Context, pool *pgxpool.Pool, shopID string, m access.Module) (bool, error) {
	if shopID == "" {
		return false, nil
	}
	var on bool
	err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT exists(SELECT 1 FROM shop_modules WHERE shop_id = $1 AND module = $2)`,
			shopID, string(m)).Scan(&on)
	})
	return on, err
}
