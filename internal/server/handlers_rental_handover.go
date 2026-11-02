package server

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/RedaEkengren/FreeSMS/internal/storage"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// Check-out and check-in (#194): the protocol page a person walks round
// the unit with, one-handed on a phone. A checkpoint's answer and the
// handover's finish are held by the offline queue when there is no
// connection; a photograph waits for one, as on the workshop's checks.

func (s *Server) rentalHandoverRoutes(m routeMux) {
	m.HandleFunc("GET /rental/handovers/{id}", s.requireSession(s.handleRentalHandover))
	m.HandleFunc("POST /rental/handovers/{id}/items/{itemID}", s.requireSession(s.handleRentalHandoverItem))
	m.HandleFunc("POST /rental/handovers/{id}/items/{itemID}/photo", s.requireSession(s.handleRentalHandoverPhoto))
	m.HandleFunc("POST /rental/handovers/{id}/finish", s.requireSession(s.handleRentalHandoverFinish))
	m.HandleFunc("POST /rental/handovers/{id}/abandon", s.requireSession(s.handleRentalHandoverAbandon))
	m.HandleFunc("GET /rental/photos/{key}", s.requireSession(s.handleRentalPhoto))
}

// handleRentalHandoverStart starts a check-out or check-in from the
// booking, or carries on with the one under way.
func (s *Server) handleRentalHandoverStart(w http.ResponseWriter, r *http.Request, kind string) {
	session := sessionFrom(r.Context())
	id, err := workshop.StartHandover(r.Context(), s.pool, session.Scope, r.PathValue("id"), kind)
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/rental/handovers/"+id, http.StatusSeeOther)
}

func (s *Server) handleRentalHandover(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	view, p, err := workshop.RentalHandoverOfProtocol(r.Context(), s.pool, session.Scope, r.PathValue("id"))
	if s.handoverError(w, r, err) {
		return
	}
	d := rentalData{Handovers: view, Protocol: p}
	if d.Booking, err = workshop.RentalBookingByID(r.Context(), s.pool, session.Scope, view.BookingID); s.handoverError(w, r, err) {
		return
	}
	title := "Check-out"
	if p.Subject == workshop.HandoverIn {
		title = "Check-in"
	}
	s.render(w, r, http.StatusOK, "rentalhandover", pageData{Title: title, Session: session, Rental: d})
}

func (s *Server) handleRentalHandoverItem(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	err := workshop.RecordProtocolItem(r.Context(), s.pool, session.Scope, r.PathValue("itemID"), r.FormValue("status"), r.FormValue("note"))
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/rental/handovers/"+r.PathValue("id")+"#item-"+r.PathValue("itemID"), http.StatusSeeOther)
}

