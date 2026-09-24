package server

import (
	"bytes"
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

		replay, err := workshop.CheckIdempotency(r.Context(), s.pool, session.Scope, key, body)
		switch {
		case errors.Is(err, workshop.ErrKeyReused):
			// Answering the second question with the first answer would be
			// worse than refusing.
			http.Error(w, "that idempotency key was used for a different request", http.StatusConflict)
			return
		case errors.Is(err, workshop.ErrInvalid):
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		case err != nil:
			s.log.Error("idempotency check", "error", err)
			next.ServeHTTP(w, r)
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

		// Only successful requests are remembered. A repeat of something that
		// failed should be allowed to succeed.
		if recorder.status < 400 {
			if err := workshop.RecordIdempotency(r.Context(), s.pool, session.Scope,
				key, body, recorder.status, recorder.Header().Get("Location")); err != nil {
				s.log.Error("record idempotency", "error", err)
			}
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
