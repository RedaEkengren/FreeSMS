package server

import (
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
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
type routeMux struct {
	*http.ServeMux
	s *Server
}

func (m routeMux) HandleFunc(pattern string, h http.HandlerFunc) {
	var names []string
	for _, n := range uuidWildcards {
		if strings.Contains(pattern, "{"+n+"}") {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		m.ServeMux.HandleFunc(pattern, h)
		return
	}
	m.ServeMux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		for _, n := range names {
			if !uuidShape.MatchString(r.PathValue(n)) {
				m.s.renderError(w, r, http.StatusNotFound, "Not found", "It is not there any more.")
				return
			}
		}
		h(w, r)
	})
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
	s.log.Error(what, "error", err, "path", r.URL.Path)
	s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "Try again.")
}
