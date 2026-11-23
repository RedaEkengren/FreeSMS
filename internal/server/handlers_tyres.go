package server

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// The tyre hotel's sets (#226): all of them for whoever works in the
// workshop, one set with where it has been, and a customer's own with the
// form that takes a new one in.

func (s *Server) handleTyres(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	sets, err := workshop.TyreSetsOf(r.Context(), s.pool, session.Scope, workshop.TyreFilter{})
	if s.handoverError(w, r, err) {
		return
	}
	s.render(w, r, http.StatusOK, "tyres", pageData{Title: "Tyre sets", Session: session, Tyres: sets})
}

func (s *Server) handleTyreSet(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	set, err := workshop.TyreSetByID(r.Context(), s.pool, session.Scope, r.PathValue("id"))
	if s.handoverError(w, r, err) {
		return
	}
	pd := pageData{Title: "A set of tyres", Session: session, TyreSet: set}
	if places, err := workshop.StorageLocations(r.Context(), s.pool, session.Scope); err == nil {
		pd.Places = places
	}
	// The owner's other cars, for moving the set to one of them.
	if set.CustomerID != "" {
		if card, err := workshop.CustomerByID(r.Context(), s.pool, session.Scope, set.CustomerID); err == nil {
			pd.CustomerCard = card
		}
	}
	s.render(w, r, http.StatusOK, "tyreset", pd)
}

func (s *Server) handleTyreSetMove(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	err := workshop.MoveTyreSet(r.Context(), s.pool, session.Scope, r.PathValue("id"), r.FormValue("vehicle"))
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/tyres/"+r.PathValue("id"), http.StatusSeeOther)
}

func (s *Server) handleTyreReplace(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	week, year := formInt(r, "dot_week"), formInt(r, "dot_year")
	err := workshop.ReplaceTyre(r.Context(), s.pool, session.Scope, r.FormValue("tyre"), workshop.NewTyre{
		Position: r.FormValue("position"), Dimension: r.FormValue("dimension"), DotWeek: week, DotYear: year})
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/tyres/"+r.PathValue("id"), http.StatusSeeOther)
}

func formInt(r *http.Request, key string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(r.FormValue(key)))
	return n
}

// handleCustomerTyres is one customer's sets and the form for a new one.
func (s *Server) handleCustomerTyres(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	card, err := workshop.CustomerByID(r.Context(), s.pool, session.Scope, r.PathValue("id"))
	if s.handoverError(w, r, err) {
		return
	}
	sets, err := workshop.TyreSetsOf(r.Context(), s.pool, session.Scope, workshop.TyreFilter{CustomerID: card.ID})
	if s.handoverError(w, r, err) {
		return
	}
	s.render(w, r, http.StatusOK, "customertyres", pageData{Title: "Tyre sets", Session: session, CustomerCard: card, Tyres: sets})
}

// handleCreateTyreSet takes a set in: a dimension for the front and, when the
// rear differs, for the rear, the same date of manufacture on all, and a spare
// when it has a dimension.
func (s *Server) handleCreateTyreSet(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	front := strings.TrimSpace(r.FormValue("front"))
	rear := strings.TrimSpace(r.FormValue("rear"))
	if rear == "" {
		rear = front
	}
	week, year := formInt(r, "dot_week"), formInt(r, "dot_year")
	tyre := func(pos, dim string) workshop.NewTyre {
		return workshop.NewTyre{Position: pos, Dimension: dim, DotWeek: week, DotYear: year}
	}
	n := workshop.NewTyreSet{
		CustomerID: r.PathValue("id"), VehicleID: r.FormValue("vehicle"), Season: r.FormValue("season"),
		Brand: r.FormValue("brand"), Model: r.FormValue("model"), Rims: r.FormValue("rims"),
		TPMS: r.FormValue("tpms") != "", Fasteners: r.FormValue("fasteners") != "", Note: r.FormValue("note"),
		Tyres: []workshop.NewTyre{tyre("front_left", front), tyre("front_right", front), tyre("rear_left", rear), tyre("rear_right", rear)},
	}
	if spare := strings.TrimSpace(r.FormValue("spare")); spare != "" {
		n.Tyres = append(n.Tyres, tyre("spare", spare))
	}
	id, err := workshop.CreateTyreSet(r.Context(), s.pool, session.Scope, n)
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/tyres/"+id, http.StatusSeeOther)
}

// handleStorage is the places with what is in each, and for whoever runs the
// shop the form that makes a new one.
func (s *Server) handleStorage(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	places, err := workshop.StorageLocations(r.Context(), s.pool, session.Scope)
	if s.handoverError(w, r, err) {
		return
	}
	s.render(w, r, http.StatusOK, "storage", pageData{Title: "Storage places", Session: session, Places: places})
}

func (s *Server) handleCreateStorage(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	_, err := workshop.CreateStorageLocation(r.Context(), s.pool, session.Scope, r.FormValue("name"), formInt(r, "capacity"))
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/storage", http.StatusSeeOther)
}

func (s *Server) handleStorageUse(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	err := workshop.SetStorageLocationInUse(r.Context(), s.pool, session.Scope, r.PathValue("id"), r.FormValue("use") == "1")
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/storage", http.StatusSeeOther)
}

// handlePlaceTyreSet puts a set in a place, or takes it out of storage when no
// place is given.
func (s *Server) handlePlaceTyreSet(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	var err error
	if place := r.FormValue("place"); place == "" {
		err = workshop.TakeTyreSetOutOfStorage(r.Context(), s.pool, session.Scope, r.PathValue("id"))
	} else {
		err = workshop.PlaceTyreSet(r.Context(), s.pool, session.Scope, r.PathValue("id"), place)
	}
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/tyres/"+r.PathValue("id"), http.StatusSeeOther)
}
