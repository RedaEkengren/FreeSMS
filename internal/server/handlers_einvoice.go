package server

import (
	"errors"
	"net/http"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// handleEInvoice gives an issued invoice as a Peppol BIS Billing 3.0 file, or
// says what it was issued without. It never gives half of one.
func (s *Server) handleEInvoice(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	body, missing, err := workshop.EInvoice(r.Context(), s.pool, session.Scope, id)
	switch {
	case errors.Is(err, access.ErrForbidden):
		s.renderError(w, r, http.StatusForbidden, "Not for your role", "An invoice is the front desk's business.")
		return
	case errors.Is(err, workshop.ErrNotFound):
		s.renderError(w, r, http.StatusNotFound, "Not found", "No such invoice.")
		return
	case err != nil:
		s.log.Error("write the electronic invoice", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "Try again.")
		return
	}
	if len(missing) > 0 {
		doc, err := workshop.DocumentByID(r.Context(), s.pool, session.Scope, id)
		if err != nil {
			s.log.Error("read invoice", "error", err)
			s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "Try again.")
			return
		}
		s.render(w, r, http.StatusConflict, "einvoice", pageData{
			Title: "Electronic invoice", Session: session, Document: doc, Missing: missing,
		})
		return
	}
	doc, err := workshop.DocumentByID(r.Context(), s.pool, session.Scope, id)
	if err != nil {
		s.log.Error("read invoice", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "Try again.")
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+doc.Reference()+`.xml"`)
	_, _ = w.Write(body)
}
