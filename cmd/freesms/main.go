// Command freesms is the whole application: one binary, one database.
//
// Self-hosting is the point of this project, so the deployment story is "run
// this next to a Postgres". Anything that would make that harder needs a
// better reason than the convenience of the person adding it.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/auth"
	"github.com/RedaEkengren/FreeSMS/internal/config"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/server"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/RedaEkengren/FreeSMS/migrations"
)

func main() {
	// A container health check runs the binary again with this flag, because
	// the runtime image has no shell to run anything else with.
	if len(os.Args) > 1 && os.Args[1] == "-hash" {
		if err := hashPassword(os.Args[2:]); err != nil {
			os.Stderr.WriteString("freesms: " + err.Error() + "\n")
			os.Exit(1)
		}
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		if err := healthcheck(); err != nil {
			os.Stderr.WriteString("freesms: unhealthy: " + err.Error() + "\n")
			os.Exit(1)
		}
		return
	}

	// Signs everybody out, in every shop. The operator's answer to a leaked
	// session or a lost device, run beside the service:
	//
	//   docker compose exec app /freesms -revoke-sessions
	//
	// The sessions are rows, so this survives a restart and needs none.
	if len(os.Args) > 1 && os.Args[1] == "-revoke-sessions" {
		if err := revokeSessions(); err != nil {
			os.Stderr.WriteString("freesms: " + err.Error() + "\n")
			os.Exit(1)
		}
		return
	}

	if err := run(); err != nil {
		// Configuration errors arrive as a list, so print rather than log:
		// slog would put the newlines inside a quoted field and make the list
		// unreadable at exactly the moment it needs to be read.
		os.Stderr.WriteString("freesms: " + err.Error() + "\n")
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel(cfg.LogLevel),
	}))

	// Cancelled on SIGINT or SIGTERM, which is what a container runtime sends
	// before it kills the process.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Migrations run before the server listens. Serving against a schema the
	// code does not expect produces errors that look like bugs, so failing
	// here is both cheaper and clearer.
	migrated, err := database.MigrateWith(ctx, pool, migrations.FS)
	if err != nil {
		return err
	}
	log.Info("migrations up to date")
	if len(migrated.Ahead) > 0 {
		// A rollback: an older release put back over a newer schema, which
		// every newer migration said it may run under. Worth saying, because
		// the next deploy of the newer release is the way back to normal.
		log.Warn("running under migrations from a newer release, all marked safe for this one",
			"versions", migrated.Ahead)
	}
	for _, n := range cfg.Notices {
		log.Warn(n)
	}

	// A fresh installation has no shop, and that is not a reason to refuse to
	// start. It used to be: `docker compose up` against an empty database
	// produced a container restarting in a loop and a reason buried in a log
	// nobody had thought to read yet. The server now serves a setup page
	// instead, and resolves the shop once there is one.
	shopID, err := workshop.ResolveShop(ctx, pool, cfg.ShopID)
	switch {
	case err != nil && cfg.ShopID != "":
		return err
	case err != nil:
		log.Warn("no shop configured yet; serving the setup page", "reason", err)
	default:
		log.Info("serving shop", "shop_id", shopID)
	}

	srv, err := server.New(pool, log, cfg, shopID)
	if err != nil {
		return err
	}

	// The retention policy is applied rather than written down and forgotten.
	// A policy that depends on somebody remembering to press something is not
	// a policy.
	go workshop.SweepPeriodically(ctx, pool, log, srv.Shop, srv.ShopSet(), 24*time.Hour)

	return srv.Run(ctx, cfg.HTTPAddr)
}

func logLevel(name string) slog.Level {
	switch name {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// revokeSessions ends every session and says how many.
func revokeSessions() error {
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
	ended, err := auth.RevokeAll(ctx, pool)
	if err != nil {
		return err
	}
	os.Stdout.WriteString(fmt.Sprintf("signed out %d session(s); everybody signs in again\n", ended))
	return nil
}
