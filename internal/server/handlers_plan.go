package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// planForm is a plan as it was typed, handed back with the clashes it ran
// into so that going ahead is a second, deliberate press with a reason.
type planForm struct {
	JobID, Version string
	Starts, Ends   string
	Hours          string
	OnLift         bool
	Note           string
	Clashes        []workshop.Clash
	// Carry is a move to the next open day rather than a typed plan.
	Carry bool
}

// planError turns the two outcomes only planning has into pages and leaves
// the rest to the common handler.
func (s *Server) planError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, workshop.ErrPlanChanged):
		s.renderError(w, r, http.StatusConflict, "Somebody else got there first",
			"Who is on this job, or when it is planned, changed while you were looking. Open the job again and decide from what it says now.")
		return true
	case errors.Is(err, workshop.ErrFinished):
		s.renderError(w, r, http.StatusConflict, "The job is over", "A finished job cannot be planned.")
		return true
	}
	return s.handoverError(w, r, err)
}

// handleStaffJob puts technicians on a job, takes them off, or hands it from
// one to another.
func (s *Server) handleStaffJob(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	reason := r.FormValue("reason")
	var err error
	switch r.FormValue("action") {
	case "add":
		err = workshop.AssignTechnician(r.Context(), s.pool, session.Scope, id, r.FormValue("user_id"), reason)
	case "release":
		err = workshop.ReleaseTechnician(r.Context(), s.pool, session.Scope, id, r.FormValue("user_id"), reason)
	case "reassign":
		err = workshop.ReassignJob(r.Context(), s.pool, session.Scope, id, r.FormValue("from_id"), r.FormValue("user_id"), reason)
	default:
		s.renderError(w, r, http.StatusBadRequest, "Not accepted", "Put on, take off or hand over.")
		return
	}
	if s.planError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
}

// handlePlan writes a new version of the plan, or shows what it clashes with.
func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	form := planForm{
		JobID: id, Version: r.FormValue("version"),
		Starts: strings.TrimSpace(r.FormValue("starts")), Ends: strings.TrimSpace(r.FormValue("ends")),
		Hours: strings.TrimSpace(r.FormValue("hours")), OnLift: r.FormValue("on_lift") != "",
		Note: strings.TrimSpace(r.FormValue("note")),
	}
	in := workshop.PlanInput{OnLift: form.OnLift, Note: form.Note, Override: r.FormValue("override")}
	if v := strings.TrimSpace(form.Version); v != "" {
		n, err := parseScaled(v, 0)
		if err != nil || n < 0 {
			s.renderError(w, r, http.StatusBadRequest, "Not accepted", "That plan version is not a number.")
			return
		}
		in.Version = int(n)
	}
	if form.Starts != "" || form.Ends != "" {
		from, err1 := parseWallClock(form.Starts, s.shopLocation(r), "")
		to, err2 := parseWallClock(form.Ends, s.shopLocation(r), "")
		if err1 != nil || err2 != nil {
			s.renderError(w, r, http.StatusBadRequest, "That time will not do", "Give both when it starts and when it ends, with the pickers.")
			return
		}
		in.Starts, in.Ends = &from, &to
	}
	if form.Hours != "" {
		h, err := parseScaled(form.Hours, 2)
		if err != nil || h < 0 {
			s.renderError(w, r, http.StatusBadRequest, "That did not parse", "Write the hours left as a number, with a comma or a full stop: 6 or 6,5.")
			return
		}
		m := int(h * 60 / 100)
		in.RemainingMinutes = &m
	}
	clashes, err := workshop.PlanJob(r.Context(), s.pool, session.Scope, id, in)
	if errors.Is(err, workshop.ErrPlanClash) {
		form.Clashes = clashes
		s.render(w, r, http.StatusConflict, "planclash", pageData{
			Title: "This plan clashes", Session: session, PlanForm: form,
		})
		return
	}
	if s.planError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
}

// handleCarryOver moves an unfinished job's plan to the next open day.
func (s *Server) handleCarryOver(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	n, err := parseScaled(r.FormValue("version"), 0)
	if err != nil || n < 0 {
		s.renderError(w, r, http.StatusBadRequest, "Not accepted", "That plan version is not a number.")
		return
	}
	clashes, err := workshop.CarryOver(r.Context(), s.pool, session.Scope, id, int(n), r.FormValue("override"))
	if errors.Is(err, workshop.ErrPlanClash) {
		// The carried-over window is not one the person typed; show what it
		// ran into and let them plan it by hand, with a reason if need be.
		s.render(w, r, http.StatusConflict, "planclash", pageData{
			Title: "This plan clashes", Session: session,
			PlanForm: planForm{JobID: id, Version: r.FormValue("version"), Clashes: clashes, OnLift: true, Carry: true},
		})
		return
	}
	if s.planError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
}
