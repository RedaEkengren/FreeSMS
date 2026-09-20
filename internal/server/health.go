package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
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
