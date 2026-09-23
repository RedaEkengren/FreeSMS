package server

import (
	"net/http"

	"github.com/RedaEkengren/RedaSMS/internal/workshop"
)

// handleExportPerson sends somebody a copy of what is held about them.
func (s *Server) handleExportPerson(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")

	export, err := workshop.ExportPerson(r.Context(), s.pool, session.Scope, id)
	if s.handoverError(w, r, err) {
		return
	}
	body, err := workshop.MarshalExport(export)
	if err != nil {
		s.log.Error("marshal export", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "Try again.")
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// A file the person can keep, rather than a page they have to screenshot.
	w.Header().Set("Content-Disposition", `attachment; filename="personal-data.json"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

func (s *Server) handleErasePerson(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")

	if err := workshop.ErasePerson(r.Context(), s.pool, session.Scope, id, r.FormValue("reason")); s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/privacy", http.StatusSeeOther)
}

// handlePrivacy is where the shop answers for what it holds.
func (s *Server) handlePrivacy(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())

	erasures, err := workshop.Erasures(r.Context(), s.pool, session.Scope)
	if s.handoverError(w, r, err) {
		return
	}
	s.render(w, r, http.StatusOK, "privacy", pageData{
		Title:    "Personal data",
		Session:  session,
		Erasures: erasures,
	})
}
