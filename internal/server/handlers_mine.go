package server

import (
	"context"
	"net/http"

	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// handleMine is what is waiting for the person signed in, read in one
// transaction so that the counter's half and the parts half are one moment.
func (s *Server) handleMine(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	var items []workshop.WaitingItem
	var err error
	database.Reading(r.Context(), s.pool, session.Scope, func(ctx context.Context) {
		items, err = workshop.WaitingFor(ctx, s.pool, session.Scope)
	})
	if s.handoverError(w, r, err) {
		return
	}
	s.render(w, r, http.StatusOK, "mine", pageData{Title: "For me", Session: session, Waiting: items})
}

// handleMineCount is the number in the header, asked for after the page has
// loaded and again whenever the stream says something changed -- so it costs
// no page its budget, and no refresh keeps a session alive.
func (s *Server) handleMineCount(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	var n int
	var sound bool
	var err error
	database.Reading(r.Context(), s.pool, session.Scope, func(ctx context.Context) {
		if n, err = workshop.CountWaiting(ctx, s.pool, session.Scope); err != nil {
			return
		}
		sound, err = workshop.AlertSound(ctx, s.pool, session.Scope)
	})
	if err != nil {
		s.log.Error("count waiting", "error", err)
	}
	s.renderPartial(w, r, "mine", "count", pageData{Session: session, WaitingCount: n, AlertSound: sound})
}

// handleAlertSound is the person's own choice to hear when something new is
// waiting. Off until they turn it on.
func (s *Server) handleAlertSound(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	if s.handoverError(w, r, workshop.SetAlertSound(r.Context(), s.pool, session.Scope, r.FormValue("on") == "yes")) {
		return
	}
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}
