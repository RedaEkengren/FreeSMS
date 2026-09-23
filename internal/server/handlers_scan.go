package server

import (
	"errors"
	"html/template"
	"net/http"
	"strings"

	"github.com/RedaEkengren/RedaSMS/internal/barcode"
	"github.com/RedaEkengren/RedaSMS/internal/workshop"
)

// handleScan is the counting screen.
//
// One field, focused, and nothing else that can take a keystroke. A handheld
// scanner is a keyboard: it types the code very fast and presses Enter, and on
// a page with several inputs that lands in whatever happened to have focus.
// Giving scanning its own screen with one target is cheaper and more reliable
// than trying to detect typing speed.
func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	code := strings.TrimSpace(r.URL.Query().Get("code"))

	data := pageData{
		Title:   "Scan",
		Session: session,
		Query:   code,
		Reasons: workshop.WriteOffReasons(),
	}

	if code != "" {
		part, err := workshop.PartByCode(r.Context(), s.pool, session.Scope, code)
		switch {
		case errors.Is(err, workshop.ErrNotFound):
			// A parts person in a cold store will not walk back to a desk.
			// Offering to create it here is the difference between a record
			// and a number written on a hand.
			data.Error = "Nothing answers to that code."
		case err != nil:
			s.log.Error("scan", "error", err)
			data.Error = "Something went wrong. Try again."
		default:
			data.Part = part
			movements, err := workshop.MovementsFor(r.Context(), s.pool, session.Scope, part.ID)
			if err != nil {
				s.log.Error("movements", "error", err)
			}
			data.Movements = movements
		}
	}
	s.render(w, r, http.StatusOK, "scan", data)
}

// handleLabels renders a printable sheet.
//
// A sheet for an ordinary printer, because a thermal label printer is the
// right tool and a painful one from a browser. This is the path that always
// works.
func (s *Server) handleLabels(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())

	parts, err := workshop.Parts(r.Context(), s.pool, session.Scope)
	if s.handoverError(w, r, err) {
		return
	}

	labels := make([]label, 0, len(parts))
	for _, p := range parts {
		svg, err := barcode.SVG(p.Number, 240, 60)
		if err != nil {
			// A number Code 128 cannot carry still gets a label, with the
			// text on it. Half a label beats none in a parts store.
			s.log.Warn("label", "part", p.Number, "error", err)
			labels = append(labels, label{Part: p})
			continue
		}
		labels = append(labels, label{Part: p, SVG: template.HTML(svg)})
	}

	s.render(w, r, http.StatusOK, "labels", pageData{
		Title:   "Labels",
		Session: session,
		Labels:  labels,
	})
}

// label pairs a part with its rendered barcode.
type label struct {
	Part workshop.Part
	SVG  template.HTML
}
