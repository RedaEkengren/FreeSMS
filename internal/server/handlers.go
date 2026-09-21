package server

import (
	"errors"
	"net/http"

	"github.com/RedaEkengren/RedaSMS/internal/auth"
	"github.com/RedaEkengren/RedaSMS/internal/workshop"
)

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if sessionFrom(r.Context()).Scope.UserID != "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "login", pageData{Title: "Sign in"})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	email := r.FormValue("email")
	password := r.FormValue("password")

	token, session, err := auth.Login(r.Context(), s.pool, s.shop(), email, password, r.UserAgent())
	switch {
	case errors.Is(err, auth.ErrThrottled):
		// 429 rather than 401: the credentials were not judged at all, and a
		// client that retries on 401 would hammer a door that is already shut.
		s.render(w, r, http.StatusTooManyRequests, "login", pageData{
			Title: "Sign in",
			Email: email,
			Error: "Too many attempts. Wait a few minutes and try again.",
		})
		return
	case errors.Is(err, auth.ErrInvalidCredentials):
		// One message for a wrong password, an unknown address and a
		// deactivated account. Telling them apart tells whoever is guessing
		// which addresses are worth their time.
		s.render(w, r, http.StatusUnauthorized, "login", pageData{
			Title: "Sign in",
			Email: email,
			Error: "Those details do not match an account.",
		})
		return
	case err != nil:
		s.log.Error("login", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "Try again.")
		return
	}

	s.setSessionCookie(w, token, session)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil && cookie.Value != "" {
		if err := auth.Logout(r.Context(), s.pool, s.shop(), cookie.Value); err != nil {
			s.log.Error("logout", "error", err)
		}
	}
	s.clearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	jobs, err := workshop.OpenJobs(r.Context(), s.pool, session.Scope)
	if err != nil {
		s.log.Error("list jobs", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "Try again.")
		return
	}
	s.render(w, r, http.StatusOK, "jobs", pageData{
		Title:   "In the shop",
		Session: session,
		Jobs:    jobs,
	})
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")

	job, lines, err := workshop.JobByID(r.Context(), s.pool, session.Scope, id)
	if errors.Is(err, workshop.ErrNotFound) {
		// A work order belonging to another shop is not there, so this is the
		// same 404 as one that never existed. Row level security makes that
		// the natural outcome rather than something a handler has to remember.
		s.renderError(w, r, http.StatusNotFound, "Not found", "No such job.")
		return
	}
	if err != nil {
		s.log.Error("read job", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "Try again.")
		return
	}

	s.render(w, r, http.StatusOK, "job", pageData{
		Title:   "Job",
		Session: session,
		Job:     job,
		Lines:   lines,
		Next:    workshop.NextStates(workshop.State(job.State)),
	})
}

// handleClock starts or stops the caller's clock and returns the button.
//
// The response is the block htmx swapped from, so the page does not reload and
// a technician does not lose their place on a phone.
func (s *Server) handleClock(in bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := sessionFrom(r.Context())
		id := r.PathValue("id")

		var err error
		if in {
			err = workshop.ClockIn(r.Context(), s.pool, session.Scope, id)
		} else {
			err = workshop.ClockOut(r.Context(), s.pool, session.Scope, id)
		}
		if errors.Is(err, workshop.ErrNotFound) {
			http.Error(w, "no such job", http.StatusNotFound)
			return
		}
		if err != nil {
			s.log.Error("clock", "in", in, "error", err)
			http.Error(w, "could not change the clock", http.StatusInternalServerError)
			return
		}

		// Read the job back rather than assuming the new state. If something
		// else stopped the clock in between, the button must show what is
		// true, not what this request intended.
		job, _, err := workshop.JobByID(r.Context(), s.pool, session.Scope, id)
		if err != nil {
			http.Error(w, "could not read the job back", http.StatusInternalServerError)
			return
		}
		s.renderPartial(w, r, "job", "clock", pageData{Session: session, Job: job})
	}
}
