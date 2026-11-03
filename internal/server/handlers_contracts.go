package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// handleContract shows a contract (#150) to whoever may read its kind, to
// print or to send a link to.
func (s *Server) handleContract(w http.ResponseWriter, r *http.Request) {
	s.renderContract(w, r, "")
}

// handleContractLink makes the customer's link and shows it, this once: only
// its hash is kept.
func (s *Server) handleContractLink(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	token, err := workshop.CreateDocumentRecordLink(r.Context(), s.pool, session.Scope, r.PathValue("id"))
	if s.contractError(w, r, err) {
		return
	}
	s.renderContract(w, r, strings.TrimRight(s.baseURL, "/")+"/c/"+token)
}

func (s *Server) renderContract(w http.ResponseWriter, r *http.Request, link string) {
	session := sessionFrom(r.Context())
	d, err := workshop.DocumentRecordByID(r.Context(), s.pool, session.Scope, r.PathValue("id"))
	if s.contractError(w, r, err) {
		return
	}
	s.render(w, r, http.StatusOK, "contract", pageData{
		Title: d.Title, Session: session, Contract: d, ContractLink: link,
	})
}

func (s *Server) contractError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, access.ErrForbidden):
		s.renderError(w, r, http.StatusForbidden, "Not for your role", "A contract is for those who made it with the customer.")
	case errors.Is(err, workshop.ErrNotFound):
		s.renderError(w, r, http.StatusNotFound, "Not found", "No such document.")
	default:
		s.fault(w, r, "read contract", err)
	}
	return true
}

// handleCustomerContract is the customer's own copy of a contract, on its
// link. No session: the token is the whole of the authorisation, for one
// document. In the shop's language, as everything the customer reads.
func (s *Server) handleCustomerContract(w http.ResponseWriter, r *http.Request) {
	d, err := workshop.OpenDocumentRecordLink(r.Context(), s.pool, s.shop(), r.PathValue("token"))
	switch {
	case err == nil:
	case errors.Is(err, workshop.ErrShareNotUsable), errors.Is(err, workshop.ErrNotFound):
		s.render(w, r, http.StatusNotFound, "status", pageData{
			Locale: s.shopLocale(r), Title: "This link is no longer open", Error: "Ask the workshop for a new one.",
		})
		return
	default:
		s.log.Error("customer contract", "error", err)
		s.render(w, r, http.StatusInternalServerError, "status", pageData{
			Locale: s.shopLocale(r), Title: "Something went wrong", Error: "Try again shortly.",
		})
		return
	}
	s.render(w, r, http.StatusOK, "contract", pageData{
		Locale: s.shopLocale(r), Title: d.Title, Contract: d, ContractCustomer: true,
	})
}
