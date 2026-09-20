// Package server wires up HTTP handling.
//
// Pages are rendered on the server. The single most common complaint about
// every commercial system in this category is that it is slow, and a page that
// arrives as HTML is fast in a way that a page assembled in the browser has to
// work to match -- especially on a phone, on workshop wifi, which is the
// primary target here.
package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Server holds everything a handler may need.
type Server struct {
	pool    *pgxpool.Pool
	log     *slog.Logger
	release string
}

// New returns a Server. It does not listen; that is Run's job.
func New(pool *pgxpool.Pool, log *slog.Logger, release string) *Server {
	return &Server{pool: pool, log: log, release: release}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	return mux
}

// Run listens until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:    addr,
		Handler: s.routes(),

		// A server without these keeps a slow or dead client's connection
		// forever, and enough of them exhaust the process without anything
		// appearing to be wrong.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errs := make(chan error, 1)
	go func() {
		s.log.Info("listening", "addr", addr, "release", s.release)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errs <- err
		}
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	// Finish in-flight requests rather than cutting them off. A technician
	// mid-save during a deploy should not lose the save; see the autosave
	// issue for the other half of that promise.
	s.log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
