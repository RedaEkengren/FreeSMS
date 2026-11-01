// Package nomaps checks everything and declares no maps, which is not enough:
// a package cannot opt out of the contract by saying nothing.
package nomaps

import "github.com/RedaEkengren/FreeSMS/internal/access"

// Checked checks both.
func Checked(scope access.Scope) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	return scope.Require(access.Dealer)
}
