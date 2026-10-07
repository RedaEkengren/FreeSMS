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

	"github.com/RedaEkengren/FreeSMS/internal/auth"
	"github.com/RedaEkengren/FreeSMS/internal/config"
	"github.com/RedaEkengren/FreeSMS/internal/i18n"
	"github.com/RedaEkengren/FreeSMS/internal/storage"
	"github.com/RedaEkengren/FreeSMS/internal/vehicledata"
	"github.com/RedaEkengren/FreeSMS/internal/web"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
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

	// The shop's currency, read when the shop resolves. A workshop does not
	// change currency while the process is running, so this is not re-read
	// per page: that would be a query on every render for a value that only
	// setup can change.
	resolvedCurrency string

	// The shop's timezone, read with the currency and for the same reason.
	// Every time a person reads or types is a wall clock in this zone; every
	// time stored is an instant. Nil until a shop resolves, which is UTC.
	resolvedZone *time.Location

	// Signalled when the shop is set after start-up, for whatever runs in the
	// background and needs to know -- the retention sweep. Buffered by one
	// and never blocked on: a signal nobody has read yet already says it.
	shopSet chan struct{}
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
func (s *Server) localeFor(r *http.Request, session auth.Session) string {
	if session.Locale != "" {
		return session.Locale
	}
	// The shop's language, not DEFAULT_LOCALE. A user who has never opened a
	// setting -- which is most of them -- and a page with nobody signed in
	// both belong to the shop, and a Swedish workshop running with the shipped
	// default was reading English at the door.
	//
	// The query only happens on this path, so a user who has chosen a language
	// costs nothing, and a user who has not costs one row by primary key.
	return s.shopLocale(r)
}

// shopLocale is the language for a page with nobody signed in.
//
// The customer's page is the case. They have no account, no setting and no way
// to ask for another language, so the shop's is the only honest signal.
// Accept-Language from the browser is tempting and wrong: a workshop writes to
// its customers in the language it does business in, and a Swedish shop should
// not address somebody in German because their phone is set that way.
func (s *Server) shopLocale(r *http.Request) string {
	locale, err := workshop.ShopLocale(r.Context(), s.pool, s.shop())
	if err != nil {
		s.log.Warn("read shop locale", "error", err)
		return s.locale
	}
	if locale == "" {
		return s.locale
	}
	return locale
}

func (s *Server) shop() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.resolved
}

func (s *Server) currency() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.resolvedCurrency
}

// Shop is the shop this process serves, or empty before setup. The one
// source of it: background work asks here rather than keeping a copy.
func (s *Server) Shop() string { return s.shop() }

// ShopSet fires when setup has made the shop.
func (s *Server) ShopSet() <-chan struct{} { return s.shopSet }

func (s *Server) setShop(id string) {
	s.mu.Lock()
	s.resolved = id
	s.mu.Unlock()
	select {
	case s.shopSet <- struct{}{}:
	default:
	}

	// Best effort, and on its own connection: a shop that cannot be read is
	// already failing louder elsewhere, and a page with no currency symbol is
	// better than no page.
	if id == "" {
		return
	}
	zone := time.UTC
	if name, err := workshop.ShopTimezone(context.Background(), s.pool, id); err != nil {
		s.log.Warn("read shop timezone", "error", err)
	} else if loc, err := time.LoadLocation(name); err != nil {
		s.log.Warn("unknown timezone", "timezone", name, "error", err)
	} else {
		zone = loc
	}
	s.mu.Lock()
	s.resolvedZone = zone
	s.mu.Unlock()

	currency, err := workshop.ShopCurrency(context.Background(), s.pool, id)
	if err != nil {
		s.log.Warn("read shop currency", "error", err)
		return
	}
	s.mu.Lock()
	s.resolvedCurrency = currency
	s.mu.Unlock()
}

