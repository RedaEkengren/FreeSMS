// Package unchecked has an operation that checks nothing, an entry that
// contradicts the code and one for an operation that is gone.
package unchecked

import "github.com/RedaEkengren/FreeSMS/internal/access"

// Leak takes a scope and checks neither.
func Leak(scope access.Scope) error { return nil }

// Both checks its role and module and is listed in both maps anyway.
func Both(scope access.Scope) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	return scope.Require(access.Dealer)
}
