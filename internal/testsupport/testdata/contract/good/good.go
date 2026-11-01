// Package good keeps the contract: every operation checks its role and its
// module, or is listed with the reason.
package good

import "github.com/RedaEkengren/FreeSMS/internal/access"

// Read checks its role itself and its module through a helper.
func Read(scope access.Scope) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	return inRental(scope)
}

// Mine is open to every role and checks the module directly.
func Mine(scope access.Scope) error { return scope.Require(access.Rental) }

// Settings is Core.
func Settings(scope access.Scope) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	return nil
}

// Passes passes the scope on to an operation that checks.
func Passes(scope access.Scope) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	return Mine(scope)
}

func inRental(scope access.Scope) error { return scope.Require(access.Rental) }
