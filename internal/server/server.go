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
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/RedaEkengren/RedaSMS/internal/auth"
	"github.com/RedaEkengren/RedaSMS/internal/config"
	"github.com/RedaEkengren/RedaSMS/internal/i18n"
	"github.com/RedaEkengren/RedaSMS/internal/storage"
	"github.com/RedaEkengren/RedaSMS/internal/vehicledata"
	"github.com/RedaEkengren/RedaSMS/internal/web"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Server holds everything a handler may need.
type Server struct {
	pool          *pgxpool.Pool
	log           *slog.Logger
	release       string
	locale        string
	secureCookies bool
	templates     map[string]*template.Template
	photos        *storage.Store
	baseURL       string
	vehicleLookup vehicledata.Lookup
	catalogues    *i18n.Catalogues

	// Pinned by SHOP_ID when an installation serves a named shop.
	configuredShopID string

	// Resolved once there is a shop. It starts empty on a fresh installation
	// and is filled in by setup, while the process is running -- hence the
	// mutex rather than a plain field read at boot.
	mu       sync.RWMutex
	resolved string
}

// newLookup returns whatever the configuration asks for, or nothing.
//
// Nothing is the default and is not a degraded mode: a shop without an
// arrangement types the make and model, which is what it does today on paper.
func newLookup(cfg *config.Config) vehicledata.Lookup {
	if cfg.VehicleLookupURL == "" {
		return vehicledata.None{}
	}
	return vehicledata.HTTPLookup{
		URL:    cfg.VehicleLookupURL,
		Token:  cfg.VehicleLookupToken,
		Fields: vehicledata.DefaultFields(),
	}
}

// localeFor resolves the language for a request: the person's, then the
// shop's, then the fallback.
func (s *Server) localeFor(session auth.Session) string {
	if session.Locale != "" {
		return session.Locale
	}
	return s.locale
}

func (s *Server) shop() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.resolved
}

func (s *Server) setShop(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolved = id
}

// New returns a Server. It does not listen; that is Run's job.
func New(pool *pgxpool.Pool, log *slog.Logger, cfg *config.Config, shopID string) (*Server, error) {
	templates, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	photos, err := storage.New(cfg.AttachmentsDir)
	if err != nil {
		return nil, err
	}
	// Strict outside production: a string nobody has translated should be
	// obvious to whoever is looking at the screen and invisible to a workshop.
	catalogues, err := i18n.Load("en", cfg.Release == "dev")
	if err != nil {
		return nil, err
	}
	return &Server{
		pool:             pool,
		log:              log,
		release:          cfg.Release,
		resolved:         shopID,
		configuredShopID: cfg.ShopID,
		locale:           cfg.DefaultLocale,
		photos:           photos,
		vehicleLookup:    newLookup(cfg),
		catalogues:       catalogues,
		baseURL:          strings.TrimRight(cfg.BaseURL, "/"),
		templates:        templates,
		// A Secure cookie is not sent over plain HTTP, so setting it
		// unconditionally would break every local and reverse-proxied
		// installation that terminates TLS elsewhere. BASE_URL is what the
		// operator says the service is reached as, so it is the honest source.
		secureCookies: strings.HasPrefix(cfg.BaseURL, "https://"),
	}, nil
}

