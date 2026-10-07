package server

import (
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/auth"
	"github.com/RedaEkengren/FreeSMS/internal/i18n"
	"github.com/RedaEkengren/FreeSMS/internal/money"
	"github.com/RedaEkengren/FreeSMS/internal/vehicledata"
	"github.com/RedaEkengren/FreeSMS/internal/web"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// pageData is what every template receives. One struct rather than a map, so
// a typo in a field name fails to compile instead of rendering nothing.
type pageData struct {
	Title  string
	Locale string

	// The reader's language. Templates call .T and .N rather than a global
	// function, because a template function is bound when the template is
	// parsed and the language is not known until the request.
	printer *i18n.Printer

	// How this page writes money: the reader's language decides the
	// separators, the shop's currency decides the symbol.
	display money.Display

	// Where the reader is standing, for times: the shop's zone. Times are
	// stored as instants and read as wall clocks, and a page that formats an
	// instant directly prints the server's wall clock -- UTC in a container,
	// an hour or two off in Stockholm, and the day before at half past
	// midnight.
	zone    *time.Location
	Session auth.Session
	Error   string
	Email   string

	Jobs          []workshop.Job
	Job           workshop.Job
	Progress      workshop.Progress
	Surcharges    []workshop.SurchargeLine
	Charges       workshop.Surcharges
	Languages     []workshop.Language
	MyLocale      string
	Lines         []workshop.Line
	Board         []workshop.BoardEntry
	Next          []workshop.State
	Totals        money.Totals
	Invoices      []workshop.Invoice
	Document      workshop.Document
	Balance       workshop.Balance
	Receivables   []workshop.Balance
	Methods       []string
	Today         string
	Contact       workshop.Contact
	Shop          workshop.ShopDetails
	Customers     []workshop.Customer
	CustomerQuery string
	Requests      []workshop.PartRequest
	Findings      []workshop.Finding

	Inspection  workshop.Inspection
	Inspections []workshop.Inspection
	Templates   []workshop.Template
	Shares      []workshop.Share

	// The shop's checklists, and the one in the edit form: empty for a new
	// one, or what was typed when a save is sent back.
	Checklists []workshop.ChecklistTemplate

	// The part being added or changed, and the units it can be counted in.
	Catalogue    workshop.CatalogueEntry
	Units        []string
	RetiredParts []workshop.CatalogueEntry
	Checklist    workshop.ChecklistTemplate

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
	PartQuery   string
	PriceBands  []workshop.PriceBand
	WriteOffs   []workshop.WriteOffCost
	Reasons     map[string]string
	Part        workshop.Part
	Movements   []workshop.Movement
	Labels      []label
	Exports     []workshop.AccountingExport
	PeriodFrom  time.Time
	PeriodTo    time.Time
	Lookup      vehicledata.Vehicle
	LookupNote  string

	// Whether a vehicle lookup provider is configured at all. Empty is the
	// default and is not a degraded mode -- a shop without an arrangement
	// types what it knows -- so the intake form must not offer a button that
	// cannot do anything.
	CanLookUp bool
	Form      intakeForm

	// Setup, staff and the account page.
	MinPassword int

	// The staff page, and the account page's answer.
	Staff     []workshop.StaffMember
	StaffForm staffForm
	Roles     []access.Role
	Changed   bool
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

// templateFuncs are the few things a template genuinely cannot do itself.
// Anything more belongs in Go, where it can be tested.
var templateFuncs = template.FuncMap{
	"fmt":    money.Format,
	"divide": func(a, b int) int { return a / b },
	"date":   func(t time.Time) string { return t.Format("2006-01-02") },
	"asset":  web.Asset,
}

// T renders a message in the reader's language.
//
// The key is the English text: it reads at the call site, it survives a
// catalogue going missing, and a string nobody has translated still says
// something sensible.
func (d pageData) T(key string, args ...any) string {
	if key == "" || d.printer == nil {
		return key
	}
	return d.printer.T(key, args...)
}

// Money renders an amount for somebody to read.
//
// Templates call this rather than the fmt function, which stays the canonical
// decimal for a value that will be posted back and parsed. A grouped number in
// a hidden input returns as a parse error.
func (d pageData) Money(minor int64) string { return d.display.Amount(minor) }

// MoneyOrBlank renders an optional amount, and nothing at all when there is
// none. A part with no price set has no price, which is not the same as zero.
func (d pageData) MoneyOrBlank(minor *int64) string {
	if minor == nil {
		return ""
	}
	return d.display.Amount(*minor)
}

// Local is a stored instant as the shop's wall clock. Every instant a
// template prints goes through here; a date that is only a date -- an export
// period -- does not, because moving midnight UTC west of Greenwich is the
// day before.
func (d pageData) Local(t any) time.Time {
	zone := d.zone
	if zone == nil {
		zone = time.UTC
	}
	switch v := t.(type) {
	case time.Time:
		return v.In(zone)
	case *time.Time:
		if v != nil {
			return v.In(zone)
		}
	}
	return time.Time{}
}

// Instant is an instant in RFC 3339, for a form to send back unchanged.
func (d pageData) Instant(t any) string {
	switch v := t.(type) {
	case time.Time:
		return v.UTC().Format(time.RFC3339)
	case *time.Time:
		if v != nil {
			return v.UTC().Format(time.RFC3339)
		}
	}
	return ""
}

// Due is a document's due date, counted in days on the shop's calendar.
func (d pageData) Due(doc workshop.Document) time.Time {
	return doc.DueIn(d.Local(doc.IssuedAt).Location())
}

// Dot joins the parts that are there, separated, and leaves out the ones that
// are not.
//
// Three screens printed a separator alongside the field after it rather than
// between two fields that both exist, which put "· Shelf A2" on the stock page
// when nothing was reserved, "Shelf A2 ·" on the job page when the part had no
// price, and "Floor E · ·" when both were missing at once. A separator belongs
// between things, so something has to know how many things there are.
func (d pageData) Dot(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, " \u00b7 ")
}

// N renders a message with a count, choosing the plural form.
func (d pageData) N(key string, count int, args ...any) string {
	if d.printer == nil {
		return key
	}
	return d.printer.N(key, count, args...)
}

// Plural is a message's plural form with the count left as %d, for a script to
// fill in. See i18n.Printer.Form.
func (d pageData) Plural(key string, count int) string {
	if d.printer == nil {
		return key
	}
	return d.printer.Form(key, count)
}

// parseTemplates builds one template set per page.
//
// Each page file defines "content", so they cannot be parsed together -- the
// last one parsed would win and every page would render the same body. Pairing
// each page with the layout keeps the definitions from colliding.
func parseTemplates() (map[string]*template.Template, error) {
	pages := []string{"login", "jobs", "job", "board", "newjob", "parts", "inspect", "shared", "search", "vehicle", "time", "labour", "dashboard", "privacy", "stock", "scan", "labels", "accounting", "invoice", "shop", "customer", "checklists", "part", "staff", "account", "receivables", "setup", "error"}
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
		data.Locale = s.localeFor(r, data.Session)
	}
	data.printer = s.catalogues.For(data.Locale)
	data.display = money.DisplayFor(data.Locale, s.currency())
	data.zone = s.zone()

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
	if data.Locale == "" {
		data.Locale = s.localeFor(r, data.Session)
	}
	data.printer = s.catalogues.For(data.Locale)
	data.display = money.DisplayFor(data.Locale, s.currency())
	data.zone = s.zone()
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
