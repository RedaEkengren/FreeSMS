package server

import (
	"errors"
	"io"
	"net/http"

	"github.com/RedaEkengren/RedaSMS/internal/access"
	"github.com/RedaEkengren/RedaSMS/internal/storage"
	"github.com/RedaEkengren/RedaSMS/internal/workshop"
)

func (s *Server) handleStartInspection(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	jobID := r.PathValue("id")

	id, err := workshop.StartInspection(r.Context(), s.pool, session.Scope, jobID, r.FormValue("template_id"))
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/inspections/"+id, http.StatusSeeOther)
}

func (s *Server) handleInspection(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")

	insp, err := s.inspection(r, session.Scope, id)
	if s.handoverError(w, r, err) {
		return
	}
	s.render(w, r, http.StatusOK, "inspect", pageData{
		Title:      insp.TemplateName,
		Session:    session,
		Inspection: insp,
		Shares:     s.sharesFor(r, session.Scope, id),
	})
}

// sharesFor lists the links made for an inspection, for the front desk.
//
// A failure here is logged and returns nothing rather than failing the page:
// not being able to list the links is no reason to be unable to see the
// inspection.
func (s *Server) sharesFor(r *http.Request, scope access.Scope, id string) []workshop.Share {
	if !scope.Role.SeesCustomerPersonalData() {
		return nil
	}
	shares, err := workshop.SharesFor(r.Context(), s.pool, scope, id)
	if err != nil {
		s.log.Error("list shares", "error", err)
		return nil
	}
	return shares
}

// inspection reads one inspection by id, via the job it belongs to.
func (s *Server) inspection(r *http.Request, scope access.Scope, id string) (workshop.Inspection, error) {
	all, err := workshop.InspectionsForID(r.Context(), s.pool, scope, id)
	if err != nil {
		return workshop.Inspection{}, err
	}
	return all, nil
}

func (s *Server) handleSetInspectionItem(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")

	err := workshop.SetItem(r.Context(), s.pool, session.Scope,
		r.PathValue("itemID"), r.FormValue("status"), r.FormValue("note"))
	if s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/inspections/"+id, http.StatusSeeOther)
}

// handleUploadPhoto takes a photograph from the technician's phone.
func (s *Server) handleUploadPhoto(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")
	itemID := r.PathValue("itemID")

	// Bound what is read before reading it. The declared length is the
	// client's claim, and this is not.
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
		s.renderError(w, r, http.StatusBadRequest, "Not a photograph",
			"That file is not an image this system can read.")
		return
	}
	if err != nil {
		s.log.Error("save photo", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Could not store it", "Try again.")
		return
	}

	err = workshop.AttachPhoto(r.Context(), s.pool, session.Scope,
		itemID, saved.Key, saved.ContentType, saved.ByteSize, header.Filename)
	if err != nil {
		// The row is what makes the file reachable, so a file with no row is
		// rubbish. Remove it rather than leaving it to be found by a disk
		// audit in two years.
		if rmErr := s.photos.Remove(saved.Key); rmErr != nil {
			s.log.Error("remove orphaned photo", "key", saved.Key, "error", rmErr)
		}
		s.handoverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/inspections/"+id, http.StatusSeeOther)
}

func (s *Server) handleCompleteInspection(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")

	if err := workshop.CompleteInspection(r.Context(), s.pool, session.Scope, id); s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/inspections/"+id, http.StatusSeeOther)
}

func (s *Server) handleShareInspection(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")

	token, err := workshop.CreateShare(r.Context(), s.pool, session.Scope, id)
	if s.handoverError(w, r, err) {
		return
	}
	// The token is shown once, here. It is not stored, so there is nowhere to
	// go and look it up again -- make a new link instead.
	insp, err := s.inspection(r, session.Scope, id)
	if s.handoverError(w, r, err) {
		return
	}
	s.render(w, r, http.StatusOK, "inspect", pageData{
		Title:      insp.TemplateName,
		Session:    session,
		Inspection: insp,
		Shares:     s.sharesFor(r, session.Scope, id),
		ShareURL:   s.baseURL + "/i/" + token,
	})
}

func (s *Server) handleRevokeShare(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")

	if err := workshop.RevokeShare(r.Context(), s.pool, session.Scope, r.FormValue("share_id")); s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/inspections/"+id, http.StatusSeeOther)
}

// handlePhoto serves a photograph to somebody signed in.
func (s *Server) handlePhoto(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	key := r.PathValue("key")

	ok, err := workshop.PhotoInShop(r.Context(), s.pool, session.Scope, key)
	if err != nil {
		s.log.Error("photo lookup", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.servePhoto(w, r, key)
}

func (s *Server) servePhoto(w http.ResponseWriter, r *http.Request, key string) {
	f, err := s.photos.Open(key)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "image/jpeg")
	// Private: this is somebody's vehicle, and a shared cache has no business
	// holding it. Immutable because the key names exactly these bytes.
	w.Header().Set("Cache-Control", "private, max-age=86400, immutable")
	if _, err := io.Copy(w, f); err != nil {
		s.log.Warn("serve photo", "key", key, "error", err)
	}
}

// handleSharedInspection is the page the customer opens.
func (s *Server) handleSharedInspection(w http.ResponseWriter, r *http.Request) {
	insp, _, err := workshop.SharedInspection(r.Context(), s.pool, s.shop(), r.PathValue("token"))
	if errors.Is(err, workshop.ErrShareNotUsable) {
		// One answer for wrong, expired and revoked. Whoever is holding a link
		// that does not work does not need to be told which kind.
		s.render(w, r, http.StatusNotFound, "shared", pageData{
			Title: "This link is no longer open",
			Error: "Ask the workshop for a new one.",
		})
		return
	}
	if err != nil {
		s.log.Error("shared inspection", "error", err)
		s.render(w, r, http.StatusInternalServerError, "shared", pageData{
			Title: "Something went wrong", Error: "Try again shortly.",
		})
		return
	}
	s.render(w, r, http.StatusOK, "shared", pageData{
		Title:      "Your vehicle",
		Inspection: insp,
		ShareToken: r.PathValue("token"),
	})
}

// handleSharedDecision records the customer's yes or no.
func (s *Server) handleSharedDecision(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	err := workshop.RecordDecision(r.Context(), s.pool, s.shop(), token,
		r.PathValue("itemID"), r.FormValue("decision"))
	if err != nil {
		status := http.StatusNotFound
		if errors.Is(err, workshop.ErrInvalid) {
			status = http.StatusBadRequest
		}
		s.render(w, r, status, "shared", pageData{
			Title: "That did not go through", Error: "Open the link again and try once more.",
		})
		return
	}
	http.Redirect(w, r, "/i/"+token, http.StatusSeeOther)
}

// handleSharedPhoto serves a photograph to whoever holds the link.
func (s *Server) handleSharedPhoto(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	ok, err := workshop.PhotoInShare(r.Context(), s.pool, s.shop(), r.PathValue("token"), key)
	if err != nil {
		s.log.Error("shared photo lookup", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.servePhoto(w, r, key)
}
