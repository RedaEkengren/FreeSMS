package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
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

// handleDiscardDraft removes one. The browser calls it once the server has
// acknowledged a submission, for an autosave that arrived after the save.
func (s *Server) handleDiscardDraft(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	if err := workshop.DiscardDraft(r.Context(), s.pool, session.Scope, r.FormValue("form")); err != nil {
		s.log.Error("discard draft", "error", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// savedCookie carries the receipt for a submitted draft back to the browser.
const savedCookie = "freesms_saved"

// submitted is called by a handler that has saved a form, and only then.
//
// The browser used to delete its draft the moment the button was pressed,
// before anybody knew whether the server took it. A validation refusal, an
// error page or a dropped connection then left nothing to recover. Now the
// server, having saved, drops its own copy and names the draft in a cookie
// that lives for a minute; keep.js reads it on the next page and lets its copy
// go. No receipt, no deletion.
//
// The name comes from the form itself, in a field keep.js adds, so this does
// not need to know which forms keep drafts. It is the caller's own draft by
// construction: drafts are scoped to the person signed in.
func (s *Server) submitted(w http.ResponseWriter, r *http.Request) {
	form := strings.TrimSpace(r.FormValue("draft_form"))
	if form == "" || len(form) > 200 {
		return
	}
	session := sessionFrom(r.Context())
	// Detached: the work is saved, so the draft has to go whether or not the
	// client is still waiting for the answer.
	if err := workshop.DiscardDraft(context.WithoutCancel(r.Context()), s.pool, session.Scope, form); err != nil {
		s.log.Error("discard submitted draft", "error", err)
	}

	// Appended to a receipt not yet read, so two submissions sent back to back
	// by the offline queue each get theirs. Dot-separated, with the dots in a
	// name escaped, because a comma or a space would make Go quote the value.
	name := strings.ReplaceAll(url.QueryEscape(form), ".", "%2E")
	value := name
	if prev, err := r.Cookie(savedCookie); err == nil && prev.Value != "" && len(prev.Value) < 1000 {
		value = prev.Value + "." + name
	}
	http.SetCookie(w, &http.Cookie{
		Name:  savedCookie,
		Value: value,
		Path:  "/",
		// Short: it is a message to the next page, not state.
		MaxAge: 60,
		// Not HttpOnly, because the script is who reads it. It says only which
		// of the reader's own forms was saved.
		SameSite: http.SameSiteLaxMode,
		Secure:   s.secureCookies,
	})
}
