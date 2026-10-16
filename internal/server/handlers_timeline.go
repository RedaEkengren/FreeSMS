package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// The timeline and handover of a job, and the notes written there.

func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	job, _, err := workshop.JobByID(r.Context(), s.pool, session.Scope, id)
	if s.handoverError(w, r, err) {
		return
	}
	events, err := workshop.TimelineFor(r.Context(), s.pool, session.Scope, id)
	if s.handoverError(w, r, err) {
		return
	}
	handover, err := workshop.HandoverFor(r.Context(), s.pool, session.Scope, id)
	if s.handoverError(w, r, err) {
		return
	}
	s.render(w, r, http.StatusOK, "timeline", pageData{
		Title: "Timeline", Session: session, Job: job, Events: events, Handover: handover,
		Today: time.Now().In(s.shopLocation(r)).Format("2006-01-02T15:04"),
	})
}

func (s *Server) handleAddNote(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	in := workshop.NoteInput{
		Kind: r.FormValue("kind"), Channel: r.FormValue("channel"), Body: r.FormValue("body"),
		// A conversation with the customer is the counter's unless the writer
		// says the shop floor may read it.
		DeskOnly: r.FormValue("everyone") == "",
	}
	if in.Kind == "note" || in.Kind == "" {
		in.Kind = "note"
		in.DeskOnly = r.FormValue("desk_only") != ""
	}
	if v := strings.TrimSpace(r.FormValue("occurred")); v != "" {
		t, err := time.ParseInLocation("2006-01-02T15:04", v, s.shopLocation(r))
		if err != nil {
			s.renderError(w, r, http.StatusBadRequest, "Not accepted", "That is not a date and time.")
			return
		}
		in.Occurred = t
	}
	_, err := workshop.AddNote(r.Context(), s.pool, session.Scope, id, in)
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/jobs/"+id+"/timeline", http.StatusSeeOther)
}

func (s *Server) handleCorrectNote(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	err := workshop.CorrectNote(r.Context(), s.pool, session.Scope, r.PathValue("note"), r.FormValue("body"))
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/jobs/"+id+"/timeline", http.StatusSeeOther)
}
