package server

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Charges for a vehicle (#198): congestion tax, parking fees and tolls,
// typed or imported with a preview, matched to the rental that had the car
// and passed to it, kept, or paid.

// FineKinds are the kinds a fine form offers.
func (rentalData) FineKinds() []string { return workshop.FineKinds }

// FineKindName is what a kind of fine is called on a page.
func (rentalData) FineKindName(kind string) string {
	return map[string]string{"congestion_tax": "Congestion tax", "parking_control_fee": "Parking control fee",
		"parking_fine": "Parking fine", "toll": "Toll", "other": "Other charge"}[kind]
}

// FineMatchSays is what matching found, as a page says it.
func (rentalData) FineMatchSays(match string) string {
	return map[string]string{workshop.FineMatched: "One rental had the car then",
		workshop.FineOwn:         "No rental had the car then",
		workshop.FineAmbiguous:   "Too near a handover, or more than one rental: choose",
		workshop.FineUnknownUnit: "No vehicle had that registration then"}[match]
}

func (s *Server) handleRentalFines(w http.ResponseWriter, r *http.Request) {
	s.rentalFinesPage(w, r, http.StatusOK, nil, "", "")
}

// rentalFinesPage is the list, with an import's preview and its file when
// there is one.
func (s *Server) rentalFinesPage(w http.ResponseWriter, r *http.Request, status int, preview []workshop.Fine, file, msg string) {
	session := sessionFrom(r.Context())
	fines, err := workshop.Fines(r.Context(), s.pool, session.Scope)
	if s.handoverError(w, r, err) {
		return
	}
	s.render(w, r, status, "rentalfines", pageData{Title: "Charges for vehicles", Session: session, Error: msg,
		Rental: rentalData{Fines: fines, FinePreview: preview, FineFile: file, Now: time.Now()}})
}

// handleRentalFineRecord records one charge typed in.
func (s *Server) handleRentalFineRecord(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	loc := s.shopLocation(r)
	at, err := parseWallClock(r.FormValue("when"), loc, "")
	amount, err2 := parseMinorUnits(strings.TrimSpace(r.FormValue("amount")))
	if err != nil || err2 != nil {
		s.rentalFinesPage(w, r, http.StatusBadRequest, nil, "", "Use the picker for when, and say the amount in kronor.")
		return
	}
	f := workshop.NewFine{Kind: r.FormValue("kind"), Registration: r.FormValue("registration"), OccurredAt: at, AmountMinor: amount,
		Issuer: r.FormValue("issuer"), Reference: r.FormValue("reference")}
	if due, ok := s.formDate(r, "due_on"); !ok {
		s.rentalFinesPage(w, r, http.StatusBadRequest, nil, "", "Use the picker for when, and say the amount in kronor.")
		return
	} else {
		f.DueOn = due
	}
	_, err = workshop.RecordFines(r.Context(), s.pool, session.Scope, "typed", []workshop.NewFine{f})
	if errors.Is(err, workshop.ErrInvalid) {
		s.rentalFinesPage(w, r, http.StatusBadRequest, nil, "", trimInvalid(err))
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/rental/fines", http.StatusSeeOther)
}

// handleRentalFineImport reads a file and shows what it holds and what
// matching says, saving nothing; sent again with save, it records them.
func (s *Server) handleRentalFineImport(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	file := r.FormValue("file_text")
	if file == "" {
		r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			s.rentalFinesPage(w, r, http.StatusBadRequest, nil, "", "The file was too large, or the upload was interrupted.")
			return
		}
		defer r.MultipartForm.RemoveAll()
		f, _, err := r.FormFile("file")
		if err != nil {
			s.rentalFinesPage(w, r, http.StatusBadRequest, nil, "", "Nothing was attached.")
			return
		}
		defer f.Close()
		b, err := io.ReadAll(f)
		if err != nil {
			s.rentalFinesPage(w, r, http.StatusBadRequest, nil, "", "Nothing was attached.")
			return
		}
		file = string(b)
	}
	fines, err := workshop.ParseFinesCSV(strings.NewReader(file), s.shopLocation(r))
	if err != nil {
		s.rentalFinesPage(w, r, http.StatusBadRequest, nil, "", trimInvalid(err))
		return
	}
	if r.FormValue("save") == "" {
		preview, err := workshop.PreviewFines(r.Context(), s.pool, session.Scope, fines)
		if s.handoverError(w, r, err) {
			return
		}
		s.rentalFinesPage(w, r, http.StatusOK, preview, file, "")
		return
	}
	if _, err := workshop.RecordFines(r.Context(), s.pool, session.Scope, "imported", fines); s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/rental/fines", http.StatusSeeOther)
}

// handleRentalFineDecide passes a charge to a rental, keeps it as the
// shop's, or records it paid.
func (s *Server) handleRentalFineDecide(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	var err error
	switch r.PathValue("what") {
	case "renter":
		_, err = workshop.PassFineToRenter(r.Context(), s.pool, session.Scope, id, r.FormValue("booking"))
	case "shop":
		err = workshop.KeepFine(r.Context(), s.pool, session.Scope, id, r.FormValue("reason"))
	case "paid":
		err = workshop.FinePaid(r.Context(), s.pool, session.Scope, id)
	default:
		http.NotFound(w, r)
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/rental/fines", http.StatusSeeOther)
}

// fineRoutes registers the charges for vehicles.
func (s *Server) fineRoutes(m routeMux) {
	m.HandleFunc("GET /rental/fines", s.requireSession(s.handleRentalFines))
	m.HandleFunc("POST /rental/fines", s.requireSession(s.handleRentalFineRecord))
	m.HandleFunc("POST /rental/fines/import", s.requireSession(s.handleRentalFineImport))
	m.HandleFunc("POST /rental/fines/{id}/{what}", s.requireSession(s.handleRentalFineDecide))
}
