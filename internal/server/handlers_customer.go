package server

import (
	"errors"
	"net/http"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// handleCustomerForm is where a job gets somebody to bill.
//
// On the job rather than on its own screen, because that is when the question
// comes up: the car is in, the work is priced, and now the front desk needs a
// name. A customer list of its own is a place to go and tidy; this is the
// place the work actually stops without it.
func (s *Server) handleCustomerForm(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")

	job, _, err := workshop.JobByID(r.Context(), s.pool, session.Scope, id)
	if errors.Is(err, workshop.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.log.Error("read job", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "Try again.")
		return
	}
	if !session.Scope.Role.SeesCustomerPersonalData() {
		s.renderError(w, r, http.StatusForbidden, "Not for your role",
			"Who is paying is a conversation at the counter.")
		return
	}

	query := r.URL.Query().Get("q")
	found, err := workshop.FindCustomers(r.Context(), s.pool, session.Scope, query)
	if err != nil {
		s.log.Error("find customers", "error", err)
	}
	contact, _ := workshop.ContactFor(r.Context(), s.pool, session.Scope, id)

	data := pageData{
		Title:         "Who is paying",
		Session:       session,
		Job:           job,
		Contact:       contact,
		Customers:     found,
		CustomerQuery: query,
	}
	// The search is swapped in on its own; the rest of the page does not move
	// under somebody who is typing.
	if r.Header.Get("HX-Request") != "" {
		s.renderPartial(w, r, "customer", "results", data)
		return
	}
	s.render(w, r, http.StatusOK, "customer", data)
}

// handleSaveCustomer assigns an existing customer, or creates one and assigns
// it, depending on which half of the form was filled in.
func (s *Server) handleSaveCustomer(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")

	customerID := r.FormValue("customer_id")
	if customerID == "" {
		created, err := workshop.CreateCustomer(r.Context(), s.pool, session.Scope, workshop.NewCustomer{
			Kind:         r.FormValue("kind"),
			Name:         r.FormValue("name"),
			ContactName:  r.FormValue("contact_name"),
			OrgNumber:    r.FormValue("org_number"),
			VATNumber:    r.FormValue("vat_number"),
			Phone:        r.FormValue("phone"),
			Email:        r.FormValue("email"),
			AddressLine1: r.FormValue("address_line1"),
			AddressLine2: r.FormValue("address_line2"),
			PostalCode:   r.FormValue("postal_code"),
			City:         r.FormValue("city"),
		})
		if s.customerError(w, r, err) {
			return
		}
		customerID = created
	}

	err := workshop.AssignCustomer(r.Context(), s.pool, session.Scope, id, customerID,
		r.FormValue("also_owner") == "yes")
	if s.customerError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
}

func (s *Server) customerError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, access.ErrForbidden):
		s.renderError(w, r, http.StatusForbidden, "Not for your role", "")
	case errors.Is(err, workshop.ErrNotFound):
		http.NotFound(w, r)
	case errors.Is(err, workshop.ErrInvalid):
		s.renderError(w, r, http.StatusBadRequest, "Not saved", trimInvalid(err))
	default:
		s.log.Error("customer", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "Try again.")
	}
	return true
}
