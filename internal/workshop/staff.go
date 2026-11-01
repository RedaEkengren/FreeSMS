package workshop

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"slices"
	"strings"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/auth"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// StaffMember is somebody who signs in.
type StaffMember struct {
	UserID string
	Name   string
	Email  string
	// Their main role, and any others they hold (#145).
	Role   access.Role
	Others []access.Role
	Active bool
	// Given the planning of people (rotas, absences).
	Planner bool
	// The person looking at the list. They cannot switch themselves off, so
	// the page does not offer it.
	You bool
}

// Roles is every role the person holds, main role first.
func (m StaffMember) Roles() access.Roles { return access.Of(m.Role, m.Others...) }

// Choices is what the roles form offers for this person: the roles the shop
// may give, and every role they already hold, in StaffRoles order. A role
// whose module has been switched off is kept by the switch, and SetStaffRoles
// rewrites what the form sends -- so a form that left it out would take it
// away the next time anything else was ticked, and a main role it did not
// list would fall back to the first option.
func (m StaffMember) Choices(offered []access.Role) []access.Role {
	held := m.Roles()
	var out []access.Role
	for _, r := range StaffRoles {
		if slices.Contains(offered, r) || held.Has(r) {
			out = append(out, r)
		}
	}
	return out
}

// StaffRoles are the roles somebody can be given, in the order a form offers
// them: the common ones first. RolesOffered is the ones a shop may give.
var StaffRoles = []access.Role{
	access.RoleTechnician, access.RoleServiceAdvisor, access.RoleParts,
	access.RoleSales, access.RoleBuyer, access.RoleRentalAgent,
	access.RoleOwner, access.RoleAdmin,
}

// RolesOffered is the roles this shop may give somebody: Core's, and those
// of the modules it runs. A rental desk in a shop that does not rent has
// nothing to do and would be a door left open for the day it does (#145).
func RolesOffered(scope access.Scope) []access.Role {
	var out []access.Role
	for _, r := range StaffRoles {
		if offered(scope, r) {
			out = append(out, r)
		}
	}
	return out
}

// offered reports whether the shop may give r. Read from the scope's modules
// rather than with scope.Uses, which the permission contract reads as a
// gate: people are Core's whatever the shop runs, and this only decides
// which roles there are to give.
func offered(scope access.Scope, r access.Role) bool {
	return r.Valid() && (r.Module() == "" || slices.Contains(scope.Modules, r.Module()))
}

// errRoleNotOffered is a role whose module the shop does not run.
var errRoleNotOffered = fmt.Errorf("%w: that role belongs to a part of FreeSMS this shop does not run", ErrInvalid)

