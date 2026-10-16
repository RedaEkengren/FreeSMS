package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A car that comes back.

func (s *Server) handleComebackPage(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	if !session.Scope.Role.SeesCustomerPersonalData() {
		s.renderError(w, r, http.StatusForbidden, "Not for your role", "")
		return
	}
	id := r.PathValue("id")
	job, lines, err := workshop.JobByID(r.Context(), s.pool, session.Scope, id)
	if s.handoverError(w, r, err) {
		return
	}
	data := pageData{Title: "Return visit", Session: session, Job: job, Lines: lines, Today: s.today(r).Format("2006-01-02")}
	if job.ReturnOf != 0 {
		cb, err := workshop.ComebackFor(r.Context(), s.pool, session.Scope, id)
		if s.handoverError(w, r, err) {
			return
		}
		data.Comeback = cb
		data.Positions = workshop.ComebackPositions()
		data.ClaimStates = workshop.ClaimAnswers()
	}
	if job.Returns > 0 || data.Comeback.ID == "" {
		cbs, err := workshop.ComebacksOf(r.Context(), s.pool, session.Scope, id)
		if s.handoverError(w, r, err) {
			return
		}
		data.Comebacks = cbs
	}
	switch workshop.State(job.State) {
	case workshop.StateReady, workshop.StateInvoiced, workshop.StateClosed:
		data.CanReturn = true
	}
	s.render(w, r, http.StatusOK, "comeback", data)
}

func (s *Server) handleComeback(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	bad := func(msg string) { s.renderError(w, r, http.StatusBadRequest, "Not recorded", msg) }
	var err error
	to := "/jobs/" + id + "/comeback"
	switch r.FormValue("action") {
	case "open":
		returned, ok := s.dayField(r, "returned_on")
		if !ok || returned == nil {
			bad("Say which day it came back.")
			return
		}
		in := workshop.ComebackInput{OriginalJobID: id, LineID: r.FormValue("line"), Symptoms: r.FormValue("symptoms"), ReturnedOn: *returned}
		if v := strings.TrimSpace(r.FormValue("odometer")); v != "" {
			km, perr := strconv.ParseInt(strings.ReplaceAll(v, " ", ""), 10, 64)
			if perr != nil {
				bad("Write the odometer as a whole number of kilometres.")
				return
			}
			in.OdometerKm = &km
		}
		var follow string
		follow, err = workshop.OpenComeback(r.Context(), s.pool, session.Scope, in)
		to = "/jobs/" + follow + "/comeback"
	case "classify":
		err = workshop.Classify(r.Context(), s.pool, session.Scope, r.FormValue("case"), r.FormValue("kind"), r.FormValue("note"))
	case "note":
		err = workshop.AddComebackNote(r.Context(), s.pool, session.Scope, r.FormValue("case"), r.FormValue("note"))
	case "claim":
		in := workshop.ClaimInput{Supplier: r.FormValue("supplier"), Reference: r.FormValue("reference")}
		if v := strings.TrimSpace(r.FormValue("amount")); v != "" {
			amount, perr := parseMinorUnits(v)
			if perr != nil {
				bad("Write the amount as 150 or 150,50.")
				return
			}
			in.AmountMinor = &amount
		}
		_, err = workshop.FileClaim(r.Context(), s.pool, session.Scope, r.FormValue("case"), in)
	case "answer":
		var amount *int64
		if v := strings.TrimSpace(r.FormValue("amount")); v != "" {
			a, perr := parseMinorUnits(v)
			if perr != nil {
				bad("Write the amount as 150 or 150,50.")
				return
			}
			amount = &a
		}
		err = workshop.AnswerClaim(r.Context(), s.pool, session.Scope, r.FormValue("claim"), r.FormValue("status"), amount, r.FormValue("note"))
	default:
		bad("Say what to do.")
		return
	}
	if errors.Is(err, workshop.ErrInvalid) {
		bad(trimInvalid(err))
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}
