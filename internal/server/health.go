package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/ops"
)

type health struct {
	Status   string `json:"status"`
	Release  string `json:"release"`
	Database string `json:"database"`
}

// handleHealth reports whether the service can do its job, not whether the
// process is running.
//
// The distinction is the point. A health check that returns ok unconditionally
// tells a deploy that the container started, which it could already see. This
// one reaches the database, so a deploy onto an unreachable database fails
// loudly instead of going green and waiting for the first customer to find it.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	body := health{Status: "healthy", Release: s.release, Database: "up"}
	code := http.StatusOK

	if err := s.pool.Ping(ctx); err != nil {
		body.Status = "unhealthy"
		body.Database = "down"
		code = http.StatusServiceUnavailable
		s.log.Error("health check failed", "error", err)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// handleOpsStatus answers a monitor: not whether the process is up -- /healthz
// says that -- but whether it is doing what it has to. Messages that are past
// their time and have not gone, messages given up on, a disk photographs can no
// longer be written to or that is nearly full. 503 when anything is wrong, so
// the simplest possible check (curl --fail) is a usable alert.
//
// It needs OPS_TOKEN as a bearer token and is off when none is set. Nothing in
// the answer is about a person: counts and ages, never a name or an address.
func (s *Server) handleOpsStatus(w http.ResponseWriter, r *http.Request) {
	if s.opsToken == "" {
		http.NotFound(w, r)
		return
	}
	got := sha256.Sum256([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
	want := sha256.Sum256([]byte(s.opsToken))
	if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="freesms-ops"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	st := ops.StatusOf(r.Context(), s.pool, s.shop(), s.release, s.attachmentsDir)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if !st.OK {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(st)
}