// Staff lists everybody who signs in, for whoever runs the shop.
func Staff(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]StaffMember, error) {
	if !scope.Role.RunsTheShop() {
		return nil, access.ErrForbidden
	}
	var out []StaffMember
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT u.id, p.display_name, coalesce(p.email, ''), u.role, u.active, u.plans_staff,
			       ARRAY(SELECT r.role FROM user_roles r WHERE r.user_id = u.id AND r.role <> u.role ORDER BY r.role)
			FROM users u JOIN people p ON p.id = u.person_id
			ORDER BY NOT u.active, p.display_name`)
		if err != nil {
			return fmt.Errorf("list staff: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var m StaffMember
			var role string
			var others []string
			if err := rows.Scan(&m.UserID, &m.Name, &m.Email, &role, &m.Active, &m.Planner, &others); err != nil {
				return fmt.Errorf("scan staff: %w", err)
			}
			m.Role, m.You = access.Role(role), m.UserID == scope.UserID
			for _, o := range others {
				m.Others = append(m.Others, access.Role(o))
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

// AddStaff gives somebody a sign-in.
//
// Setup made the owner and nothing made anybody else, so a workshop had one
// sign-in -- and the technician's screens, built first, could not be reached
// by a technician. The password is the first one, typed by the owner and
// handed over; the person changes it on their own page.
func AddStaff(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, name, email string, role access.Role, password string) (string, error) {
	if !scope.Role.RunsTheShop() {
		return "", access.ErrForbidden
	}
	name, email = strings.TrimSpace(name), strings.TrimSpace(email)
	switch {
	case name == "":
		return "", fmt.Errorf("%w: a name, so colleagues know who it is", ErrInvalid)
	case !role.Valid():
		return "", fmt.Errorf("%w: %q is not a role", ErrInvalid, role)
	case !offered(scope, role):
		return "", errRoleNotOffered
	}
	if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email {
		return "", fmt.Errorf("%w: that does not look like an email address", ErrInvalid)
	}
	hash, err := auth.HashNew(ctx, password)
	if errors.Is(err, auth.ErrRefused) {
		return "", fmt.Errorf("%w: %s", ErrInvalid, strings.TrimPrefix(err.Error(), "auth: refused: "))
	}
	if err != nil {
		return "", err
	}

	var userID string
	err = database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var personID string
		if err := tx.QueryRow(ctx,
			`INSERT INTO people (shop_id, display_name, email) VALUES ($1, $2, $3) RETURNING id`,
			scope.ShopID, name, email).Scan(&personID); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`INSERT INTO users (shop_id, person_id, role, password_hash) VALUES ($1, $2, $3, $4) RETURNING id`,
			scope.ShopID, personID, string(role), hash).Scan(&userID)
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		// The email is how somebody signs in, so it belongs to one person.
		return "", fmt.Errorf("%w: %s already belongs to somebody in this workshop", ErrInvalid, email)
	}
	if err != nil {
		return "", fmt.Errorf("add staff: %w", err)
	}
	return userID, nil
}

// SetStaffRoles gives somebody their main role and any others (#145).
//
// The main role is the one the header names and their home page starts
// from; a capability is theirs if any role they hold has it. A role is given
// only if the shop runs its module, except one the person already holds:
// switching a module off does not take people's roles away, it only stops
// new work. Nobody changes their own roles -- an owner who took away their
// own would be locked out of this page -- and the shop is never left without
// an owner who can sign in. Takes effect on the person's next request, which
// reads their roles with the session.
func SetStaffRoles(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, userID string, main access.Role, others []access.Role) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(userID) {
		return ErrNotFound
	}
	if userID == scope.UserID {
		return fmt.Errorf("%w: you cannot change your own roles; another owner can", ErrInvalid)
	}
	want := access.Of(main, others...)
	if !want.Valid() {
		return fmt.Errorf("%w: those are not all roles", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var heldMain string
		var heldOthers []string
		err := tx.QueryRow(ctx, `
			SELECT u.role, ARRAY(SELECT r.role FROM user_roles r WHERE r.user_id = u.id)
			FROM users u WHERE u.id = $1 FOR UPDATE`, userID).Scan(&heldMain, &heldOthers)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read roles: %w", err)
		}
		held := access.Of(access.Role(heldMain))
		for _, r := range heldOthers {
			held = append(held, access.Role(r))
		}
		for _, r := range want {
			if !held.Has(r) && !offered(scope, r) {
				return errRoleNotOffered
			}
		}
		if held.Has(access.RoleOwner) && !want.Has(access.RoleOwner) {
			n, err := auth.OtherOwners(ctx, tx, userID)
			if err != nil {
				return err
			}
			if n == 0 {
				return fmt.Errorf("%w: that is the last owner who can sign in", ErrInvalid)
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET role = $2 WHERE id = $1`, userID, string(want.Main())); err != nil {
			return fmt.Errorf("set the main role: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM user_roles WHERE user_id = $1`, userID); err != nil {
			return fmt.Errorf("clear the other roles: %w", err)
		}
		for _, r := range want[1:] {
			if _, err := tx.Exec(ctx, `
				INSERT INTO user_roles (shop_id, user_id, role, granted_by) VALUES ($1, $2, $3, NULLIF($4, '')::uuid)`,
				scope.ShopID, userID, string(r), scope.UserID); err != nil {
				return fmt.Errorf("give a role: %w", err)
			}
		}
		return nil
	})
}

// SetStaffActive switches somebody off -- through auth.Deactivate, which ends
// their sessions and will not leave the shop without an owner -- or back on.
func SetStaffActive(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, userID string, active bool) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(userID) {
		return ErrNotFound
	}
	if !active {
		return staffError(auth.Deactivate(ctx, pool, scope, userID))
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE users SET active = true, deactivated_at = NULL WHERE id = $1`, userID)
		if err != nil {
			return fmt.Errorf("reactivate: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// SetStaffPassword gives somebody a new password and ends their sessions.
func SetStaffPassword(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, userID, password string) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(userID) {
		return ErrNotFound
	}
	return staffError(auth.SetPassword(ctx, pool, scope, userID, password))
}

// staffError says auth's refusals in this package's words.
func staffError(err error) error {
	switch {
	case errors.Is(err, auth.ErrRefused):
		return fmt.Errorf("%w: %s", ErrInvalid, strings.TrimPrefix(err.Error(), "auth: refused: "))
	case errors.Is(err, auth.ErrNoSuchUser):
		return ErrNotFound
	}
	return err
}
