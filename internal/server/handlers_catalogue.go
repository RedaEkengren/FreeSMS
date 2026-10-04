package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// handlePartForm shows a part to change, or an empty one to add.
func (s *Server) handlePartForm(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	// An empty form reads nothing, so nothing below would refuse a role that
	// may not keep the catalogue. Said here, as the workshop functions say it.
	if !session.Scope.Role.SeesParts() {
		s.handoverError(w, r, access.ErrForbidden)
		return
	}
	e := workshop.CatalogueEntry{Unit: "each", Active: true}
	if id := r.PathValue("id"); id != "" {
		var err error
		if e, err = workshop.CatalogueEntryByID(r.Context(), s.pool, session.Scope, id); s.handoverError(w, r, err) {
			return
		}
	}
	s.renderPart(w, r, http.StatusOK, e, "")
}

// handleSavePart adds a part, or changes the one in the path.
func (s *Server) handleSavePart(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	e := workshop.CatalogueEntry{
		ID:       r.PathValue("id"),
		Number:   r.FormValue("number"),
		Name:     r.FormValue("name"),
		Unit:     r.FormValue("unit"),
		Location: r.FormValue("location"),
		Codes:    workshop.ParseCodes(r.FormValue("codes")),
		Active:   true,
	}
	// Each problem goes back with what was typed, rather than to an error
	// page: a part with eight codes typed out is not typed out twice.
	var problem string
	optionalMoney := func(field string) *int64 {
		v := strings.TrimSpace(r.FormValue(field))
		if v == "" {
			return nil
		}
		n, err := parseMinorUnits(v)
		if err != nil && problem == "" {
			problem = "That amount did not parse: " + v
		}
		return &n
	}
	e.CostMinor = optionalMoney("cost")
	e.PriceMinor = optionalMoney("price")
	if v := strings.TrimSpace(r.FormValue("minimum")); v != "" {
		n, err := parseScaled(v, 3)
		if err != nil && problem == "" {
			problem = "That minimum did not parse: " + v
		}
		e.MinimumMilli = n
	}
	if problem != "" {
		s.renderPart(w, r, http.StatusBadRequest, e, problem)
		return
	}

	id, err := workshop.SavePart(r.Context(), s.pool, session.Scope, e)
	if errors.Is(err, workshop.ErrInvalid) {
		s.renderPart(w, r, http.StatusBadRequest, e, trimInvalid(err))
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/stock/parts/"+id, http.StatusSeeOther)
}

// handlePartActive retires a part or brings one back.
func (s *Server) handlePartActive(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	if err := workshop.SetPartActive(r.Context(), s.pool, session.Scope, id,
		r.FormValue("active") == "yes"); s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/stock/parts/"+id, http.StatusSeeOther)
}

func (s *Server) renderPart(w http.ResponseWriter, r *http.Request, status int, e workshop.CatalogueEntry, problem string) {
	title := "Add a part"
	if e.ID != "" {
		title = e.Number
	}
	s.render(w, r, status, "part", pageData{
		Title:     title,
		Session:   sessionFrom(r.Context()),
		Catalogue: e,
		Units:     workshop.PartUnits,
		Error:     problem,
	})
}
