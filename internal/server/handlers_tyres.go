package server

import (
	"errors"
	"html/template"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/RedaEkengren/FreeSMS/internal/barcode"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// The tyre hotel's sets (#226): all of them for whoever works in the
// workshop, one set with where it has been, and a customer's own with the
// form that takes a new one in.

func (s *Server) handleTyres(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	sets, err := workshop.TyreSetsOf(r.Context(), s.pool, session.Scope, workshop.TyreFilter{})
	if s.handoverError(w, r, err) {
		return
	}
	pd := pageData{Title: "Tyre sets", Session: session, Tyres: sets}
	if limit, err := workshop.TreadWarningOf(r.Context(), s.pool, session.Scope); err == nil {
		pd.TreadLimit = limit
	}
	s.render(w, r, http.StatusOK, "tyres", pd)
}

func (s *Server) handleTyreSet(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	set, err := workshop.TyreSetByID(r.Context(), s.pool, session.Scope, r.PathValue("id"))
	if s.handoverError(w, r, err) {
		return
	}
	pd := pageData{Title: "A set of tyres", Session: session, TyreSet: set}
	if places, err := workshop.StorageLocations(r.Context(), s.pool, session.Scope); err == nil {
		pd.Places = places
	}
	// The owner's other cars, for moving the set to one of them.
	if set.CustomerID != "" {
		if card, err := workshop.CustomerByID(r.Context(), s.pool, session.Scope, set.CustomerID); err == nil {
			pd.CustomerCard = card
		}
	}
	s.render(w, r, http.StatusOK, "tyreset", pd)
}

func (s *Server) handleTyreSetMove(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	err := workshop.MoveTyreSet(r.Context(), s.pool, session.Scope, r.PathValue("id"), r.FormValue("vehicle"))
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/tyres/"+r.PathValue("id"), http.StatusSeeOther)
}

func (s *Server) handleTyreReplace(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	week, year := formInt(r, "dot_week"), formInt(r, "dot_year")
	err := workshop.ReplaceTyre(r.Context(), s.pool, session.Scope, r.FormValue("tyre"), workshop.NewTyre{
		Position: r.FormValue("position"), Dimension: r.FormValue("dimension"), DotWeek: week, DotYear: year})
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/tyres/"+r.PathValue("id"), http.StatusSeeOther)
}

func formInt(r *http.Request, key string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(r.FormValue(key)))
	return n
}

// handleCustomerTyres is one customer's sets and the form for a new one.
func (s *Server) handleCustomerTyres(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	card, err := workshop.CustomerByID(r.Context(), s.pool, session.Scope, r.PathValue("id"))
	if s.handoverError(w, r, err) {
		return
	}
	sets, err := workshop.TyreSetsOf(r.Context(), s.pool, session.Scope, workshop.TyreFilter{CustomerID: card.ID})
	if s.handoverError(w, r, err) {
		return
	}
	s.render(w, r, http.StatusOK, "customertyres", pageData{Title: "Tyre sets", Session: session, CustomerCard: card, Tyres: sets})
}

// handleCreateTyreSet takes a set in: a dimension for the front and, when the
// rear differs, for the rear, the same date of manufacture on all, and a spare
// when it has a dimension.
func (s *Server) handleCreateTyreSet(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	front := strings.TrimSpace(r.FormValue("front"))
	rear := strings.TrimSpace(r.FormValue("rear"))
	if rear == "" {
		rear = front
	}
	week, year := formInt(r, "dot_week"), formInt(r, "dot_year")
	tyre := func(pos, dim string) workshop.NewTyre {
		return workshop.NewTyre{Position: pos, Dimension: dim, DotWeek: week, DotYear: year}
	}
	n := workshop.NewTyreSet{
		CustomerID: r.PathValue("id"), VehicleID: r.FormValue("vehicle"), Season: r.FormValue("season"),
		Brand: r.FormValue("brand"), Model: r.FormValue("model"), Rims: r.FormValue("rims"),
		TPMS: r.FormValue("tpms") != "", Fasteners: r.FormValue("fasteners") != "", Note: r.FormValue("note"),
		Tyres: []workshop.NewTyre{tyre("front_left", front), tyre("front_right", front), tyre("rear_left", rear), tyre("rear_right", rear)},
	}
	if spare := strings.TrimSpace(r.FormValue("spare")); spare != "" {
		n.Tyres = append(n.Tyres, tyre("spare", spare))
	}
	id, err := workshop.CreateTyreSet(r.Context(), s.pool, session.Scope, n)
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/tyres/"+id, http.StatusSeeOther)
}

// handleStorage is the places with what is in each, and for whoever runs the
// shop the form that makes a new one.
func (s *Server) handleStorage(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	places, err := workshop.StorageLocations(r.Context(), s.pool, session.Scope)
	if s.handoverError(w, r, err) {
		return
	}
	s.render(w, r, http.StatusOK, "storage", pageData{Title: "Storage places", Session: session, Places: places})
}

func (s *Server) handleCreateStorage(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	_, err := workshop.CreateStorageLocation(r.Context(), s.pool, session.Scope, r.FormValue("name"), formInt(r, "capacity"))
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/storage", http.StatusSeeOther)
}

func (s *Server) handleStorageUse(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	err := workshop.SetStorageLocationInUse(r.Context(), s.pool, session.Scope, r.PathValue("id"), r.FormValue("use") == "1")
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/storage", http.StatusSeeOther)
}

