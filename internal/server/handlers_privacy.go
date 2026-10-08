package server

import (
	"context"
	"fmt"
	"github.com/RedaEkengren/FreeSMS/internal/access"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// handleExportPerson sends somebody a copy of what is held about them.
func (s *Server) handleExportPerson(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")

	export, err := workshop.ExportPerson(r.Context(), s.pool, session.Scope, id)
	if s.handoverError(w, r, err) {
		return
	}
	body, err := workshop.MarshalExport(export)
	if err != nil {
		s.log.Error("marshal export", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "Try again.")
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// A file the person can keep, rather than a page they have to screenshot.
	w.Header().Set("Content-Disposition", `attachment; filename="personal-data.json"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

func (s *Server) handleErasePerson(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id := r.PathValue("id")

	if err := workshop.ErasePerson(r.Context(), s.pool, session.Scope, id, r.FormValue("reason")); s.handoverError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/privacy", http.StatusSeeOther)
}

// handlePrivacy is where the shop answers for what it holds.
func (s *Server) handlePrivacy(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())

	erasures, err := workshop.Erasures(r.Context(), s.pool, session.Scope)
	if s.handoverError(w, r, err) {
		return
	}
	var exports []workshop.ShopExport
	if session.Scope.Role.RunsTheShop() {
		if exports, err = workshop.ShopExports(r.Context(), s.pool, session.Scope); err != nil {
			s.log.Error("read shop exports", "error", err)
		}
	}
	s.render(w, r, http.StatusOK, "privacy", pageData{
		Title:       "Personal data",
		Session:     session,
		Erasures:    erasures,
		ShopExports: exports,
	})
}

// handleShopExport starts the export of the whole shop, which runs on after
// the request: ten years of history will not fit in one. The file is written
// readable by this service's user alone.
func (s *Server) handleShopExport(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	id, err := workshop.StartShopExport(r.Context(), s.pool, session.Scope)
	if s.handoverError(w, r, err) {
		return
	}
	go s.writeShopExport(session.Scope, id)
	http.Redirect(w, r, "/privacy", http.StatusSeeOther)
}

func (s *Server) writeShopExport(scope access.Scope, id string) {
	ctx := context.Background()
	name := "freesms-" + time.Now().UTC().Format("20060102-150405") + "-" + id[:8] + ".zip"
	var size int64
	err := func() error {
		if err := os.MkdirAll(s.exportDir, 0o700); err != nil {
			return fmt.Errorf("export directory: %w", err)
		}
		f, err := os.OpenFile(filepath.Join(s.exportDir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("export file: %w", err)
		}
		werr := workshop.WriteShopExport(ctx, s.pool, scope, f, s.photos, s.release)
		if st, err := f.Stat(); err == nil {
			size = st.Size()
		}
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		return werr
	}()
	if err != nil {
		s.log.Error("shop export", "error", err)
		name = ""
	}
	if ferr := workshop.FinishShopExport(ctx, s.pool, scope, id, name, size, err); ferr != nil {
		s.log.Error("record shop export", "error", ferr)
	}
}

// handleShopExportFile hands the owner the file. Owner only, by the export's
// own record, so no path from a request ever reaches the disk.
func (s *Server) handleShopExportFile(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	e, err := workshop.ShopExportByID(r.Context(), s.pool, session.Scope, r.PathValue("id"))
	if s.handoverError(w, r, err) {
		return
	}
	if !e.Done() {
		s.renderError(w, r, http.StatusConflict, "Not ready", "The export is still being written, or it failed.")
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+e.FileName+`"`)
	http.ServeFile(w, r, filepath.Join(s.exportDir, filepath.Base(e.FileName)))
}
