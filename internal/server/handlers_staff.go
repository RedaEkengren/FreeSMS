package server

import (
	"errors"
	"net/http"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/auth"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// handleStaff lists who signs in, with the form to add somebody.
func (s *Server) handleStaff(w http.ResponseWriter, r *http.Request) {
	s.renderStaff(w, r, http.StatusOK, staffForm{Role: string(access.RoleTechnician)}, "")
}

// handleAddStaff gives somebody a sign-in.
func (s *Server) handleAddStaff(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	f := staffForm{Name: r.FormValue("name"), Email: r.FormValue("email"), Role: r.FormValue("role")}
	_, err := workshop.AddStaff(r.Context(), s.pool, session.Scope, f.Name, f.Email,
		access.Role(f.Role), r.FormValue("password"))
	if errors.Is(err, workshop.ErrInvalid) {
		// Back with what was typed, the password excepted: it is never
		// written into a page.
		s.renderStaff(w, r, http.StatusBadRequest, f, trimInvalid(err))
		return
	}
	if s.staffError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/staff", http.StatusSeeOther)
}

func (s *Server) handleStaffActive(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	err := workshop.SetStaffActive(r.Context(), s.pool, session.Scope, r.PathValue("id"), r.FormValue("active") == "yes")
	if errors.Is(err, workshop.ErrInvalid) {
		s.renderStaff(w, r, http.StatusBadRequest, staffForm{Role: string(access.RoleTechnician)}, trimInvalid(err))
		return
	}
	if s.staffError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/staff", http.StatusSeeOther)
}

func (s *Server) handleStaffPassword(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	err := workshop.SetStaffPassword(r.Context(), s.pool, session.Scope, r.PathValue("id"), r.FormValue("password"))
	if errors.Is(err, workshop.ErrInvalid) {
		s.renderStaff(w, r, http.StatusBadRequest, staffForm{Role: string(access.RoleTechnician)}, trimInvalid(err))
		return
	}
	if s.staffError(w, r, err) {
		return
	}
	http.Redirect(w, r, "/staff", http.StatusSeeOther)
}

// staffForm keeps what was typed into the add form, never the password.
type staffForm struct{ Name, Email, Role string }

func (s *Server) renderStaff(w http.ResponseWriter, r *http.Request, status int, f staffForm, problem string) {
	session := sessionFrom(r.Context())
	staff, err := workshop.Staff(r.Context(), s.pool, session.Scope)
	if s.staffError(w, r, err) {
		return
	}
	s.render(w, r, status, "staff", pageData{
		Title: "Staff", Session: session, Staff: staff, StaffForm: f,
		Roles: workshop.StaffRoles, Error: problem, MinPassword: workshop.MinPasswordLength,
	})
}

func (s *Server) staffError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, access.ErrForbidden):
		s.renderError(w, r, http.StatusForbidden, "Not for your role", "Staff are managed by whoever runs the workshop.")
	case errors.Is(err, workshop.ErrNotFound):
		s.renderError(w, r, http.StatusNotFound, "Not found", "Nobody by that name here.")
	case errors.Is(err, auth.ErrBusy):
		w.Header().Set("Retry-After", "3")
		s.renderError(w, r, http.StatusServiceUnavailable, "The server is busy", "Try again in a moment.")
	default:
		s.log.Error("staff", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "Try again.")
	}
	return true
}

// handleAccount is anybody's own page: for now, their password.
func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "account", s.accountPage(r, sessionFrom(r.Context())))
}

func (s *Server) accountPage(r *http.Request, session auth.Session) pageData {
	mine, err := workshop.MyLocale(r.Context(), s.pool, session.Scope)
	if err != nil {
		s.log.Error("read own language", "error", err)
	}
	sound, err := workshop.AlertSound(r.Context(), s.pool, session.Scope)
	if err != nil {
		s.log.Error("read alert sound", "error", err)
	}
	return pageData{
		Title: "Your account", Session: session, MinPassword: workshop.MinPasswordLength,
		Languages: workshop.Languages(), MyLocale: mine, AlertSound: sound,
	}
}

// handleMyLanguage sets the language the caller reads in. Blank is the
// workshop's.
func (s *Server) handleMyLanguage(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	err := workshop.SetMyLocale(r.Context(), s.pool, session.Scope, r.FormValue("locale"))
	switch {
	case errors.Is(err, workshop.ErrInvalid):
		s.renderError(w, r, http.StatusBadRequest, "Not saved", trimInvalid(err))
		return
	case err != nil:
		s.log.Error("save own language", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "Try again.")
		return
	}
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

// handleChangePassword changes the caller's own password. The session it is
// done from stays; every other one of theirs ends.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	var token string
	if c, err := r.Cookie(sessionCookie); err == nil {
		token = c.Value
	}
	err := auth.ChangePassword(r.Context(), s.pool, session.Scope, token,
		r.FormValue("current"), r.FormValue("password"))
	page := s.accountPage(r, session)
	switch {
	case errors.Is(err, auth.ErrRefused):
		page.Error = trimPrefixRefused(err)
		s.render(w, r, http.StatusBadRequest, "account", page)
		return
	case errors.Is(err, auth.ErrBusy):
		w.Header().Set("Retry-After", "3")
		page.Error = "The server is busy. Try again in a moment."
		s.render(w, r, http.StatusServiceUnavailable, "account", page)
		return
	case err != nil:
		s.log.Error("change password", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "Try again.")
		return
	}
	page.Changed = true
	s.render(w, r, http.StatusOK, "account", page)
}

func trimPrefixRefused(err error) string {
	const p = "auth: refused: "
	msg := err.Error()
	if len(msg) > len(p) && msg[:len(p)] == p {
		return msg[len(p):]
	}
	return msg
}