// handlePlaceTyreSet puts a set in a place, or takes it out of storage when no
// place is given.
func (s *Server) handlePlaceTyreSet(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	var err error
	if place := r.FormValue("place"); place == "" {
		err = workshop.TakeTyreSetOutOfStorage(r.Context(), s.pool, session.Scope, r.PathValue("id"))
	} else {
		err = workshop.PlaceTyreSet(r.Context(), s.pool, session.Scope, r.PathValue("id"), place)
	}
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/tyres/"+r.PathValue("id"), http.StatusSeeOther)
}

// storageLabel is one label to print: what it names and its code as a
// barcode.
type storageLabel struct {
	Name string
	Code string
	SVG  template.HTML
}

func (s *Server) barcodeLabel(name, code string) storageLabel {
	l := storageLabel{Name: name, Code: code}
	if svg, err := barcode.SVG(code, 240, 60); err == nil {
		l.SVG = template.HTML(svg)
	} else {
		s.log.Warn("storage label", "code", code, "error", err)
	}
	return l
}

// handleStorageLabels is the labels of every place and every set, for print.
func (s *Server) handleStorageLabels(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	places, err := workshop.StorageLocations(r.Context(), s.pool, session.Scope)
	if s.handoverError(w, r, err) {
		return
	}
	sets, err := workshop.TyreSetsOf(r.Context(), s.pool, session.Scope, workshop.TyreFilter{})
	if s.handoverError(w, r, err) {
		return
	}
	var pd pageData
	pd.Title, pd.Session = "Storage labels", session
	for _, p := range places {
		pd.StorageLabels = append(pd.StorageLabels, s.barcodeLabel(p.Name, workshop.StorageLabel(p.ID)))
	}
	for _, t := range sets {
		name := strings.TrimSpace(t.Brand + " " + t.Model)
		if t.Vehicle != "" {
			name += " · " + t.Vehicle
		}
		pd.SetLabels = append(pd.SetLabels, s.barcodeLabel(name, workshop.TyreSetLabel(t.ID)))
	}
	s.render(w, r, http.StatusOK, "storagelabels", pd)
}

// handleStorageScan is the quick way to put a set in a place: scan the set's
// label, then the place's. The set being worked on is carried in the address,
// so nothing is kept between the two scans.
func (s *Server) handleStorageScan(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	pd := pageData{Title: "Scan to place", Session: session}
	if id := r.URL.Query().Get("set"); id != "" {
		set, err := workshop.TyreSetByID(r.Context(), s.pool, session.Scope, id)
		if s.handoverError(w, r, err) {
			return
		}
		pd.TyreSet = set
	}
	pd.ShareToken = r.URL.Query().Get("done")
	s.render(w, r, http.StatusOK, "storagescan", pd)
}

func (s *Server) handleStorageScanPost(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	kind, id, err := workshop.ResolveStorageLabel(r.Context(), s.pool, session.Scope, r.FormValue("code"))
	if errors.Is(err, workshop.ErrNotFound) {
		s.renderError(w, r, http.StatusNotFound, "Not found", "Nothing in this shop has that label.")
		return
	}
	if s.handoverError(w, r, err) {
		return
	}
	set := r.FormValue("set")
	switch {
	case kind == "set":
		http.Redirect(w, r, "/storage/scan?set="+id, http.StatusSeeOther)
	case set == "":
		http.Redirect(w, r, "/storage", http.StatusSeeOther)
	default:
		if err := workshop.PlaceTyreSet(r.Context(), s.pool, session.Scope, set, id); s.handoverError(w, r, err) {
			return
		}
		s.submitted(w, r)
		http.Redirect(w, r, "/storage/scan?done="+id, http.StatusSeeOther)
	}
}

// parseMillimetres reads a depth such as "3,5" or "3.5" as tenths of a
// millimetre. It is a measurement, never money, so a float is fine for the
// reading; what is kept is a whole number.
func parseMillimetres(v string) (int, bool) {
	v = strings.ReplaceAll(strings.TrimSpace(v), ",", ".")
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 || f > 25 {
		return 0, false
	}
	return int(math.Round(f * 10)), true
}

func (s *Server) handleTreadDepth(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	tenths, ok := parseMillimetres(r.FormValue("depth"))
	if !ok {
		s.renderError(w, r, http.StatusBadRequest, "Not saved", "A depth is a number of millimetres, from 0 to 25.")
		return
	}
	_, err := workshop.RecordTreadDepth(r.Context(), s.pool, session.Scope, r.FormValue("tyre"), tenths, r.FormValue("note"))
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/tyres/"+r.PathValue("id"), http.StatusSeeOther)
}

func (s *Server) handleCorrectTreadDepth(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	tenths, ok := parseMillimetres(r.FormValue("depth"))
	if !ok {
		s.renderError(w, r, http.StatusBadRequest, "Not saved", "A depth is a number of millimetres, from 0 to 25.")
		return
	}
	_, err := workshop.CorrectTreadDepth(r.Context(), s.pool, session.Scope, r.FormValue("measurement"), tenths, r.FormValue("reason"))
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/tyres/"+r.PathValue("id"), http.StatusSeeOther)
}

func (s *Server) handleTreadLimit(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	tenths, ok := parseMillimetres(r.FormValue("limit"))
	if !ok {
		s.renderError(w, r, http.StatusBadRequest, "Not saved", "A depth is a number of millimetres, from 0 to 25.")
		return
	}
	err := workshop.SetTreadWarning(r.Context(), s.pool, session.Scope, tenths)
	if s.handoverError(w, r, err) {
		return
	}
	s.submitted(w, r)
	http.Redirect(w, r, "/tyres", http.StatusSeeOther)
}
