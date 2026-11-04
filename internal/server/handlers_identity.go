package server

import (
	"errors"
	"net/http"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A party's identity checks (#153): that a check was made, by whom and when,
// for whoever may see the party; the value, masked or in full, only for a
// role that reads identity records, and each read an audit event. Core:
// the checks belong to the person, whichever module needed them.

// identityData is the identity page's.
type identityData struct {
	Party   workshop.Party
	Records []workshop.IdentityRecord
	// The purposes this person may record for, in this shop's modules.
	Purposes []workshop.IdentityPurpose
	// Reads is whether this person may read values and record checks.
	Reads bool
	// Shown is the one value read in this response, by its record: never
	// kept, never in a URL.
	ShownID, Shown string
}

// IdentityKindName is what a kind of record is called on a page.
func (identityData) IdentityKindName(kind string) string {
	return map[string]string{workshop.IdentityNumber: "Personal identity number",
		workshop.IdentityLicence: "Driving licence check", workshop.IdentityDocument: "ID document check"}[kind]
}

// IdentityPurposeName is what a purpose is called on a page.
func (identityData) IdentityPurposeName(purpose string) string {
	return map[string]string{"aml_due_diligence": "Money-laundering due diligence",
		"ownership_change": "Change of ownership", "licence_check": "Licence check before a rental"}[purpose]
}

// IdentityBasisName is what a legal basis is called on a page.
func (identityData) IdentityBasisName(basis string) string {
	return map[string]string{"legal_obligation": "a legal obligation", "contract": "a contract",
		"legitimate_interest": "a legitimate interest", "consent": "consent"}[basis]
}

func (s *Server) handleIdentity(w http.ResponseWriter, r *http.Request) {
	s.identityPage(w, r, http.StatusOK, "", "", "")
}

func (s *Server) identityPage(w http.ResponseWriter, r *http.Request, status int, msg, shownID, shown string) {
	session := sessionFrom(r.Context())
	scope := session.Scope
	d := identityData{Reads: scope.Role.SeesIdentityRecords(), ShownID: shownID, Shown: shown}
	var err error
	if d.Party, err = workshop.PartyByID(r.Context(), s.pool, scope, r.PathValue("id")); s.handoverError(w, r, err) {
		return
	}
	if d.Records, err = workshop.IdentityRecordsFor(r.Context(), s.pool, scope, d.Party.ID); s.handoverError(w, r, err) {
		return
	}
	if d.Reads {
		for _, p := range workshop.IdentityPurposes {
			if p.Records(scope.Role) && scope.Require(p.Module) == nil {
				d.Purposes = append(d.Purposes, p)
			}
		}
	}
	// A value on the page is not to be kept by anything between here and
	// the reader, nor by the browser's history.
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, r, status, "identity", pageData{Title: "Identity checks", Session: session, Identity: &d, Error: msg})
}

// identityError is handoverError with the one failure of its own: no key.
func (s *Server) identityError(w http.ResponseWriter, r *http.Request, err error) bool {
	if errors.Is(err, workshop.ErrIdentityKey) {
		s.renderError(w, r, http.StatusServiceUnavailable, "No key for identity values",
			"The key identity values are sealed with is not configured on this server, or is not the one this value was sealed with. Nothing is shown in the clear instead.")
		return true
	}
	return s.handoverError(w, r, err)
}

// handleRecordIdentity records a check: the value sealed, or a reference.
func (s *Server) handleRecordIdentity(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	party := r.PathValue("id")
	_, err := workshop.RecordIdentity(r.Context(), s.pool, session.Scope, s.identityKeys, workshop.NewIdentityRecord{
		PartyID: party, Kind: r.FormValue("kind"), Purpose: r.FormValue("purpose"),
		Value: r.FormValue("value"), Reference: r.FormValue("reference"),
	})
	if errors.Is(err, workshop.ErrInvalid) {
		s.identityPage(w, r, http.StatusBadRequest, trimInvalid(err), "", "")
		return
	}
	if s.identityError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/parties/"+party+"/identity", http.StatusSeeOther)
}

// handleShowIdentity reads one value, masked or in full, onto this
// response only. A POST: a read is an event, and a link prefetched or
// followed twice must not make one.
func (s *Server) handleShowIdentity(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	record := r.PathValue("record")
	value, err := workshop.ReadIdentityValue(r.Context(), s.pool, session.Scope, s.identityKeys, record, r.FormValue("full") != "")
	if s.identityError(w, r, err) {
		return
	}
	s.identityPage(w, r, http.StatusOK, "", record, value)
}

// handleEndIdentity ends a record's purpose early and removes its value.
func (s *Server) handleEndIdentity(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	err := workshop.EndIdentityPurpose(r.Context(), s.pool, session.Scope, r.PathValue("record"), r.FormValue("reason"))
	if errors.Is(err, workshop.ErrInvalid) {
		s.identityPage(w, r, http.StatusBadRequest, trimInvalid(err), "", "")
		return
	}
	if s.identityError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/parties/"+r.PathValue("id")+"/identity", http.StatusSeeOther)
}

// Kinds are the kinds of record there are, for the form.
func (identityData) Kinds() []string {
	return []string{workshop.IdentityNumber, workshop.IdentityLicence, workshop.IdentityDocument}
}
