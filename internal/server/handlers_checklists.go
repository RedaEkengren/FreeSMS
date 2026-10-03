package server

import (
	"errors"
	"net/http"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// handleChecklists lists the shop's inspection checklists, with an empty form
// to make one -- or, given an id, that one in the form to edit.
func (s *Server) handleChecklists(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	var editing workshop.ChecklistTemplate
	if id := r.PathValue("id"); id != "" {
		var err error
		editing, err = workshop.ChecklistTemplateByID(r.Context(), s.pool, session.Scope, id)
		if s.checklistError(w, r, err) {
			return
		}
	}
	s.renderChecklists(w, r, http.StatusOK, editing, "")
}

// handleSaveChecklist creates a checklist, or replaces one's name and
// checkpoints when the path carries an id.
func (s *Server) handleSaveChecklist(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	typed := workshop.ChecklistTemplate{
		ID:          r.PathValue("id"),
		Name:        r.FormValue("name"),
		Checkpoints: workshop.ParseCheckpoints(r.FormValue("checkpoints")),
	}
	_, err := workshop.SaveChecklistTemplate(r.Context(), s.pool, session.Scope,
		typed.ID, typed.Name, typed.Checkpoints)
	if errors.Is(err, workshop.ErrInvalid) {
		// Back to the form with what was typed. A thirty-line checklist
		// cleared because one line was there twice is typed twice.
		s.renderChecklists(w, r, http.StatusBadRequest, typed, trimInvalid(err))
		return
	}
	if s.checklistError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/checklists", http.StatusSeeOther)
}

// handleChecklistActive retires a checklist or brings one back.
func (s *Server) handleChecklistActive(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	err := workshop.SetChecklistActive(r.Context(), s.pool, session.Scope,
		r.PathValue("id"), r.FormValue("active") == "yes")
	if s.checklistError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/checklists", http.StatusSeeOther)
}

func (s *Server) renderChecklists(w http.ResponseWriter, r *http.Request, status int, editing workshop.ChecklistTemplate, problem string) {
	session := sessionFrom(r.Context())
	all, err := workshop.ChecklistTemplates(r.Context(), s.pool, session.Scope)
	if s.checklistError(w, r, err) {
		return
	}
	s.render(w, r, status, "checklists", pageData{
		Title:      "Inspection checklists",
		Session:    session,
		Checklists: all,
		Checklist:  editing,
		Error:      problem,
	})
}

func (s *Server) checklistError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, access.ErrForbidden):
		s.renderError(w, r, http.StatusForbidden, "Not for your role",
			"Checklists are kept by whoever runs the workshop.")
	case errors.Is(err, workshop.ErrNotFound):
		s.renderError(w, r, http.StatusNotFound, "Not found", "No such checklist.")
	case errors.Is(err, workshop.ErrInvalid):
		s.renderError(w, r, http.StatusBadRequest, "Not accepted", trimInvalid(err))
	default:
		s.log.Error("checklists", "error", err, "path", r.URL.Path)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "Try again.")
	}
	return true
}