func (s *Server) routes() (http.Handler, error) {
	mux := http.NewServeMux()

	// Health is outside the session middleware on purpose: a health check must
	// not depend on authentication working.
	mux.HandleFunc("GET /healthz", s.handleHealth)

	staticFS, err := fs.Sub(web.Static, "static")
	if err != nil {
		return nil, fmt.Errorf("static files: %w", err)
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/",
		cacheForever(http.FileServer(http.FS(staticFS)))))

	mux.HandleFunc("GET /setup", s.handleSetupForm)
	mux.HandleFunc("POST /setup", s.handleSetup)

	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.handleLogout)

	mux.HandleFunc("GET /{$}", s.requireSession(s.handleHome))
	mux.HandleFunc("GET /board", s.requireSession(s.handleBoard))
	mux.HandleFunc("GET /jobs/new", s.requireSession(s.handleNewJobForm))
	mux.HandleFunc("POST /jobs/new", s.requireSession(s.handleNewJob))
	mux.HandleFunc("GET /jobs/new/lookup", s.requireSession(s.handleVehicleLookup))
	mux.HandleFunc("GET /jobs/{id}", s.requireSession(s.handleJob))
	mux.HandleFunc("POST /jobs/{id}/state", s.requireSession(s.handleSetState))
	mux.HandleFunc("POST /jobs/{id}/lines", s.requireSession(s.handleAddLine))
	mux.HandleFunc("POST /jobs/{id}/invoice", s.requireSession(s.handleInvoice))
	mux.HandleFunc("POST /jobs/{id}/credit", s.requireSession(s.handleCreditNote))

	mux.HandleFunc("POST /jobs/{id}/inspect", s.requireSession(s.handleStartInspection))
	mux.HandleFunc("GET /inspections/{id}", s.requireSession(s.handleInspection))
	mux.HandleFunc("POST /inspections/{id}/items/{itemID}", s.requireSession(s.handleSetInspectionItem))
	mux.HandleFunc("POST /inspections/{id}/items/{itemID}/photo", s.requireSession(s.handleUploadPhoto))
	mux.HandleFunc("POST /inspections/{id}/complete", s.requireSession(s.handleCompleteInspection))
	mux.HandleFunc("POST /inspections/{id}/share", s.requireSession(s.handleShareInspection))
	mux.HandleFunc("POST /inspections/{id}/revoke", s.requireSession(s.handleRevokeShare))
	mux.HandleFunc("GET /photos/{key}", s.requireSession(s.handlePhoto))

	// The customer's link. No session; the token is the whole of the
	// authorisation, and it authorises exactly one inspection.
	mux.HandleFunc("GET /i/{token}", s.handleSharedInspection)
	mux.HandleFunc("POST /i/{token}/items/{itemID}", s.handleSharedDecision)
	mux.HandleFunc("GET /i/{token}/photos/{key}", s.handleSharedPhoto)

	mux.HandleFunc("GET /figures", s.requireSession(s.handleDashboard))
	mux.HandleFunc("GET /accounting", s.requireSession(s.handleAccounting))
	mux.HandleFunc("POST /accounting/export", s.requireSession(s.handleAccountingExport))
	mux.HandleFunc("GET /privacy", s.requireSession(s.handlePrivacy))
	mux.HandleFunc("GET /people/{id}/export", s.requireSession(s.handleExportPerson))
	mux.HandleFunc("POST /people/{id}/erase", s.requireSession(s.handleErasePerson))

	mux.HandleFunc("GET /labour", s.requireSession(s.handleLabour))
	mux.HandleFunc("POST /labour", s.requireSession(s.handleSaveLabour))
	mux.HandleFunc("POST /labour/rate", s.requireSession(s.handleSetLabourRate))

	mux.HandleFunc("GET /time", s.requireSession(s.handleTime))
	mux.HandleFunc("POST /time/correct", s.requireSession(s.handleCorrectTime))

	mux.HandleFunc("GET /search", s.requireSession(s.handleSearch))
	mux.HandleFunc("GET /vehicles/{id}", s.requireSession(s.handleVehicle))

	mux.HandleFunc("GET /scan", s.requireSession(s.handleScan))
	mux.HandleFunc("GET /labels", s.requireSession(s.handleLabels))
	mux.HandleFunc("GET /stock", s.requireSession(s.handleStock))
	mux.HandleFunc("POST /stock/move", s.requireSession(s.handleStockMove))
	mux.HandleFunc("POST /stock/bands", s.requireSession(s.handleSavePriceBand))

	mux.HandleFunc("GET /parts", s.requireSession(s.handleParts))
	mux.HandleFunc("POST /parts/arrived", s.requireSession(s.handlePartArrived))
	mux.HandleFunc("POST /jobs/{id}/parts", s.requireSession(s.handleRequestPart))
	mux.HandleFunc("POST /jobs/{id}/findings", s.requireSession(s.handleReportFinding))
	mux.HandleFunc("POST /jobs/{id}/findings/handled", s.requireSession(s.handleHandleFinding))
	mux.HandleFunc("POST /jobs/{id}/clock-in", s.requireSession(s.handleClock(true)))
	mux.HandleFunc("POST /jobs/{id}/clock-out", s.requireSession(s.handleClock(false)))

	return s.securityHeaders(s.checkOrigin(s.requireSetup(s.withSession(mux)))), nil
}

// cacheForever is safe here because everything under /static is embedded in
// the binary and changes only when the binary does.
func cacheForever(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		next.ServeHTTP(w, r)
	})
}

// Run listens until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context, addr string) error {
	handler, err := s.routes()
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:    addr,
		Handler: handler,

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
