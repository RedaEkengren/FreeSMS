package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Leads' pages (#172): the salesperson's list and the form for an enquiry,
// and one lead with what happened to it. What a salesperson may see is
// decided by the operations; the page shows what it is handed.

// leadsData is what the leads pages show.
type leadsData struct {
	Leads   []workshop.Lead
	Lead    workshop.Lead
	Closed  bool
	Sellers []workshop.StaffMember
	Cars    []workshop.StockCar
	// An enquiry from someone with an open lead already: theirs, and what
	// was typed, to add to it or record beside it.
	Same   *workshop.DuplicateLeadError
	Form   map[string]string
	Months int
	Key    string
	Today  string
}

// Statuses are where a lead can be moved by hand: not new, and not won,
// which is the deal's.
func (leadsData) Statuses() []string {
	return []string{workshop.LeadContacted, workshop.LeadTestDriveBooked, workshop.LeadOfferMade, workshop.LeadLost}
}

func (leadsData) Channels() []string    { return workshop.LeadChannels }
func (leadsData) LostReasons() []string { return workshop.LeadLostReasons }
func (leadsData) Hows() []string        { return workshop.LeadContactHows }

// Word is a lead's status, channel, reason or way of contact, as a
// catalogue key.
func (leadsData) Word(key string) string { return workshop.LeadLabel(key) }

func (s *Server) leadRoutes(m routeMux) {
	m.HandleFunc("GET /cars/leads", s.requireSession(s.handleLeads))
	m.HandleFunc("POST /cars/leads", s.requireSession(s.handleLeadCreate))
	m.HandleFunc("POST /cars/leads/settings", s.requireSession(s.handleLeadSettings))
	m.HandleFunc("POST /cars/leads/reassign", s.requireSession(s.handleLeadsReassign))
	m.HandleFunc("GET /cars/leads/{id}", s.requireSession(s.handleLead))
	m.HandleFunc("POST /cars/leads/{id}/contacts", s.requireSession(s.handleLeadContact))
	m.HandleFunc("POST /cars/leads/{id}/status", s.requireSession(s.handleLeadMove))
	m.HandleFunc("POST /cars/leads/{id}/assign", s.requireSession(s.handleLeadAssign))
	m.HandleFunc("POST /cars/leads/{id}/next", s.requireSession(s.handleLeadNext))
}

// leadsPage fills in what the list and its form need.
func (s *Server) leadsPage(w http.ResponseWriter, r *http.Request, status int, d leadsData, msg string) {
	session := sessionFrom(r.Context())
	var err error
	if d.Leads, err = workshop.Leads(r.Context(), s.pool, session.Scope, d.Closed); s.handoverError(w, r, err) {
		return
	}
	if d.Sellers, err = workshop.SellersOfCars(r.Context(), s.pool, session.Scope); s.handoverError(w, r, err) {
		return
	}
	if d.Cars, err = workshop.CarsInStock(r.Context(), s.pool, session.Scope); s.handoverError(w, r, err) {
		return
	}
	if d.Months, err = workshop.LeadErasureMonths(r.Context(), s.pool, session.Scope); s.handoverError(w, r, err) {
		return
	}
	if d.Form == nil {
		d.Form = map[string]string{"salesperson": session.Scope.UserID, "unit": r.URL.Query().Get("unit")}
	}
	if d.Key == "" {
		d.Key = newFormKey()
	}
	s.render(w, r, status, "leads", pageData{Title: "Leads", Session: session, Leads: d, Error: msg})
}

// handleLeads is the salesperson's list: open leads by their next step,
// and the closed ones too when asked.
func (s *Server) handleLeads(w http.ResponseWriter, r *http.Request) {
	s.leadsPage(w, r, http.StatusOK, leadsData{Closed: r.URL.Query().Get("closed") == "1"}, "")
}

// handleLeadCreate records an enquiry. One from somebody with an open lead
// already comes back with theirs, to add it to or record beside it.
func (s *Server) handleLeadCreate(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	form := map[string]string{}
	for _, k := range []string{"name", "phone", "email", "unit", "wishes", "channel", "salesperson", "note"} {
		form[k] = strings.TrimSpace(r.FormValue(k))
	}
	key := strings.TrimSpace(r.FormValue("key"))
	l, err := workshop.CreateLead(r.Context(), s.pool, session.Scope, workshop.NewLead{
		Name: form["name"], Phone: form["phone"], Email: form["email"], StockUnitID: form["unit"], Wishes: form["wishes"],
		Channel: form["channel"], Salesperson: form["salesperson"], Note: form["note"], Key: key,
		Alongside: r.FormValue("alongside") == "1"})
	var same *workshop.DuplicateLeadError
	if errors.As(err, &same) {
		s.leadsPage(w, r, http.StatusConflict, leadsData{Same: same, Form: form, Key: key}, "")
		return
	}
	if errors.Is(err, workshop.ErrInvalid) {
		s.leadsPage(w, r, http.StatusBadRequest, leadsData{Form: form, Key: key}, trimInvalid(err))
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/cars/leads/"+l.ID, http.StatusSeeOther)
}

// handleLead is one lead: who, which car, where it stands, what was said,
// and what to do next.
func (s *Server) handleLead(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	var d leadsData
	var err error
	if d.Lead, err = workshop.LeadByID(r.Context(), s.pool, session.Scope, r.PathValue("id")); s.handoverError(w, r, err) {
		return
	}
	if session.Scope.Role.RunsTheShop() {
		if d.Sellers, err = workshop.SellersOfCars(r.Context(), s.pool, session.Scope); s.handoverError(w, r, err) {
			return
		}
	}
	d.Today = time.Now().In(s.shopLocation(r)).Format(time.DateOnly)
	s.render(w, r, http.StatusOK, "lead", pageData{Title: "Lead", Session: session, Leads: d})
}

func (s *Server) backToLead(w http.ResponseWriter, r *http.Request, err error) {
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/cars/leads/"+r.PathValue("id"), http.StatusSeeOther)
}

func (s *Server) handleLeadContact(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	s.backToLead(w, r, workshop.RecordLeadContact(r.Context(), s.pool, session.Scope, r.PathValue("id"), r.FormValue("how"), r.FormValue("note")))
}

func (s *Server) handleLeadMove(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	s.backToLead(w, r, workshop.MoveLead(r.Context(), s.pool, session.Scope, r.PathValue("id"), r.FormValue("status"),
		r.FormValue("reason"), r.FormValue("note")))
}

func (s *Server) handleLeadAssign(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	s.backToLead(w, r, workshop.AssignLead(r.Context(), s.pool, session.Scope, r.PathValue("id"), r.FormValue("salesperson")))
}

func (s *Server) handleLeadNext(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	on, ok := s.formDate(r, "on")
	if !ok {
		s.renderError(w, r, http.StatusBadRequest, "That date did not parse", "Use the picker.")
		return
	}
	s.backToLead(w, r, workshop.SetLeadNextStep(r.Context(), s.pool, session.Scope, r.PathValue("id"), on, r.FormValue("what")))
}

func (s *Server) handleLeadSettings(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	months, err := strconv.Atoi(strings.TrimSpace(r.FormValue("months")))
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Not accepted", "Say how many months.")
		return
	}
	if s.handoverError(w, r, workshop.SetLeadErasure(r.Context(), s.pool, session.Scope, months)) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/cars/leads", http.StatusSeeOther)
}

func (s *Server) handleLeadsReassign(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	_, err := workshop.ReassignLeads(r.Context(), s.pool, session.Scope, r.FormValue("from"), r.FormValue("to"))
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/cars/leads", http.StatusSeeOther)
}
