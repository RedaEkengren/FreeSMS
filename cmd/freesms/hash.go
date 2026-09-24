package main

import (
	"fmt"
	"os"

	"github.com/RedaEkengren/FreeSMS/internal/auth"
)

// hashPassword prints an argon2id hash for a password given on the command
// line.
//
// It exists because there is no other way to create the first user: the
// database stores a hash, and nothing can compute one but this binary. Without
// it, standing up a new installation means either shipping a default password
// -- which some installations would keep -- or writing a throwaway program.
func hashPassword(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: freesms -hash <password>")
	}
	hash, err := auth.HashPassword(args[0])
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, hash)
	return nil
}
