package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// The repair and collection summary: composed from what was recorded, handed
// over by the counter, frozen, and readable by the customer on their link.

func (s *Server) handleSummaryPage(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	job, _, err := workshop.JobByID(r.Context(), s.pool, session.Scope, id)
	if s.handoverError(w, r, err) {
		return
	}
	if !session.Scope.Role.SeesCustomerPersonalData() {
		s.renderError(w, r, http.StatusForbidden, "Not for your role", "Handing the car back is a conversation at the counter.")
		return
	}
	draft, err := workshop.DeliveryDraft(r.Context(), s.pool, session.Scope, id)
	if s.handoverError(w, r, err) {
		return
	}
	issued, err := workshop.SummariesFor(r.Context(), s.pool, session.Scope, id)
	if s.handoverError(w, r, err) {
		return
	}
	recs, err := workshop.RecommendationsFor(r.Context(), s.pool, session.Scope, id)
	if err != nil {
		s.log.Error("read recommendations", "error", err)
	}
	s.render(w, r, http.StatusOK, "summary", pageData{
		Title: "Repair summary", Session: session, Job: job, Draft: draft, Summaries: issued, Recommendations: recs,
	})
}

func (s *Server) handleSummaryVersion(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	version, _ := strconv.Atoi(r.PathValue("version"))
	sum, err := workshop.SummaryVersion(r.Context(), s.pool, session.Scope, r.PathValue("id"), version)
	if s.handoverError(w, r, err) {
		return
	}
	job, _, err := workshop.JobByID(r.Context(), s.pool, session.Scope, r.PathValue("id"))
	if s.handoverError(w, r, err) {
		return
	}
	s.render(w, r, http.StatusOK, "summarydoc", pageData{
		Title: "Repair summary", Session: session, Job: job, Summary: sum, StaffView: true,
	})
}

func (s *Server) handleIssueSummary(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	in := workshop.SummaryInput{
		Instructions: r.FormValue("instructions"), DeliveredTo: r.FormValue("delivered_to"), Note: r.FormValue("note"),
	}
	if v := strings.TrimSpace(r.FormValue("odometer")); v != "" {
		km, err := strconv.ParseInt(strings.ReplaceAll(v, " ", ""), 10, 64)
		if err != nil {
			s.renderError(w, r, http.StatusBadRequest, "Not accepted", "That is not an odometer reading.")
			return
		}
		in.OdometerKm = &km
	}
	sum, err := workshop.IssueSummary(r.Context(), s.pool, session.Scope, id, in)
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/jobs/"+id+"/summary/"+strconv.Itoa(sum.Version), http.StatusSeeOther)
}

func (s *Server) handleAddRecommendation(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	err := workshop.AddRecommendation(r.Context(), s.pool, session.Scope, id, r.FormValue("body"), r.FormValue("when"))
	if s.handoverError(w, r, err) {
		return
	}
	if session.Scope.Role.SeesCustomerPersonalData() {
		http.Redirect(w, r, "/jobs/"+id+"/summary", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
}

func (s *Server) handleWithdrawRecommendation(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	err := workshop.WithdrawRecommendation(r.Context(), s.pool, session.Scope, r.PathValue("rec"))
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/jobs/"+id+"/summary", http.StatusSeeOther)
}

func (s *Server) handleRecommendationEnquiry(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	enquiry, err := workshop.RecommendationToEnquiry(r.Context(), s.pool, session.Scope, r.PathValue("rec"))
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/enquiries/"+enquiry, http.StatusSeeOther)
}

// handleCustomerSummary is the customer's own copy, on their link.
func (s *Server) handleCustomerSummary(w http.ResponseWriter, r *http.Request) {
	sum, shop, err := workshop.CustomerSummary(r.Context(), s.pool, s.shop(), r.PathValue("token"))
	switch {
	case err == nil:
	case errors.Is(err, workshop.ErrShareNotUsable), errors.Is(err, workshop.ErrNotFound):
		s.render(w, r, http.StatusNotFound, "status", pageData{
			Locale: s.shopLocale(r), Title: "This link is no longer open", Error: "Ask the workshop for a new one.",
		})
		return
	default:
		s.log.Error("customer summary", "error", err)
		s.render(w, r, http.StatusInternalServerError, "status", pageData{
			Locale: s.shopLocale(r), Title: "Something went wrong", Error: "Try again shortly.",
		})
		return
	}
	s.render(w, r, http.StatusOK, "summarydoc", pageData{
		Locale: s.shopLocale(r), Title: "Repair summary", Summary: sum, SummaryShop: shop, ShareToken: r.PathValue("token"),
	})
}
