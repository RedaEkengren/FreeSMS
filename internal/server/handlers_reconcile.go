package server

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Reconciliation with the bank (#152): the statement's lines, typed or
// imported, matched to the payments recorded, for whoever runs the shop.
// Core: money moves whatever modules the shop runs.

// reconcileData is the reconciliation page's.
type reconcileData struct {
	Open    workshop.Reconciliation
	Matches []workshop.Match
}

// Candidates are the unmatched payments of exactly a line's amount: what
// it could be.
func (d reconcileData) Candidates(l workshop.StatementLine) []workshop.Payment {
	var out []workshop.Payment
	for _, p := range d.Open.Payments {
		if p.AmountMinor == l.AmountMinor {
			out = append(out, p)
		}
	}
	return out
}

func (s *Server) handleReconcile(w http.ResponseWriter, r *http.Request) {
	s.reconcilePage(w, r, http.StatusOK, "")
}

func (s *Server) reconcilePage(w http.ResponseWriter, r *http.Request, status int, msg string) {
	session := sessionFrom(r.Context())
	var d reconcileData
	var err error
	if d.Open, err = workshop.Unreconciled(r.Context(), s.pool, session.Scope); s.handoverError(w, r, err) {
		return
	}
	if d.Matches, err = workshop.Matches(r.Context(), s.pool, session.Scope, 50); s.handoverError(w, r, err) {
		return
	}
	s.render(w, r, status, "reconcile", pageData{Title: "Reconciliation", Session: session, Reconcile: &d, Error: msg})
}

// handleReconcileLine records a line of the statement typed in.
func (s *Server) handleReconcileLine(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	day, err := time.ParseInLocation("2006-01-02", r.FormValue("booked_on"), s.shopLocation(r))
	amount, err2 := parseMinorSigned(strings.TrimSpace(r.FormValue("amount")))
	if err != nil || err2 != nil || amount == 0 {
		s.reconcilePage(w, r, http.StatusBadRequest, "Use the picker for the day, and say the amount in kronor, negative going out.")
		return
	}
	_, err = workshop.AddStatementLines(r.Context(), s.pool, session.Scope, "typed",
		[]workshop.StatementLine{{BookedOn: day, AmountMinor: amount, Reference: r.FormValue("reference")}})
	s.reconcileDone(w, r, err)
}

// handleReconcileImport records the lines of a statement file.
func (s *Server) handleReconcileImport(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	r.Body = http.MaxBytesReader(w, r.Body, 3<<20)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		s.reconcilePage(w, r, http.StatusBadRequest, "The file was too large, or the upload was interrupted.")
		return
	}
	defer r.MultipartForm.RemoveAll()
	f, _, err := r.FormFile("file")
	if err != nil {
		s.reconcilePage(w, r, http.StatusBadRequest, "Nothing was attached.")
		return
	}
	defer f.Close()
	lines, err := workshop.ParseStatementCSV(io.Reader(f))
	if err == nil {
		_, err = workshop.AddStatementLines(r.Context(), s.pool, session.Scope, "imported", lines)
	}
	s.reconcileDone(w, r, err)
}

// handleReconcileMatch matches a payment to a line, or undoes a match.
func (s *Server) handleReconcileMatch(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	if r.FormValue("undo") != "" {
		s.reconcileDone(w, r, workshop.UndoMatch(r.Context(), s.pool, session.Scope, r.FormValue("payment")))
		return
	}
	s.reconcileDone(w, r, workshop.MatchPayment(r.Context(), s.pool, session.Scope, r.FormValue("payment"), r.FormValue("line")))
}

func (s *Server) reconcileDone(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, workshop.ErrInvalid) {
		s.reconcilePage(w, r, http.StatusBadRequest, trimInvalid(err))
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/reconcile", http.StatusSeeOther)
}
