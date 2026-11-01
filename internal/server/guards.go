package server

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The model years the vehicles table accepts (its CHECK constraint says the
// same). Asking here means a person is told, instead of being shown an error
// page by the constraint.
const (
	minModelYear = 1885
	maxModelYear = 2100
)

var uuidShape = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// uuidWildcards are the path wildcards that name a row by its UUID. The others
// ({token}, {key}, {kind}, {step}, {position}, {version}, {role}, {secret})
// are something else and are read by their own handlers.
var uuidWildcards = []string{"id", "itemID", "line", "note", "quoteID", "rec", "receipt", "ret", "row", "task"}

// routeMux is the router with one addition: a route whose path names a row by
// UUID answers 404 when the text in that place is not one. Without it
// /jobs/abc reached the database, which refused the text with a type error,
// and the person was shown "Something went wrong" for a link that is simply
// not there.
//
// And a second: every route belongs to Core or to a module (#143), and a
// module's route is gated on the shop running it. The gate is outermost, ahead
// of the session check, so that a switched-off module's address answers a
// visitor who is not signed in exactly as it answers one who is: as a path
// that does not exist.
type routeMux struct {
	*http.ServeMux
	s *Server
	// The module the routes registered through this value belong to; empty
	// for Core.
	module access.Module
	// Where every registration is written down, shared by every value made
	// from the same mux.
	table *[]registeredRoute
}

// registeredRoute is one pattern and the module it belongs to.
type registeredRoute struct {
	Pattern string
	Module  access.Module
}

// in is the same router, registering routes as m's.
func (m routeMux) in(module access.Module) routeMux {
	m.module = module
	return m
}

func (m routeMux) Handle(pattern string, h http.Handler) {
	m.record(pattern)
	m.ServeMux.Handle(pattern, m.gate(h))
}

// HandleFunc registers h with the module gate outermost, ahead of the check
// that an identifier is shaped like one: a switched-off module's /jobs/abc is
// a path that does not exist, as its /jobs/<uuid> is, not the styled "not
// found" the shape check gives a module that is on.
func (m routeMux) HandleFunc(pattern string, h http.HandlerFunc) {
	m.record(pattern)
	m.ServeMux.Handle(pattern, m.gate(m.shapeChecked(pattern, h)))
}

func (m routeMux) record(pattern string) {
	if m.table != nil {
		*m.table = append(*m.table, registeredRoute{Pattern: pattern, Module: m.module})
	}
}

// gate answers as an unregistered path would -- the same status, the same
// body -- when the shop does not run the route's module. Nothing in the
// answer says a module is there to be switched on.
func (m routeMux) gate(h http.Handler) http.Handler {
	if m.module == "" {
		return h
	}
	module := m.module
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.s.runs(r, module) {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (m routeMux) shapeChecked(pattern string, h http.HandlerFunc) http.Handler {
	var names []string
	for _, n := range uuidWildcards {
		if strings.Contains(pattern, "{"+n+"}") {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, n := range names {
			if !uuidShape.MatchString(r.PathValue(n)) {
				m.s.renderError(w, r, http.StatusNotFound, "Not found", "It is not there any more.")
				return
			}
		}
		h(w, r)
	})
}

// ServeHTTP answers a method nobody registered on a path the way an
// unknown path is answered when every route on that path belongs to a
// module the shop does not run. Left to the ServeMux it is 405 with an Allow
// header naming the methods that are there, which says the path is there to
// be switched on. A module that runs, or a Core route on the path, keeps the
// ServeMux's 405.
func (m routeMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, pattern := m.ServeMux.Handler(r); pattern == "" && m.onlySwitchedOff(r) {
		http.NotFound(w, r)
		return
	}
	m.ServeMux.ServeHTTP(w, r)
}

// onlySwitchedOff reports whether some other method has a route on this path
// and every such route is a module's the shop does not run. Asked only of a
// request nothing matched, so an ordinary request pays nothing.
func (m routeMux) onlySwitchedOff(r *http.Request) bool {
	if m.table == nil {
		return false
	}
	found := false
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if method == r.Method {
			continue
		}
		other := *r
		other.Method = method
		_, pattern := m.ServeMux.Handler(&other)
		if pattern == "" {
			continue
		}
		for _, rt := range *m.table {
			if rt.Pattern != pattern {
				continue
			}
			found = true
			if rt.Module == "" || m.s.runs(r, rt.Module) {
				return false
			}
		}
	}
	return found
}

// refuseNUL answers 400 to a request with a NUL character (0x00) in a form
// field or the query. Postgres cannot store one in text, so it would otherwise
// come back from the database as an error on whichever of the two hundred
// fields happened to carry it. No person types one; it comes from a paste, a
// broken script or somebody trying.
func (s *Server) refuseNUL(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") ||
			r.URL.RawQuery != "" {
			if err := r.ParseForm(); err == nil {
				for k, vs := range r.Form {
					if strings.ContainsRune(k, 0) {
						s.renderError(w, r, http.StatusBadRequest, "Not accepted", "A text cannot contain a null character.")
						return
					}
					for _, v := range vs {
						if strings.ContainsRune(v, 0) {
							s.renderError(w, r, http.StatusBadRequest, "Not accepted", "A text cannot contain a null character.")
							return
						}
					}
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// fault answers an error from the workshop that the handler did not
// recognise. A value the person typed that the database would not take -- too
// long, too large, not a date, or an email that another customer has -- is
// theirs to correct and is told so; anything else is ours, is logged, and is
// "something went wrong".
func (s *Server) fault(w http.ResponseWriter, r *http.Request, what string, err error) {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch {
		case pg.Code == "23505" && strings.Contains(pg.ConstraintName, "email"):
			s.log.Warn(what, "error", err, "path", r.URL.Path)
			s.renderError(w, r, http.StatusConflict, "Not accepted",
				"That email address already belongs to another customer; open that customer instead.")
			return
		case pg.Code == "23505":
			s.log.Warn(what, "error", err, "path", r.URL.Path)
			s.renderError(w, r, http.StatusConflict, "Not accepted", "That already exists.")
			return
		case pg.Code == "23514" || strings.HasPrefix(pg.Code, "22"):
			s.log.Warn(what, "error", err, "path", r.URL.Path)
			s.renderError(w, r, http.StatusBadRequest, "Not accepted",
				"One of the values is outside what is allowed: too long, too large or not valid.")
			return
		}
	}
	// An operation of a module the shop has switched off, reached from a
	// page that is not that module's own.
	if errors.Is(err, access.ErrModuleOff) {
		s.log.Warn(what, "error", err, "path", r.URL.Path)
		s.renderError(w, r, http.StatusConflict, "Switched off",
			"That is part of something this shop has switched off. What it recorded before is still here; nothing new can be added to it.")
		return
	}
	s.log.Error(what, "error", err, "path", r.URL.Path)
	s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "Try again.")
}

// runs reports whether the shop runs a module, for a request: from the
// session when there is one, which carries the shop's modules as they were
// read for this request, and from the shop otherwise -- the customer's links
// have no session. A shop that cannot be read runs nothing: a module's page
// answering "not found" for a moment is better than one served regardless.
func (s *Server) runs(r *http.Request, m access.Module) bool {
	if session := sessionFrom(r.Context()); session.Scope.ShopID != "" {
		return session.Scope.Uses(m)
	}
	on, err := s.modules.runs(r.Context(), s.pool, s.shop(), m)
	if err != nil {
		s.log.Warn("read the shop's modules", "error", err)
		return false
	}
	return on
}

// moduleCacheFor is how long the modules read for a request with no session
// are kept. A customer's link to a job with twenty photographs is
// twenty-one requests in a second, and each read them in a transaction of
// its own. A switch made in this process clears it at once (forget); the
// time only bounds a change made another way, such as a restore.
const moduleCacheFor = 5 * time.Second

// moduleCache keeps what each shop runs for a moment, for requests nobody is
// signed in to. Never for a session's: those carry the modules read with the
// session, on every request, so a switch counts from the next click.
type moduleCache struct {
	mu    sync.Mutex
	shops map[string]cachedModules
	// Reads made, for the test that counts them.
	reads atomic.Int64
}

type cachedModules struct {
	on map[access.Module]bool
	at time.Time
}

func (c *moduleCache) runs(ctx context.Context, pool *pgxpool.Pool, shopID string, m access.Module) (bool, error) {
	if shopID == "" {
		return false, nil
	}
	c.mu.Lock()
	cached, ok := c.shops[shopID]
	c.mu.Unlock()
	if ok && time.Since(cached.at) < moduleCacheFor {
		return cached.on[m], nil
	}
	c.reads.Add(1)
	modules, err := workshop.ShopModules(ctx, pool, shopID)
	if err != nil {
		return false, err
	}
	on := map[access.Module]bool{}
	for _, mod := range modules {
		on[mod] = true
	}
	c.mu.Lock()
	if c.shops == nil {
		c.shops = map[string]cachedModules{}
	}
	c.shops[shopID] = cachedModules{on: on, at: time.Now()}
	c.mu.Unlock()
	return on[m], nil
}

// forget drops what is kept, after a switch or a setup.
func (c *moduleCache) forget() {
	c.mu.Lock()
	c.shops = nil
	c.mu.Unlock()
}
