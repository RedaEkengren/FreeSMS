package server

import (
	"errors"
	"net/http"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// handleParts is the parts desk's screen: everything somebody is waiting for.
func (s *Server) handleParts(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())

	requests, err := workshop.OpenPartRequests(r.Context(), s.pool, session.Scope)
	if errors.Is(err, access.ErrForbidden) {
		s.renderError(w, r, http.StatusForbidden, "Not for your role",
			"The parts list is for the parts desk and the front desk.")
		return
	}
	if err != nil {
		s.log.Error("part requests", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "Try again.")
		return
	}
	s.render(w, r, http.StatusOK, "parts", pageData{
		Title:    "Waiting for parts",
		Session:  session,
		Requests: requests,
	})
}

func (s *Server) handleRequestPart(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")

	err := workshop.RequestPart(r.Context(), s.pool, session.Scope, id, r.FormValue("description"))
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
}

func (s *Server) handlePartArrived(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())

	err := workshop.MarkPartArrived(r.Context(), s.pool, session.Scope, r.FormValue("request_id"))
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/parts", http.StatusSeeOther)
}

func (s *Server) handleReportFinding(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")

	err := workshop.ReportFinding(r.Context(), s.pool, session.Scope, id, r.FormValue("note"))
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
}

func (s *Server) handleHandleFinding(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")

	err := workshop.HandleFinding(r.Context(), s.pool, session.Scope, r.FormValue("finding_id"))
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
}

// handoverError renders the shared failures and reports whether it did.
func (s *Server) handoverError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, access.ErrForbidden):
		s.renderError(w, r, http.StatusForbidden, "Not for your role", "")
	case errors.Is(err, workshop.ErrNotFound):
		s.renderError(w, r, http.StatusNotFound, "Not found", "It is not there any more.")
	case errors.Is(err, workshop.ErrInvalid):
		s.renderError(w, r, http.StatusBadRequest, "Not accepted", trimInvalid(err))
	default:
		s.log.Error("handover", "error", err, "path", r.URL.Path)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "Try again.")
	}
	return true
}
