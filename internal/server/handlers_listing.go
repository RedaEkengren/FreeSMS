package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/storage"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A stock car offered (#167) and the public stock feed (#183). The feed is
// documented in docs/stock-feed.md.

// feedToken is the token a feed request carries: a bearer header, or the
// token query parameter for a site that can only fetch a URL.
func feedToken(r *http.Request) string {
	if t, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(t)
	}
	return r.URL.Query().Get("token")
}

// handleStockFeed is /feed/v1/stock.json: the cars for sale as the shop
// publishes them, with Last-Modified and an ETag, and since=<RFC 3339> for
// only what changed. Closed or wrong token: as a path that does not exist.
func (s *Server) handleStockFeed(w http.ResponseWriter, r *http.Request) {
	var since time.Time
	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			http.Error(w, "since is a time in RFC 3339", http.StatusBadRequest)
			return
		}
		since = t
	}
	cars, newest, err := workshop.StockFeed(r.Context(), s.pool, s.shop(), feedToken(r), since)
	if errors.Is(err, workshop.ErrFeedClosed) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.log.Error("stock feed", "error", err)
		http.Error(w, "the feed could not be read", http.StatusInternalServerError)
		return
	}
	if cars == nil {
		cars = []workshop.FeedCar{}
	}
	body, err := json.Marshal(struct {
		Version int                `json:"version"`
		Cars    []workshop.FeedCar `json:"cars"`
	}{1, cars})
	if err != nil {
		http.Error(w, "the feed could not be written", http.StatusInternalServerError)
		return
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	w.Header().Set("ETag", etag)
	if !newest.IsZero() {
		w.Header().Set("Last-Modified", newest.UTC().Format(http.TimeFormat))
	}
	w.Header().Set("Cache-Control", "no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(body)
}

// handleFeedPhoto is a photograph of a car in the feed, by its public id:
// cacheable by anyone, as the site shows it to anyone. A photograph removed,
// or of a car no longer for sale, is not there.
func (s *Server) handleFeedPhoto(w http.ResponseWriter, r *http.Request) {
	key, err := workshop.FeedPhoto(r.Context(), s.pool, s.shop(), feedToken(r), r.PathValue("photo"))
	if errors.Is(err, workshop.ErrFeedClosed) || errors.Is(err, workshop.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.log.Error("feed photo", "error", err)
		http.Error(w, "the photograph could not be read", http.StatusInternalServerError)
		return
	}
	f, err := s.photos.Open(key)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "image/jpeg")
	// Public: the site shows it to everyone. An hour, so a photograph
	// taken away leaves caches soon after.
	w.Header().Set("Cache-Control", "public, max-age=3600")
	if _, err := io.Copy(w, f); err != nil {
		s.log.Warn("serve feed photo", "error", err)
	}
}

// handleCarListing records how the car is offered.
func (s *Server) handleCarListing(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	form := map[string]string{}
	for _, k := range []string{"asking_price", "variant", "fuel", "gearbox", "colour", "equipment", "description"} {
		form[k] = strings.TrimSpace(r.FormValue(k))
	}
	l := workshop.Listing{Variant: form["variant"], Fuel: form["fuel"], Gearbox: form["gearbox"], Colour: form["colour"],
		VATDeductible: r.FormValue("vat_deductible") != "", Description: form["description"]}
	if form["asking_price"] != "" {
		n, err := parseMinorUnits(form["asking_price"])
		if err != nil {
			s.carUnitPage(w, r, http.StatusBadRequest, form, "Say the price in kronor.")
			return
		}
		l.AskingPriceMinor = &n
	}
	for _, line := range strings.Split(form["equipment"], "\n") {
		l.Equipment = append(l.Equipment, strings.TrimSpace(line))
	}
	if s.carUnitRefused(w, r, workshop.SetListing(r.Context(), s.pool, session.Scope, r.PathValue("id"), l), form) {
		return
	}
	s.carUnitDone(w, r)
}

