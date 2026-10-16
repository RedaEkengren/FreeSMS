package server

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Starting from what the shop already has: a file in, a preview, and a batch
// that is applied or thrown away. The rules are in the workshop package.

const maxImportUpload = 5 << 20

func (s *Server) handleImportPage(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	batches, err := workshop.ImportBatches(r.Context(), s.pool, session.Scope)
	if s.handoverError(w, r, err) {
		return
	}
	var kinds []workshop.Option
	for _, k := range workshop.ImportKinds {
		kinds = append(kinds, workshop.Option{Value: k, Label: workshop.ImportKindLabel(k)})
	}
	s.render(w, r, http.StatusOK, "import", pageData{Title: "Import", Session: session, Batches: batches, Positions: kinds})
}

func (s *Server) handleImportTemplate(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	if !session.Scope.Role.RunsTheShop() {
		s.renderError(w, r, http.StatusForbidden, "Not for your role", "")
		return
	}
	kind := r.PathValue("kind")
	body, ok := workshop.ImportTemplate(kind)
	if !ok {
		s.renderError(w, r, http.StatusNotFound, "Not found", "There is no such template.")
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="freesms-`+kind+`.csv"`)
	// A byte order mark, so a spreadsheet reads å, ä and ö as they are.
	_, _ = io.WriteString(w, "\xef\xbb\xbf"+body)
}

func (s *Server) handleImportUpload(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	if !session.Scope.Role.RunsTheShop() {
		s.renderError(w, r, http.StatusForbidden, "Not for your role", "")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxImportUpload+1<<20)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Not read", "The file could not be read, or is larger than 5 MB.")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Not read", "Choose a file.")
		return
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxImportUpload+1))
	if err != nil || len(raw) > maxImportUpload {
		s.renderError(w, r, http.StatusBadRequest, "Not read", "The file is larger than 5 MB.")
		return
	}
	b, err := workshop.PreviewImport(r.Context(), s.pool, session.Scope, r.FormValue("kind"), header.Filename, raw)
	if errors.Is(err, workshop.ErrInvalid) {
		s.renderError(w, r, http.StatusBadRequest, "That file cannot be imported", trimInvalid(err))
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/import/"+b.ID, http.StatusSeeOther)
}

func (s *Server) handleImportBatch(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	b, err := workshop.ImportBatchByID(r.Context(), s.pool, session.Scope, r.PathValue("id"))
	if s.handoverError(w, r, err) {
		return
	}
	s.render(w, r, http.StatusOK, "importbatch", pageData{Title: "Import", Session: session, Batch: b})
}

func (s *Server) handleImportAct(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	switch r.FormValue("action") {
	case "discard":
		if s.handoverError(w, r, workshop.DiscardImport(r.Context(), s.pool, session.Scope, id)) {
			return
		}
		http.Redirect(w, r, "/import", http.StatusSeeOther)
		return
	case "apply":
		decisions := map[int]workshop.ImportDecision{}
		if err := r.ParseForm(); err != nil {
			s.renderError(w, r, http.StatusBadRequest, "Not read", "")
			return
		}
		for key, vals := range r.PostForm {
			n, ok := strings.CutPrefix(key, "d_")
			if !ok || len(vals) == 0 || vals[0] == "" {
				continue
			}
			row, err := strconv.Atoi(n)
			if err != nil {
				continue
			}
			choice, target, _ := strings.Cut(vals[0], ":")
			decisions[row] = workshop.ImportDecision{Choice: choice, Target: target}
		}
		_, err := workshop.ApplyImport(r.Context(), s.pool, session.Scope, id, decisions, r.FormValue("skip_failed") != "")
		switch {
		case errors.Is(err, workshop.ErrImportApplied):
			// A second press of the button: the report is the answer.
		case errors.Is(err, workshop.ErrInvalid):
			s.renderError(w, r, http.StatusBadRequest, "Not imported", trimInvalid(err))
			return
		default:
			if s.handoverError(w, r, err) {
				return
			}
		}
		http.Redirect(w, r, "/import/"+id, http.StatusSeeOther)
		return
	}
	s.renderError(w, r, http.StatusBadRequest, "Not recorded", "Say what to do.")
}
