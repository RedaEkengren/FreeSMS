package server

import (
	"net/http"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// What technicians see (#225): the shop's profile and one person's
// exceptions. Whoever runs the shop.
func (s *Server) handleTechnicianView(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	view, err := workshop.TechnicianView(r.Context(), s.pool, session.Scope)
	if s.handoverError(w, r, err) {
		return
	}
	s.render(w, r, http.StatusOK, "technicians", pageData{Title: "What technicians see", Session: session, TechView: &view})
}

func (s *Server) handleTechnicianViewAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := sessionFrom(r.Context())
		field, reason := r.FormValue("field"), r.FormValue("reason")
		var err error
		switch action {
		case "field":
			err = workshop.SetTechnicianField(r.Context(), s.pool, session.Scope, field, r.FormValue("shown") == "yes", reason)
		case "grant":
			var until *time.Time
			if v := r.FormValue("until"); v != "" {
				// The end of the day chosen, in the shop's time: an exception
				// "until Friday" applies all of Friday.
				day, perr := time.ParseInLocation("2006-01-02", v, s.shopLocation(r))
				if perr != nil {
					s.renderError(w, r, http.StatusBadRequest, "That date did not parse", "Use the picker.")
					return
				}
				end := day.AddDate(0, 0, 1)
				until = &end
			}
			err = workshop.GrantTechnicianField(r.Context(), s.pool, session.Scope, r.FormValue("user_id"), field, reason, until)
		case "end":
			err = workshop.EndTechnicianGrant(r.Context(), s.pool, session.Scope, r.FormValue("user_id"), field, reason)
		}
		if s.handoverError(w, r, err) {
			return
		}
		s.submitted(w, r)
		http.Redirect(w, r, "/technicians", http.StatusSeeOther)
	}
}

// handleContactRequest is a technician asking the front desk to contact
// the customer, without being shown their number.
func (s *Server) handleContactRequest(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	if s.handoverError(w, r, workshop.AskDeskToContact(r.Context(), s.pool, session.Scope, id, r.FormValue("why"))) {
		return
	}
	http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
}
