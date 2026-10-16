package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Correcting an issued invoice: credit some of it, see what that comes to
// first, then issue it; or take all of it back and bill the right buyer.

// creditForm reads the credit page's fields. Nothing filled in is not
// "everything": taking all of it back is something you tick.
func (s *Server) creditForm(r *http.Request, lines []workshop.Eligible) (workshop.CreditRequest, string) {
	req := workshop.CreditRequest{Reason: r.FormValue("reason"), Key: r.FormValue("key")}
	if r.FormValue("everything") != "" {
		return req, ""
	}
	for _, l := range lines {
		v := strings.TrimSpace(r.FormValue("qty_" + strconv.Itoa(l.Position)))
		if v == "" {
			continue
		}
		q, err := parseScaled(v, 3)
		if err != nil || q <= 0 {
			return req, "Write the quantity as a number: 1,5 or 1.5."
		}
		req.Lines = append(req.Lines, workshop.CreditLine{Position: l.Position, QuantityMilli: q})
	}
	for i := 1; i <= 2; i++ {
		n := strconv.Itoa(i)
		desc := strings.TrimSpace(r.FormValue("adj_desc_" + n))
		amount := strings.TrimSpace(r.FormValue("adj_net_" + n))
		if desc == "" && amount == "" {
			continue
		}
		net, err := parseMinorUnits(amount)
		if err != nil || net <= 0 {
			return req, "Write the amount as 150 or 150,50."
		}
		rate := 2500
		if v := strings.TrimSpace(r.FormValue("adj_vat_" + n)); v != "" {
			pct, err := parseScaled(v, 2)
			if err != nil {
				return req, "Write the VAT rate as a percentage, like 25."
			}
			rate = int(pct)
		}
		req.Adjustments = append(req.Adjustments, workshop.Adjustment{Description: desc, NetMinor: net, VATRateBasis: rate})
	}
	if len(req.Lines) == 0 && len(req.Adjustments) == 0 {
		return req, "Pick what to credit, or tick that everything that is left should be."
	}
	return req, ""
}

func (s *Server) handleCreditForm(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	doc, err := workshop.DocumentByID(r.Context(), s.pool, session.Scope, id)
	if s.handoverError(w, r, err) {
		return
	}
	lines, err := workshop.EligibleLines(r.Context(), s.pool, session.Scope, id)
	if s.handoverError(w, r, err) {
		return
	}
	s.render(w, r, http.StatusOK, "credit", pageData{
		Title: "Credit", Session: session, Document: doc, Eligible: lines, CreditKey: newFormKey(),
	})
}

func (s *Server) handleIssueCredit(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	doc, err := workshop.DocumentByID(r.Context(), s.pool, session.Scope, id)
	if s.handoverError(w, r, err) {
		return
	}
	lines, err := workshop.EligibleLines(r.Context(), s.pool, session.Scope, id)
	if s.handoverError(w, r, err) {
		return
	}
	req, problem := s.creditForm(r, lines)
	show := func(status int, preview *workshop.CreditPreview, msg string) {
		s.render(w, r, status, "credit", pageData{
			Title: "Credit", Session: session, Document: doc, Eligible: lines, CreditKey: req.Key,
			CreditPreview: preview, CreditForm: r.Form, Error: msg,
		})
	}
	if req.Key == "" {
		req.Key = newFormKey()
	}
	if problem != "" {
		show(http.StatusBadRequest, nil, problem)
		return
	}
	if r.FormValue("confirm") != "1" {
		preview, err := workshop.PreviewCredit(r.Context(), s.pool, session.Scope, id, req)
		if errors.Is(err, workshop.ErrInvalid) {
			show(http.StatusBadRequest, nil, trimInvalid(err))
			return
		}
		if s.handoverError(w, r, err) {
			return
		}
		show(http.StatusOK, &preview, "")
		return
	}
	note, err := workshop.IssueCredit(r.Context(), s.pool, session.Scope, id, req)
	if errors.Is(err, workshop.ErrInvalid) {
		show(http.StatusBadRequest, nil, trimInvalid(err))
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/invoices/"+note.ID, http.StatusSeeOther)
}

func (s *Server) handleRebillForm(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	doc, err := workshop.DocumentByID(r.Context(), s.pool, session.Scope, id)
	if s.handoverError(w, r, err) {
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	found, err := workshop.FindCustomers(r.Context(), s.pool, session.Scope, query)
	if err != nil {
		s.log.Error("find customers", "error", err)
	}
	s.render(w, r, http.StatusOK, "rebill", pageData{
		Title: "Wrong buyer", Session: session, Document: doc, Customers: found, CustomerQuery: query, CreditKey: newFormKey(),
	})
}

func (s *Server) handleRebill(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	repl, err := workshop.RebillTo(r.Context(), s.pool, session.Scope, id, workshop.RebillRequest{
		CustomerID: r.FormValue("customer_id"), Reason: r.FormValue("reason"), Key: r.FormValue("key"),
	})
	if errors.Is(err, workshop.ErrInvalid) {
		s.renderError(w, r, http.StatusBadRequest, "Not rebilled", trimInvalid(err))
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/invoices/"+repl.ID, http.StatusSeeOther)
}

// newFormKey is the key a credit form carries, so that sending it twice is
// one credit and not two.
func newFormKey() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
