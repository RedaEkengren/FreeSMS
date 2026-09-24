package server

import (
	"errors"
	"net/http"

	"github.com/RedaEkengren/FreeSMS/internal/auth"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// needsSetup reports whether this installation still has no shop.
//
// Asked per request rather than once at startup, because setup happens while
// the process is running: the answer changes underneath it exactly once, and a
// value read at boot would be wrong from then on.
//
// Once a shop is found it is remembered. The question is only asked while the
// answer can still be yes, so this is not a hot path that needs invalidating.
func (s *Server) needsSetup(r *http.Request) bool {
	if s.shop() != "" {
		return false
	}
	id, err := workshop.ResolveShop(r.Context(), s.pool, s.configuredShopID)
	if err != nil {
		return true
	}
	s.setShop(id)
	return false
}

func (s *Server) handleSetupForm(w http.ResponseWriter, r *http.Request) {
	if !s.needsSetup(r) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "setup", pageData{
		Title:       "Set up this workshop",
		MinPassword: workshop.MinPasswordLength,
	})
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	form := setupForm{
		ShopName:  r.FormValue("shop_name"),
		OwnerName: r.FormValue("owner_name"),
		Email:     r.FormValue("email"),
	}
	password := r.FormValue("password")

	shopID, err := workshop.Setup(r.Context(), s.pool, form.ShopName, form.OwnerName, form.Email, password)
	switch {
	case errors.Is(err, workshop.ErrAlreadySetUp):
		// Somebody else finished first, or the page was left open. Either way
		// the honest answer is that it is done.
		s.setShop("")
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	case errors.Is(err, workshop.ErrInvalid):
		s.render(w, r, http.StatusBadRequest, "setup", pageData{
			Title:       "Set up this workshop",
			MinPassword: workshop.MinPasswordLength,
			Form:        form,
			Error:       trimInvalid(err),
		})
		return
	case err != nil:
		s.log.Error("setup", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "Try again.")
		return
	}

	s.setShop(shopID)
	s.log.Info("workshop set up", "shop_id", shopID)

	// Sign the new owner in rather than sending them to a login form with the
	// password they typed ten seconds ago still in their head.
	token, session, err := auth.Login(r.Context(), s.pool, shopID, form.Email, password, r.UserAgent())
	if err != nil {
		s.log.Error("sign in after setup", "error", err)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.setSessionCookie(w, token, session)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// requireSetup sends everything to the setup page until there is a shop.
//
// Without this the service either refuses to start -- which it used to, so a
// first `docker compose up` produced a container restarting in a loop and a
// reason buried in a log -- or serves a sign-in page against a database with
// nobody in it.
func (s *Server) requireSetup(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz", "/setup":
			next.ServeHTTP(w, r)
			return
		}
		if s.needsSetup(r) {
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func trimInvalid(err error) string {
	const prefix = "workshop: invalid: "
	msg := err.Error()
	if len(msg) > len(prefix) && msg[:len(prefix)] == prefix {
		return msg[len(prefix):]
	}
	return msg
}
