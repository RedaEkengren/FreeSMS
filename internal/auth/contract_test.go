package auth

// The permission contract for this package (#144), read by
// testsupport.CheckContracts from the source; see internal/testsupport/contract.go.

// everyRole is the operations any signed-in role may call, with the reason.
var everyRole = map[string]string{
	"ChangePassword": "the caller's own password, checked against the current one",
}

// core is the operations that belong to no module, with the reason: an
// account is a person's, whatever the shop runs.
var core = map[string]string{
	"ChangePassword": "a person's own account",
	"SetPassword":    "a person's account, set by whoever runs the shop",
	"Deactivate":     "a person's account, closed by whoever runs the shop",
}
