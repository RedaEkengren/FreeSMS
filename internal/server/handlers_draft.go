package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/RedaEkengren/RedaSMS/internal/workshop"
)

// handleSaveDraft stores what somebody is typing.
//
// Called as they type, debounced by the browser. There is no Save button to
// miss, which is the point: the complaint this answers is a shop owner losing
// hours of quotes because the system fell over before anybody pressed one.
func (s *Server) handleSaveDraft(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())

	var payload struct {
		Form   string            `json:"form"`
		Fields map[string]string `json:"fields"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&payload); err != nil {
		http.Error(w, "unreadable draft", http.StatusBadRequest)
		return
	}
	if err := workshop.SaveDraft(r.Context(), s.pool, session.Scope, payload.Form, payload.Fields); err != nil {
		s.log.Error("save draft", "error", err)
		http.Error(w, "could not keep the draft", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleLoadDraft returns what was typed and never submitted.
func (s *Server) handleLoadDraft(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	form := strings.TrimSpace(r.URL.Query().Get("form"))

	fields, err := workshop.LoadDraft(r.Context(), s.pool, session.Scope, form)
	if err != nil {
		s.log.Error("load draft", "error", err)
		http.Error(w, "could not read the draft", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(fields)
}

// handleDiscardDraft removes one, which is what submitting means.
func (s *Server) handleDiscardDraft(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	if err := workshop.DiscardDraft(r.Context(), s.pool, session.Scope, r.FormValue("form")); err != nil {
		s.log.Error("discard draft", "error", err)
	}
	w.WriteHeader(http.StatusNoContent)
}
