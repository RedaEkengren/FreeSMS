package server

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Merging two records that are one. The page shows what would move before
// anything does, and the form carries the versions it was shown against.

func (s *Server) handleCustomerMergeForm(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	card, err := workshop.CustomerByID(r.Context(), s.pool, session.Scope, id)
	if s.handoverError(w, r, err) {
		return
	}
	data := pageData{Title: "Merge", Session: session, MergeKind: "customer", CustomerCard: card}
	with := strings.TrimSpace(r.URL.Query().Get("with"))
	if with == "" {
		query := strings.TrimSpace(r.URL.Query().Get("q"))
		data.CustomerQuery = query
		if query != "" {
			data.Customers, _ = workshop.FindCustomers(r.Context(), s.pool, session.Scope, query)
		}
		data.Candidates, _ = workshop.LikelySame(r.Context(), s.pool, session.Scope, id)
		s.render(w, r, http.StatusOK, "merge", data)
		return
	}
	preview, err := workshop.PreviewCustomerMerge(r.Context(), s.pool, session.Scope, id, with)
	if s.handoverError(w, r, err) {
		return
	}
	data.CustomerMerge, data.MergeWith = preview, with
	s.render(w, r, http.StatusOK, "merge", data)
}

func (s *Server) handleCustomerMerge(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	kr, _ := strconv.Atoi(r.FormValue("keep_revision"))
	dr, _ := strconv.Atoi(r.FormValue("drop_revision"))
	_, err := workshop.MergeCustomers(r.Context(), s.pool, session.Scope, id, r.FormValue("drop"), kr, dr, r.FormValue("reason"))
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/customers/"+id+"?saved=1", http.StatusSeeOther)
}

func (s *Server) handleVehicleMergeForm(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	vehicle, err := workshop.VehicleByID(r.Context(), s.pool, session.Scope, id)
	if s.handoverError(w, r, err) {
		return
	}
	data := pageData{Title: "Merge", Session: session, MergeKind: "vehicle", Vehicle: vehicle}
	with := strings.TrimSpace(r.URL.Query().Get("with"))
	if with == "" {
		query := strings.TrimSpace(r.URL.Query().Get("q"))
		data.Query = query
		if query != "" {
			data.Results, _ = workshop.Search(r.Context(), s.pool, session.Scope, query)
		}
		s.render(w, r, http.StatusOK, "merge", data)
		return
	}
	preview, err := workshop.PreviewVehicleMerge(r.Context(), s.pool, session.Scope, id, with)
	if s.handoverError(w, r, err) {
		return
	}
	data.VehicleMerge, data.MergeWith = preview, with
	s.render(w, r, http.StatusOK, "merge", data)
}

func (s *Server) handleVehicleMerge(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	kr, _ := strconv.Atoi(r.FormValue("keep_revision"))
	dr, _ := strconv.Atoi(r.FormValue("drop_revision"))
	_, err := workshop.MergeVehicles(r.Context(), s.pool, session.Scope, id, r.FormValue("drop"), kr, dr, r.FormValue("reason"))
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/vehicles/"+id, http.StatusSeeOther)
}
