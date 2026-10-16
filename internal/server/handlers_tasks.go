package server

import (
	"net/http"
	"strconv"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// The job's work list. Every handler hands the form to the workshop package
// and goes back to the job; who may do what is decided there.

func (s *Server) handleAddTask(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	line, _ := strconv.Atoi(r.FormValue("line"))
	_, err := workshop.AddTask(r.Context(), s.pool, session.Scope, id, r.FormValue("title"), line, r.FormValue("finding_id"))
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/jobs/"+id+"#work-list", http.StatusSeeOther)
}

func (s *Server) handleTasksFromLines(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	_, err := workshop.TasksFromLines(r.Context(), s.pool, session.Scope, id)
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/jobs/"+id+"#work-list", http.StatusSeeOther)
}

func (s *Server) handleMoveTask(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	err := workshop.MoveTask(r.Context(), s.pool, session.Scope, r.PathValue("task"), workshop.TaskMove{
		To:            workshop.TaskStatus(r.FormValue("to")),
		Blocker:       r.FormValue("blocker"),
		Note:          r.FormValue("note"),
		NeedsDecision: r.FormValue("needs_decision") != "",
	})
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/jobs/"+id+"#work-list", http.StatusSeeOther)
}

func (s *Server) handleTaskDecision(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	err := workshop.RecordTaskDecision(r.Context(), s.pool, session.Scope, r.PathValue("task"), r.FormValue("note"))
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/jobs/"+id+"#work-list", http.StatusSeeOther)
}
