package server

import (
	"context"
	"net/http"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// handleOutbox lists the calls to other services that need a person: given
// up after their attempts, or a task to do by hand (#154).
func (s *Server) handleOutbox(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	items, err := workshop.OutboxNeedingAttention(r.Context(), s.pool, session.Scope)
	if s.handoverError(w, r, err) {
		return
	}
	s.render(w, r, http.StatusOK, "outbox", pageData{Title: "Calls to other services", Session: session, Outbox: items})
}

// handleOutboxAction tries a call again, says it was done by hand, or
// cancels it with why.
func (s *Server) handleOutboxAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := sessionFrom(r.Context())
		id := r.PathValue("id")
		var err error
		switch action {
		case "retry":
			err = workshop.RetryOutbox(r.Context(), s.pool, session.Scope, id)
		case "done":
			err = workshop.FinishOutboxByHand(r.Context(), s.pool, session.Scope, id, r.FormValue("note"))
		default:
			err = workshop.CancelOutbox(r.Context(), s.pool, session.Scope, id, r.FormValue("reason"))
		}
		if s.handoverError(w, r, err) {
			return
		}
		http.Redirect(w, r, "/outbox", http.StatusSeeOther)
	}
}

// runOutbox sends what is due through the adapters configured: none ships,
// so every call becomes a task for a person until a provider is connected
// (#235).
func (s *Server) runOutbox(ctx context.Context) {
	shop := s.shop()
	if shop == "" {
		return
	}
	if _, err := workshop.RunOutbox(ctx, s.pool, shop, s.outboxAdapters, 20); err != nil {
		s.log.Error("run the outbox", "error", err)
	}
}
