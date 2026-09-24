// Command freesms is the whole application: one binary, one database.
//
// Self-hosting is the point of this project, so the deployment story is "run
// this next to a Postgres". Anything that would make that harder needs a
// better reason than the convenience of the person adding it.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/config"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/server"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/RedaEkengren/FreeSMS/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
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
	if err := database.Migrate(ctx, pool, migrations.FS); err != nil {
		return err
	}
	log.Info("migrations up to date")

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
	go sweepPeriodically(ctx, pool, log, shopID)

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

// sweepPeriodically applies the data retention policy once a day.
//
// Once on startup as well, so that a service which is restarted more often
// than it is left running still does it, and so that a fresh deployment does
// not wait a day before its first sweep.
func sweepPeriodically(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger, shopID string) {
	const every = 24 * time.Hour

	run := func() {
		if shopID == "" {
			return
		}
		result, err := workshop.Sweep(ctx, pool, shopID)
		if err != nil {
			// Worth logging and not worth stopping for: a sweep that failed
			// today runs again tomorrow, and the data it would have removed is
			// not doing harm in the meantime.
			log.Error("retention sweep", "error", err)
			return
		}
		if !result.Empty() {
			log.Info("retention sweep",
				"login_attempts", result.LoginAttempts, "shares", result.Shares)
		}
	}

	run()
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}
