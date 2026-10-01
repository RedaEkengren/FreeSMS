package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// IdempotencyHeader is what a client sends to make a repeat safe.
const IdempotencyHeader = "Idempotency-Key"

// idempotent answers a repeated request with what the first one did.
//
// A flaky connection means the same request arrives twice: the phone gave up
// waiting, somebody pressed again, the offline queue replayed after coming
// back. Without this each one opens a second job, and the workshop finds out
// when two identical orders appear on the board.
//
// Only requests carrying a key are affected. A form posted from a browser
// without one behaves exactly as before.
func (s *Server) idempotent(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get(IdempotencyHeader)
		if key == "" || r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		session := sessionFrom(r.Context())
		if session.Scope.UserID == "" {
			next.ServeHTTP(w, r)
			return
		}

		// The body is read here and handed back, because deciding whether this
		// is the same request means hashing what it says.
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "could not read the request", http.StatusBadRequest)
			return
		}
		r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))

		fingerprint := workshop.Fingerprint(r.Method, r.URL.Path, session.Scope.UserID, body)
		replay, err := workshop.ClaimIdempotency(r.Context(), s.pool, session.Scope, key, fingerprint)
		switch {
		case errors.Is(err, workshop.ErrKeyReused):
			// Answering the second question with the first answer would be
			// worse than refusing.
			http.Error(w, "that idempotency key was used for a different request", http.StatusConflict)
			return
		case errors.Is(err, workshop.ErrInFlight):
			// Another copy is being carried out right now. A 503 is what the
			// offline queue reads as "ask again later", which is exactly right.
			w.Header().Set("Retry-After", "2")
			http.Error(w, "that request is still being carried out", http.StatusServiceUnavailable)
			return
		case errors.Is(err, workshop.ErrOutcomeUnknown):
			// A 4xx, so the queue stops and tells a person. Nobody can say
			// whether it happened, and guessing about money is not acceptable.
			http.Error(w, "that request was started and its outcome is unknown; check the job before trying again", http.StatusConflict)
			return
		case errors.Is(err, workshop.ErrInvalid):
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		case err != nil:
			// The client asked for a guarantee. If it cannot be given, the
			// work is not done without it -- this used to carry on regardless,
			// which is the one outcome the key exists to prevent.
			s.log.Error("idempotency claim", "error", err)
			w.Header().Set("Retry-After", "5")
			http.Error(w, "could not check that request; try again", http.StatusServiceUnavailable)
			return
		}

		if replay != nil {
			if replay.Location != "" {
				w.Header().Set("Location", replay.Location)
			}
			// The same answer as the first time, including the redirect, so a
			// replayed request lands where the original did.
			w.Header().Set("Idempotent-Replay", "true")
			w.WriteHeader(replay.Status)
			return
		}

		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)

		// Detached from the request. A client that hangs up after the work has
		// committed cancels r.Context(), and recording with it failed -- so the
		// retry found no record and did the work a second time. The outcome has
		// to be written whether or not anybody is still listening.
		ctx := context.WithoutCancel(r.Context())
		if recorder.status < 400 {
			if err := workshop.CompleteIdempotency(ctx, s.pool, session.Scope,
				key, recorder.status, recorder.Header().Get("Location")); err != nil {
				s.log.Error("complete idempotency", "error", err)
			}
			return
		}
		// A failure is given back, so that trying again may succeed.
		if err := workshop.ReleaseIdempotency(ctx, s.pool, session.Scope, key); err != nil {
			s.log.Error("release idempotency", "error", err)
		}
	})
}

// statusRecorder remembers what was written, so the middleware can store it.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if !r.written {
		r.status, r.written = status, true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.written {
		r.written = true
	}
	return r.ResponseWriter.Write(b)
}