// handleCarPhoto takes a photograph of the stock car.
func (s *Server) handleCarPhoto(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
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
	if _, err := workshop.AttachStockPhoto(r.Context(), s.pool, session.Scope, r.PathValue("id"), saved.Key, saved.ContentType,
		saved.ByteSize, header.Filename); err != nil {
		if rmErr := s.photos.Remove(saved.Key); rmErr != nil {
			s.log.Error("remove orphaned photo", "key", saved.Key, "error", rmErr)
		}
		if s.carUnitRefused(w, r, err, nil) {
			return
		}
	}
	s.carUnitDone(w, r)
}

// handleCarPhotoCover makes a photograph the cover.
func (s *Server) handleCarPhotoCover(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	if s.carUnitRefused(w, r, workshop.MakeCover(r.Context(), s.pool, session.Scope, r.PathValue("photo")), nil) {
		return
	}
	s.carUnitDone(w, r)
}

// handleCarPhotoRemove removes a photograph of the stock car.
func (s *Server) handleCarPhotoRemove(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	if s.carUnitRefused(w, r, workshop.RemoveStockPhoto(r.Context(), s.pool, session.Scope, s.photos, r.PathValue("photo")), nil) {
		return
	}
	s.carUnitDone(w, r)
}

// handleCarPhotoFile is a stock car's photograph for whoever may open the
// car: the page's own images.
func (s *Server) handleCarPhotoFile(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	photos, err := workshop.StockPhotos(r.Context(), s.pool, session.Scope, r.PathValue("id"))
	if s.handoverError(w, r, err) {
		return
	}
	for _, p := range photos {
		if p.ID == r.PathValue("photo") {
			s.servePhoto(w, r, p.Key)
			return
		}
	}
	http.NotFound(w, r)
}

// handleStockFeedSettings sets how the stock feed is published; a new
// token is made and shown once when asked for.
func (s *Server) handleStockFeedSettings(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	token := ""
	if r.FormValue("new_token") != "" {
		b := make([]byte, 24)
		if _, err := rand.Read(b); err != nil {
			http.Error(w, "no randomness", http.StatusInternalServerError)
			return
		}
		token = hex.EncodeToString(b)
	}
	err := workshop.SetFeed(r.Context(), s.pool, session.Scope, r.FormValue("published") != "", r.FormValue("open") != "",
		r.FormValue("include_reserved") != "", token)
	if errors.Is(err, workshop.ErrInvalid) {
		s.renderError(w, r, http.StatusBadRequest, "Not saved", trimInvalid(err))
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	if token != "" {
		// Shown this once, and never again: it is kept only as its hash.
		s.render(w, r, http.StatusOK, "feedtoken", pageData{Title: "The stock feed's token", Session: session,
			Dealer: &dealerData{Form: map[string]string{"token": token}}})
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/cars/stock", http.StatusSeeOther)
}

// listingRoutes registers a stock car's listing and photographs, and the
// feed.
func (s *Server) listingRoutes(m routeMux) {
	m.HandleFunc("POST /cars/stock/{id}/listing", s.requireSession(s.handleCarListing))
	m.HandleFunc("POST /cars/stock/{id}/photos", s.requireSession(s.handleCarPhoto))
	m.HandleFunc("GET /cars/stock/{id}/photos/{photo}", s.requireSession(s.handleCarPhotoFile))
	m.HandleFunc("POST /cars/stock/{id}/photos/{photo}/cover", s.requireSession(s.handleCarPhotoCover))
	m.HandleFunc("POST /cars/stock/{id}/photos/{photo}/remove", s.requireSession(s.handleCarPhotoRemove))
	m.HandleFunc("POST /cars/feed", s.requireSession(s.handleStockFeedSettings))
	m.HandleFunc("GET /feed/v1/stock.json", s.handleStockFeed)
	m.HandleFunc("GET /feed/v1/photos/{photo}", s.handleFeedPhoto)
}
