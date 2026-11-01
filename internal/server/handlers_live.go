package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/auth"
)

// liveStreams is the most screens held open at once. A workshop has a few
// dozen; past this, a screen is refused and refreshes on an interval.
const liveStreams = 500

// handleEvents is the stream a screen listens on: server-sent events, one
// line per change in the shop, naming what changed and nothing more. The
// screen decides whether the topic is one it shows and, if so, asks for
// itself again through its ordinary handler -- so what reaches it is still
// only what its role may read.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	sub, ok := s.live.Subscribe(session.Scope.ShopID)
	if !ok {
		// Not a failure the screen has to show: it refreshes on an interval
		// instead.
		http.Error(w, "too many open screens; refreshing on an interval instead", http.StatusServiceUnavailable)
		return
	}
	defer s.live.Unsubscribe(sub)

	// The server's write timeout is for responses that end. This one does
	// not, and a proxy in front must not hold it back to send it all at once.
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		s.log.Warn("live: cannot lift the write deadline", "error", err)
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func(format string, args ...any) bool {
		if _, err := fmt.Fprintf(w, format, args...); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	// How long to wait before reconnecting, when the connection drops.
	if !send("retry: 5000\n\n") {
		return
	}

	cookie, _ := r.Cookie(sessionCookie)
	scope := session.Scope
	tick := time.NewTicker(s.liveHeartbeat)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case topic, open := <-sub.C:
			if !open {
				return
			}
			// A module the shop does not run sends nothing (#143): its screens
			// are not found, and the stream says nothing about them either.
			if !scope.Uses(topicModule(topic)...) {
				continue
			}
			if !send("event: change\ndata: %s\n\n", topic) {
				return
			}
		case <-tick.C:
			// A stream does not outlive its session: signed out elsewhere,
			// switched off by the owner, or simply expired. Checked without
			// counting as activity, so the stream cannot keep it alive.
			checked, err := auth.Check(r.Context(), s.pool, s.shop(), cookie.Value)
			if errors.Is(err, auth.ErrNoSession) {
				send("event: signedout\ndata: \n\n")
				return
			}
			if err == nil {
				// The modules as they are now, if the owner switched one.
				scope = checked.Scope
			}
			// A comment line: keeps proxies from closing a quiet connection.
			if !send(": ping\n\n") {
				return
			}
		}
	}
}

// topicModule is the module whose screens a live topic is about. Everything
// a workshop screen listens for is the workshop's; the rota is Core's, which
// Uses answers yes to with no module named.
func topicModule(topic string) []access.Module {
	switch {
	case topic == "board", topic == "calendar", topic == "parts", strings.HasPrefix(topic, "job:"):
		return []access.Module{access.Workshop}
	}
	return nil
}
