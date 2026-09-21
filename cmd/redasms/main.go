// Command redasms is the whole application: one binary, one database.
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

	"github.com/RedaEkengren/RedaSMS/internal/config"
	"github.com/RedaEkengren/RedaSMS/internal/database"
	"github.com/RedaEkengren/RedaSMS/internal/server"
	"github.com/RedaEkengren/RedaSMS/internal/workshop"
	"github.com/RedaEkengren/RedaSMS/migrations"
)

func main() {
	// A container health check runs the binary again with this flag, because
	// the runtime image has no shell to run anything else with.
	if len(os.Args) > 1 && os.Args[1] == "-hash" {
		if err := hashPassword(os.Args[2:]); err != nil {
			os.Stderr.WriteString("redasms: " + err.Error() + "\n")
			os.Exit(1)
		}
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		if err := healthcheck(); err != nil {
			os.Stderr.WriteString("redasms: unhealthy: " + err.Error() + "\n")
			os.Exit(1)
		}
		return
	}

	if err := run(); err != nil {
		// Configuration errors arrive as a list, so print rather than log:
		// slog would put the newlines inside a quoted field and make the list
		// unreadable at exactly the moment it needs to be read.
		os.Stderr.WriteString("redasms: " + err.Error() + "\n")
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

	shopID, err := workshop.ResolveShop(ctx, pool, cfg.ShopID)
	if err != nil {
		return err
	}
	log.Info("serving shop", "shop_id", shopID)

	srv, err := server.New(pool, log, cfg, shopID)
	if err != nil {
		return err
	}
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
