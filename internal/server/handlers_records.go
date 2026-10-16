package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// The customer register and the corrections to customers and vehicles. The
// rules live in the workshop package; these hand the form over and go back to
// the record.

func (s *Server) handleCustomers(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	found, err := workshop.BrowseCustomers(r.Context(), s.pool, session.Scope, query)
	if s.handoverError(w, r, err) {
		return
	}
	s.render(w, r, http.StatusOK, "customers", pageData{
		Title: "Customers", Session: session, Customers: found, CustomerQuery: query,
	})
}

func (s *Server) handleCustomerCard(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	card, err := workshop.CustomerByID(r.Context(), s.pool, session.Scope, r.PathValue("id"))
	if s.handoverError(w, r, err) {
		return
	}
	s.render(w, r, http.StatusOK, "customercard", pageData{
		Title: card.Name, Session: session, CustomerCard: card, Saved: r.URL.Query().Get("saved") != "",
	})
}

func (s *Server) handleEditCustomer(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	rev, _ := strconv.Atoi(r.FormValue("revision"))
	_, err := workshop.EditCustomer(r.Context(), s.pool, session.Scope, id, rev, workshop.CustomerEdit{
		Name: r.FormValue("name"), ContactName: r.FormValue("contact_name"),
		OrgNumber: r.FormValue("org_number"), VATNumber: r.FormValue("vat_number"),
		Phone: r.FormValue("phone"), Email: r.FormValue("email"),
		AddressLine1: r.FormValue("address_line1"), AddressLine2: r.FormValue("address_line2"),
		PostalCode: r.FormValue("postal_code"), City: r.FormValue("city"),
		Reason: r.FormValue("reason"),
	})
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/customers/"+id+"?saved=1", http.StatusSeeOther)
}

func (s *Server) handleAddCustomerPerson(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	err := workshop.AddCustomerPerson(r.Context(), s.pool, session.Scope, id,
		r.FormValue("name"), r.FormValue("title"), r.FormValue("phone"), r.FormValue("email"))
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/customers/"+id, http.StatusSeeOther)
}

func (s *Server) handleRemoveCustomerPerson(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	err := workshop.RemoveCustomerPerson(r.Context(), s.pool, session.Scope, id, r.PathValue("row"))
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/customers/"+id, http.StatusSeeOther)
}

func (s *Server) handleEditVehicle(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	rev, _ := strconv.Atoi(r.FormValue("revision"))
	e := workshop.VehicleEdit{
		VIN: r.FormValue("vin"), Make: r.FormValue("make"), Model: r.FormValue("model"),
		Engine: r.FormValue("engine"), Label: r.FormValue("label"), Reason: r.FormValue("reason"),
	}
	if y := strings.TrimSpace(r.FormValue("model_year")); y != "" {
		n, err := strconv.Atoi(y)
		if err != nil || n < -32768 || n > 32767 {
			s.renderError(w, r, http.StatusBadRequest, "Not accepted", "That is not a model year.")
			return
		}
		year := int16(n)
		e.ModelYear = &year
	}
	_, err := workshop.EditVehicle(r.Context(), s.pool, session.Scope, id, rev, e)
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/vehicles/"+id, http.StatusSeeOther)
}

// dateField reads a date input as the start of that day in the shop's zone;
// empty means now.
func (s *Server) dateField(r *http.Request, name string) (time.Time, bool) {
	v := strings.TrimSpace(r.FormValue(name))
	if v == "" {
		return time.Time{}, true
	}
	loc := s.shopLocation(r)
	t, err := time.ParseInLocation("2006-01-02", v, loc)
	if err != nil {
		return time.Time{}, false
	}
	// Noon, so a date typed for today is not in the future and not before the
	// morning's own entries.
	now := time.Now()
	t = t.Add(12 * time.Hour)
	if t.After(now) {
		t = now
	}
	return t, true
}

func (s *Server) handleVehicleRegistration(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	var err error
	if r.FormValue("mode") == "change" {
		from, ok := s.dateField(r, "from")
		if !ok {
			s.renderError(w, r, http.StatusBadRequest, "Not accepted", "That is not a date.")
			return
		}
		err = workshop.ChangeRegistration(r.Context(), s.pool, session.Scope, id, r.FormValue("registration"), from, r.FormValue("reason"))
	} else {
		err = workshop.CorrectRegistration(r.Context(), s.pool, session.Scope, id, r.FormValue("registration"), r.FormValue("reason"))
	}
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/vehicles/"+id, http.StatusSeeOther)
}

func (s *Server) handleSellVehicle(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	from, ok := s.dateField(r, "from")
	if !ok {
		s.renderError(w, r, http.StatusBadRequest, "Not accepted", "That is not a date.")
		return
	}
	err := workshop.SellVehicle(r.Context(), s.pool, session.Scope, id, r.FormValue("customer_id"), from, r.FormValue("reason"))
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/vehicles/"+id, http.StatusSeeOther)
}

// handleSellForm is the first step of a sale: find the new owner, or say that
// nobody is known yet.
func (s *Server) handleSellForm(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	vehicle, err := workshop.VehicleByID(r.Context(), s.pool, session.Scope, r.PathValue("id"))
	if s.handoverError(w, r, err) {
		return
	}
	if !session.Scope.Role.SeesCustomerPersonalData() {
		s.renderError(w, r, http.StatusForbidden, "Not for your role", "")
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	found, err := workshop.FindCustomers(r.Context(), s.pool, session.Scope, query)
	if err != nil {
		s.log.Error("find customers", "error", err)
	}
	s.render(w, r, http.StatusOK, "sell", pageData{
		Title: "Sold", Session: session, Vehicle: vehicle, Customers: found, CustomerQuery: query,
		Today: time.Now().In(s.shopLocation(r)).Format("2006-01-02"),
	})
}