// handleRentalHandoverPhoto takes a photograph of a checkpoint, as the
// workshop's checks do.
func (s *Server) handleRentalHandoverPhoto(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id, itemID := r.PathValue("id"), r.PathValue("itemID")
	r.Body = http.MaxBytesReader(w, r.Body, storage.MaxUpload+1<<20)
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		s.renderError(w, r, http.StatusBadRequest, "That did not arrive",
			"The photograph was too large, or the upload was interrupted. Try again.")
		return
	}
	defer r.MultipartForm.RemoveAll()
	file, header, err := r.FormFile("photo")
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "No photograph", "Nothing was attached.")
		return
	}
	defer file.Close()
	saved, err := s.photos.SavePhoto(file)
	if errors.Is(err, storage.ErrNotAnImage) {
		s.renderError(w, r, http.StatusBadRequest, "Not a photograph", "That file is not an image this system can read.")
		return
	}
	if err != nil {
		s.log.Error("save photo", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Could not store it", "Try again.")
		return
	}
	err = workshop.AttachProtocolPhoto(r.Context(), s.pool, session.Scope, itemID, saved.Key, saved.ContentType, saved.ByteSize, header.Filename)
	if err != nil {
		if rmErr := s.photos.Remove(saved.Key); rmErr != nil {
			s.log.Error("remove orphaned photo", "key", saved.Key, "error", rmErr)
		}
		s.handoverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/rental/handovers/"+id+"#item-"+itemID, http.StatusSeeOther)
}

func (s *Server) handleRentalHandoverFinish(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	reading := workshop.HandoverReading{
		RenterPresent:  r.FormValue("renter_present") != "",
		RenterAccepted: r.FormValue("renter_accepted") != "",
		Note:           r.FormValue("note"),

		Key: r.Header.Get(IdempotencyHeader),
	}
	reading.Dismissed, reading.Confirmed = findingsChosen(r.Form)
	if v := strings.TrimSpace(r.FormValue("odometer")); v != "" {
		km, err := strconv.ParseInt(strings.ReplaceAll(v, " ", ""), 10, 64)
		if err != nil {
			s.renderError(w, r, http.StatusBadRequest, "Not accepted", "read the odometer, in whole kilometres")
			return
		}
		reading.OdometerKm = &km
	}
	if v := strings.TrimSpace(r.FormValue("fuel")); v != "" {
		f, err := strconv.Atoi(v)
		if err != nil {
			s.renderError(w, r, http.StatusBadRequest, "Not accepted", "the fuel or charge is read in eighths, from empty to full")
			return
		}
		reading.FuelEighths = &f
	}
	booking, err := workshop.RentalBookingOfProtocol(r.Context(), s.pool, session.Scope, r.PathValue("id"))
	if s.handoverError(w, r, err) {
		return
	}
	if err := workshop.FinishHandover(r.Context(), s.pool, session.Scope, r.PathValue("id"), reading); s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/rental/bookings/"+booking, http.StatusSeeOther)
}

func (s *Server) handleRentalHandoverAbandon(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	booking, err := workshop.RentalBookingOfProtocol(r.Context(), s.pool, session.Scope, r.PathValue("id"))
	if s.handoverError(w, r, err) {
		return
	}
	if err := workshop.AbandonHandover(r.Context(), s.pool, session.Scope, r.PathValue("id"), r.FormValue("reason")); s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/rental/bookings/"+booking, http.StatusSeeOther)
}

// handleRentalPhoto serves a check-out or check-in photograph to the
// rental desk, in a shop that may not run the workshop.
func (s *Server) handleRentalPhoto(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	ok, err := workshop.RentalProtocolPhoto(r.Context(), s.pool, session.Scope, r.PathValue("key"))
	if s.handoverError(w, r, err) {
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.servePhoto(w, r, r.PathValue("key"))
}

// Compared is a check-in checkpoint beside the check-out, if there is one.
func (d rentalData) Compared(itemID string) *workshop.ComparedItem {
	if d.Handovers.Comparison == nil {
		return nil
	}
	for i := range d.Handovers.Comparison.Items {
		if d.Handovers.Comparison.Items[i].In.ID == itemID {
			return &d.Handovers.Comparison.Items[i]
		}
	}
	return nil
}

// NewFindings are the check-in's checkpoints worse than at check-out.
func (d rentalData) NewFindings() []workshop.ComparedItem {
	if d.Handovers.Comparison == nil {
		return nil
	}
	var out []workshop.ComparedItem
	for _, c := range d.Handovers.Comparison.Items {
		if c.New {
			out = append(out, c)
		}
	}
	return out
}

// StatusLabel is a checkpoint's status as a catalogue key.
func (rentalData) StatusLabel(status string) string {
	switch status {
	case "pass", "attention", "fail":
		return status
	}
	return "not checked"
}

// ChargeLabel is what a proposed return charge is, as a catalogue key
// with the quantity in it.
func (rentalData) ChargeLabel(kind string) string {
	switch kind {
	case workshop.ChargeExtraKm:
		return "%s km over the allowance"
	case workshop.ChargeFuel:
		return "%s eighths of fuel or charge short"
	case workshop.ChargeLate:
		return "%s minutes late"
	}
	return kind
}

// Eighths are the fuel gauge's marks, empty to full.
func (rentalData) Eighths() []int { return []int{0, 1, 2, 3, 4, 5, 6, 7, 8} }

// HandoverEnds are the ends of the rental that have happened, out first.
func (d rentalData) HandoverEnds() []workshop.RentalHandover {
	var out []workshop.RentalHandover
	for _, h := range []*workshop.RentalHandover{d.Handovers.Out, d.Handovers.In} {
		if h != nil {
			out = append(out, *h)
		}
	}
	return out
}

// findingsChosen reads the finish form's findings. Today's form says so
// (finish_form=2), names what the page showed as new, and those shown
// and unticked are let go. A form from before -- cached, and replayed
// from a phone's queue -- names only the ticked, and those, possibly
// none, are the ones confirmed.
func findingsChosen(form url.Values) (dismissed, confirmed []string) {
	if form.Get("finish_form") != "2" {
		return nil, append([]string{}, form["damage"]...)
	}
	for _, id := range form["shown"] {
		if !slices.Contains(form["damage"], id) {
			dismissed = append(dismissed, id)
		}
	}
	return dismissed, nil
}
