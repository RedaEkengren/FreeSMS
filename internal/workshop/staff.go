package workshop

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
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
	Role   access.Role
	Active bool
	// Given the planning of people (rotas, absences).
	Planner bool
	// The person looking at the list. They cannot switch themselves off, so
	// the page does not offer it.
	You bool
}

// StaffRoles are the roles somebody can be given, in the order a form offers
// them: the common ones first.
var StaffRoles = []access.Role{
	access.RoleTechnician, access.RoleServiceAdvisor, access.RoleParts, access.RoleOwner, access.RoleAdmin,
}

// Staff lists everybody who signs in, for whoever runs the shop.
func Staff(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]StaffMember, error) {
	if !scope.Role.RunsTheShop() {
		return nil, access.ErrForbidden
	}
	var out []StaffMember
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT u.id, p.display_name, coalesce(p.email, ''), u.role, u.active, u.plans_staff
			FROM users u JOIN people p ON p.id = u.person_id
			ORDER BY NOT u.active, p.display_name`)
		if err != nil {
			return fmt.Errorf("list staff: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var m StaffMember
			var role string
			if err := rows.Scan(&m.UserID, &m.Name, &m.Email, &role, &m.Active, &m.Planner); err != nil {
				return fmt.Errorf("scan staff: %w", err)
			}
			m.Role, m.You = access.Role(role), m.UserID == scope.UserID
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
