package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/RedaEkengren/FreeSMS/internal/auth"
	"github.com/RedaEkengren/FreeSMS/internal/config"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/ops"
	"github.com/RedaEkengren/FreeSMS/internal/storage"
)

// verify reads the database and the photographs it is pointed at and says
// whether they are a trustworthy workshop: an unbroken series of invoices,
// totals that are their lines, credits and reversals that point where they
// should, and every photograph on disk. It is what to run after a restore,
// beside the service:
//
//	docker compose exec app /freesms -verify
//
// and it exits non-zero when anything fails, so a script can rely on it.
func verify() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx := context.Background()
	pool, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	files, err := storage.New(cfg.AttachmentsDir)
	if err != nil {
		return fmt.Errorf("open the photographs: %w", err)
	}
	rep, err := ops.Verify(ctx, pool, files)
	if err != nil {
		return err
	}
	os.Stdout.WriteString(rep.Text())
	if rep.Failed() {
		return fmt.Errorf("the data did not verify")
	}
	return nil
}

// resetPassword gives a person a new password when nobody inside can: the
// owner who is locked out. The password comes from standard input, so it is
// never in the process list or the shell's history:
//
//	printf '%s' 'a long new phrase' | docker compose exec -T app /freesms -reset-password owner@example.se
func resetPassword(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: freesms -reset-password <email>   (the new password on standard input)")
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1024))
	if err != nil {
		return err
	}
	password := strings.TrimRight(string(raw), "\r\n")
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx := context.Background()
	pool, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	name, err := auth.ResetPasswordByEmail(ctx, pool, args[0], password)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "new password set for %s; their sessions are ended\n", name)
	return nil
}
