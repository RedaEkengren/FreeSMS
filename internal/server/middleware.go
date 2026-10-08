package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/RedaEkengren/FreeSMS/internal/auth"
)

const sessionCookie = "freesms_session"

type contextKey string

const sessionKey contextKey = "session"

func sessionFrom(ctx context.Context) auth.Session {
	s, _ := ctx.Value(sessionKey).(auth.Session)
	return s
}

// withSession turns a cookie into a scope, or leaves the request anonymous.
//
// A token that no longer works is not an error here -- the cookie is cleared
// and the request continues unauthenticated, so an expired session lands on
// the sign-in page rather than on a stack trace.
func (s *Server) withSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookie)
		if err != nil || cookie.Value == "" {
			next.ServeHTTP(w, r)
			return
		}

		authenticate := auth.Authenticate
		if background(r) {
			authenticate = auth.Check
		}
		session, err := authenticate(r.Context(), s.pool, s.shop(), cookie.Value)
		if err != nil {
			s.clearSessionCookie(w)
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionKey, session)))
	})
}

// LiveHeader marks a request a screen made to refresh itself, rather than one
// a person made.
const LiveHeader = "X-FreeSMS-Live"

// background reports a request no person made: the live stream, or a screen
// refreshing itself. It checks the session without counting as activity.
func background(r *http.Request) bool {
	return r.URL.Path == "/events" || r.Header.Get(LiveHeader) != ""
}

// requireSession sends anonymous requests to the sign-in page.
func (s *Server) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if sessionFrom(r.Context()).Scope.UserID == "" {
			// A script is told plainly; a person is sent to sign in.
			//
			// The offline queue follows redirects, and the sign-in page answers
			// 200 -- so a 303 to /login looked exactly like success, and the
			// queue threw away work that had never been done. Anything that is
			// not a person navigating gets a 401 it cannot mistake for anything
			// else. htmx is told where to go as well, so a swap on a page whose
			// session expired takes the person to sign in rather than doing
			// nothing.
			if r.Header.Get(IdempotencyHeader) != "" || r.Header.Get("HX-Request") != "" {
				w.Header().Set("HX-Redirect", "/login")
				http.Error(w, "sign in again", http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

// checkOrigin refuses cross-site writes.
//
// The session cookie is already SameSite=Lax, which stops a form on another
// site from carrying it -- but that is one browser setting away from being the
// only thing standing between a workshop and a forged request, and older
// browsers do not enforce it at all. Comparing Origin against the host is
// cheap and independent of it.
//
// Only unsafe methods are checked. A GET that changes something would be a bug
// of its own.
func (s *Server) checkOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		// A provider's report carries no cookie, so there is no authority
		// to forge; it is checked by the secret in its address instead.
		if strings.HasPrefix(r.URL.Path, "/hooks/") {
			next.ServeHTTP(w, r)
			return
		}

		origin := r.Header.Get("Origin")
		if origin == "" {
			// Some clients omit Origin on same-origin form posts. Fall back to
			// Referer, and refuse when neither is present on a write.
			origin = r.Header.Get("Referer")
		}
		if origin == "" {
			s.log.Warn("write with no origin", "path", r.URL.Path)
			http.Error(w, "missing origin", http.StatusForbidden)
			return
		}

		u, err := url.Parse(origin)
		if err != nil || !strings.EqualFold(u.Host, r.Host) {
			s.log.Warn("cross-origin write refused", "origin", origin, "host", r.Host, "path", r.URL.Path)
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// securityHeaders applies the ones that are free and unconditional.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// No inline script and no external origin: everything this page needs
		// is served from the binary.
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string, session auth.Session) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  session.ExpiresAt,
		HttpOnly: true,
		// Lax rather than Strict: Strict would drop the cookie when a
		// technician follows a link from a chat message into the system, which
		// looks like being signed out at random.
		SameSite: http.SameSiteLaxMode,
		Secure:   s.secureCookies,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.secureCookies,
	})
}
