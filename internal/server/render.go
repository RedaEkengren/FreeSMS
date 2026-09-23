package server

import (
	"fmt"
	"html/template"
	"net/http"
	"time"

	"github.com/RedaEkengren/RedaSMS/internal/auth"
	"github.com/RedaEkengren/RedaSMS/internal/money"
	"github.com/RedaEkengren/RedaSMS/internal/web"
	"github.com/RedaEkengren/RedaSMS/internal/workshop"
)

// pageData is what every template receives. One struct rather than a map, so
// a typo in a field name fails to compile instead of rendering nothing.
type pageData struct {
	Title   string
	Locale  string
	Session auth.Session
	Error   string
	Email   string

	Jobs     []workshop.Job
	Job      workshop.Job
	Lines    []workshop.Line
	Board    []workshop.BoardEntry
	Next     []workshop.State
	Totals   money.Totals
	Invoices []workshop.Invoice
	Requests []workshop.PartRequest
	Findings []workshop.Finding

	Inspection  workshop.Inspection
	Inspections []workshop.Inspection
	Templates   []workshop.Template
	Shares      []workshop.Share

	// Shown once, straight after a link is made. The token is not stored, so
	// there is nowhere to look it up again -- make a new link instead.
	ShareURL   string
	ShareToken string

	Query   string
	Results []workshop.SearchResult
	Vehicle workshop.Vehicle

	TimeMine    []workshop.TimeEntry
	TimeFlagged []workshop.TimeEntry

	LabourTimes []workshop.LabourTime
	LabourRate  int64
	Suggestions []workshop.Suggestion
	Dashboard   workshop.Dashboard
	Erasures    []workshop.Erasure
	Parts       []workshop.Part
	PriceBands  []workshop.PriceBand
	WriteOffs   []workshop.WriteOffCost
	Reasons     map[string]string
	Part        workshop.Part
	Movements   []workshop.Movement
	Labels      []label
	Exports     []workshop.AccountingExport
	PeriodFrom  time.Time
	PeriodTo    time.Time
	Form        intakeForm

	// Setup only.
	MinPassword int
}

// intakeForm keeps what was typed when a submission is sent back with an
// error. Clearing a form because one field was wrong is how a counter ends up
// re-typing a registration number three times.
type intakeForm struct {
	Registration string
	OdometerKm   string
	Complaint    string

	// Setup reuses this struct rather than carrying a second one through
	// every page: the fields are disjoint and no template reads both.
	ShopName  string
	OwnerName string
	Email     string
}

// setupForm is intakeForm under a name that says which page it belongs to.
type setupForm = intakeForm

// templateFuncs are the two things a template genuinely cannot do itself.
// Anything more belongs in Go, where it can be tested.
var templateFuncs = template.FuncMap{
	"fmt":    money.Format,
	"divide": func(a, b int) int { return a / b },
	"date":   func(t time.Time) string { return t.Format("2006-01-02") },
}

// parseTemplates builds one template set per page.
//
// Each page file defines "content", so they cannot be parsed together -- the
// last one parsed would win and every page would render the same body. Pairing
// each page with the layout keeps the definitions from colliding.
func parseTemplates() (map[string]*template.Template, error) {
	pages := []string{"login", "jobs", "job", "board", "newjob", "parts", "inspect", "shared", "search", "vehicle", "time", "labour", "dashboard", "privacy", "stock", "scan", "labels", "accounting", "setup", "error"}
	out := make(map[string]*template.Template, len(pages))
	for _, name := range pages {
		t, err := template.New(name).Funcs(templateFuncs).ParseFS(web.Templates,
			"templates/layout.html", "templates/"+name+".html")
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		out[name] = t
	}
	return out, nil
}

// render writes a full page.
//
// The buffer is deliberate: executing straight into the ResponseWriter sends a
// 200 and part of a page before a template error is noticed, leaving the
// browser with half a document and no way to tell something went wrong.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page string, data pageData) {
	t, ok := s.templates[page]
	if !ok {
		s.log.Error("unknown template", "page", page)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if data.Locale == "" {
		data.Locale = s.locale
	}

	buf := newBuffer()
	defer releaseBuffer(buf)

	if err := t.ExecuteTemplate(buf, "layout", data); err != nil {
		s.log.Error("render", "page", page, "error", err, "path", r.URL.Path)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// renderPartial writes one named block, for htmx to swap in.
func (s *Server) renderPartial(w http.ResponseWriter, r *http.Request, page, block string, data pageData) {
	t, ok := s.templates[page]
	if !ok {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	buf := newBuffer()
	defer releaseBuffer(buf)

	if err := t.ExecuteTemplate(buf, block, data); err != nil {
		s.log.Error("render partial", "block", block, "error", err, "path", r.URL.Path)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

func (s *Server) renderError(w http.ResponseWriter, r *http.Request, status int, title, message string) {
	s.render(w, r, status, "error", pageData{
		Title:   title,
		Error:   message,
		Session: sessionFrom(r.Context()),
	})
}