// zone is the shop's timezone, or UTC before there is a shop or when it
// cannot be read: a wrong offset is better than a page that will not load.
func (s *Server) zone() *time.Location {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.resolvedZone == nil {
		return time.UTC
	}
	return s.resolvedZone
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
	// Setup creates the workshop in this language, and refuses one that did
	// not ship. A typo in DEFAULT_LOCALE should not leave nobody able to set
	// up: it is said once, here, and English is used.
	locale := cfg.DefaultLocale
	if !catalogues.Has(locale) {
		log.Warn("DEFAULT_LOCALE has no catalogue; using English", "locale", locale)
		locale = "en"
	}
	srv := &Server{
		pool:             pool,
		log:              log,
		release:          cfg.Release,
		configuredShopID: cfg.ShopID,
		locale:           locale,
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
		shopSet:       make(chan struct{}, 1),
	}

	// Through setShop, not by assigning the field: the currency is read at the
	// same moment, and a second way to resolve a shop is a second way to
	// forget something that goes with it.
	srv.setShop(shopID)
	// The shop known at start-up is not news: the sweep runs once when it
	// starts anyway. Only a shop set later, by setup, should wake it.
	select {
	case <-srv.shopSet:
	default:
	}
	return srv, nil
}

// canLookUp reports whether a registration lookup provider is configured.
//
// Asked of the value rather than of the configuration, so there is one answer:
// newLookup is the only place that decides, and a second reading of the
// environment is a second thing to keep in step with it.
func (s *Server) canLookUp() bool {
	_, none := s.vehicleLookup.(vehicledata.None)
	return !none
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
		versioned(http.FileServer(http.FS(staticFS)))))

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
	mux.HandleFunc("GET /invoices/{id}", s.requireSession(s.handleInvoiceDocument))
	mux.HandleFunc("POST /invoices/{id}/payments", s.requireSession(s.handleRecordPayment))
	mux.HandleFunc("POST /payments/{id}/reverse", s.requireSession(s.handleReversePayment))
	mux.HandleFunc("GET /receivables", s.requireSession(s.handleReceivables))

	mux.HandleFunc("POST /jobs/{id}/inspect", s.requireSession(s.handleStartInspection))
	mux.HandleFunc("POST /jobs/{id}/promise", s.requireSession(s.handlePromise))
	mux.HandleFunc("POST /jobs/{id}/answers/{itemID}", s.requireSession(s.handlePriceAnswer))
	mux.HandleFunc("POST /jobs/{id}/car", s.requireSession(s.handleCar))
	mux.HandleFunc("POST /jobs/{id}/takeout", s.requireSession(s.handleTakeOut))
	mux.HandleFunc("GET /staff", s.requireSession(s.handleStaff))
	mux.HandleFunc("POST /staff", s.requireSession(s.handleAddStaff))
	mux.HandleFunc("POST /staff/{id}/active", s.requireSession(s.handleStaffActive))
	mux.HandleFunc("POST /staff/{id}/password", s.requireSession(s.handleStaffPassword))
	mux.HandleFunc("GET /account", s.requireSession(s.handleAccount))
	mux.HandleFunc("POST /account/password", s.requireSession(s.handleChangePassword))
	mux.HandleFunc("POST /account/language", s.requireSession(s.handleMyLanguage))
	mux.HandleFunc("GET /checklists", s.requireSession(s.handleChecklists))
	mux.HandleFunc("POST /checklists", s.requireSession(s.handleSaveChecklist))
	mux.HandleFunc("GET /checklists/{id}", s.requireSession(s.handleChecklists))
	mux.HandleFunc("POST /checklists/{id}", s.requireSession(s.handleSaveChecklist))
	mux.HandleFunc("POST /checklists/{id}/active", s.requireSession(s.handleChecklistActive))
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
	mux.HandleFunc("GET /shop", s.requireSession(s.handleShop))
	mux.HandleFunc("POST /shop", s.requireSession(s.handleSaveShop))
	mux.HandleFunc("POST /shop/charges", s.requireSession(s.handleSaveSurcharges))
	mux.HandleFunc("GET /privacy", s.requireSession(s.handlePrivacy))
	mux.HandleFunc("GET /people/{id}/export", s.requireSession(s.handleExportPerson))
	mux.HandleFunc("POST /people/{id}/erase", s.requireSession(s.handleErasePerson))

	mux.HandleFunc("GET /labour", s.requireSession(s.handleLabour))
	mux.HandleFunc("POST /labour", s.requireSession(s.handleSaveLabour))
	mux.HandleFunc("POST /labour/rate", s.requireSession(s.handleSetLabourRate))

	mux.HandleFunc("GET /time", s.requireSession(s.handleTime))
	mux.HandleFunc("POST /time/correct", s.requireSession(s.handleCorrectTime))

	mux.HandleFunc("POST /drafts", s.requireSession(s.handleSaveDraft))
	mux.HandleFunc("GET /drafts", s.requireSession(s.handleLoadDraft))
	mux.HandleFunc("DELETE /drafts", s.requireSession(s.handleDiscardDraft))

	mux.HandleFunc("GET /search", s.requireSession(s.handleSearch))
	mux.HandleFunc("GET /vehicles/{id}", s.requireSession(s.handleVehicle))

	mux.HandleFunc("GET /scan", s.requireSession(s.handleScan))
	mux.HandleFunc("GET /labels", s.requireSession(s.handleLabels))
	mux.HandleFunc("GET /stock", s.requireSession(s.handleStock))
	mux.HandleFunc("POST /stock/move", s.requireSession(s.handleStockMove))
	mux.HandleFunc("POST /stock/count", s.requireSession(s.handleStocktake))
	mux.HandleFunc("GET /stock/parts/new", s.requireSession(s.handlePartForm))
	mux.HandleFunc("POST /stock/parts", s.requireSession(s.handleSavePart))
	mux.HandleFunc("GET /stock/parts/{id}", s.requireSession(s.handlePartForm))
	mux.HandleFunc("POST /stock/parts/{id}", s.requireSession(s.handleSavePart))
	mux.HandleFunc("POST /stock/parts/{id}/active", s.requireSession(s.handlePartActive))
	mux.HandleFunc("POST /stock/bands", s.requireSession(s.handleSavePriceBand))

	mux.HandleFunc("GET /parts", s.requireSession(s.handleParts))
	mux.HandleFunc("POST /parts/arrived", s.requireSession(s.handlePartArrived))
	mux.HandleFunc("GET /jobs/{id}/customer", s.requireSession(s.handleCustomerForm))
	mux.HandleFunc("POST /jobs/{id}/customer", s.requireSession(s.handleSaveCustomer))
	mux.HandleFunc("GET /jobs/{id}/parts", s.requireSession(s.handleJobParts))
	mux.HandleFunc("POST /jobs/{id}/parts", s.requireSession(s.handleRequestPart))
	mux.HandleFunc("POST /jobs/{id}/findings", s.requireSession(s.handleReportFinding))
	mux.HandleFunc("POST /jobs/{id}/findings/handled", s.requireSession(s.handleHandleFinding))
	mux.HandleFunc("POST /jobs/{id}/clock-in", s.requireSession(s.handleClock(true)))
	mux.HandleFunc("POST /jobs/{id}/clock-out", s.requireSession(s.handleClock(false)))

	return s.securityHeaders(s.checkOrigin(s.requireSetup(s.withSession(s.idempotent(mux))))), nil
}

// versioned sets how long a static file may be kept.
//
// Asked for with the version the page names, the bytes behind that URL never
// change, so it is kept for a year without asking again. Asked for without
// one, or with a version that is no longer current -- a page loaded before an
// update -- it is revalidated every time, which with the ETag is a 304 and no
// body when nothing changed. The file served is always the current one.
func versioned(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := web.Version(r.URL.Path)
		if current != "" {
			w.Header().Set("ETag", `"`+current+`"`)
		}
		if current != "" && r.URL.Query().Get("v") == current {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
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
