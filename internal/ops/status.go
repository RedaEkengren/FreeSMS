package ops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// Status is what a monitor needs to know about a running service that /healthz
// does not say: not that the process answers, but that the things it has to do
// are being done. A service can be up and not sending the messages it promised,
// or up with a disk it can no longer write a photograph to.
type Status struct {
	OK         bool      `json:"ok"`
	Problems   []string  `json:"problems"`
	Release    string    `json:"release"`
	Migrations int       `json:"migrations"`
	CheckedAt  time.Time `json:"checked_at"`
	Queue      Queue     `json:"queue"`
	Storage    Storage   `json:"storage"`
}

// Queue is the messages that are meant to go.
type Queue struct {
	// Waiting for the sender, and past the time they were meant to go by more
	// than the sender's own interval and a margin: something is not sending.
	DueNotSent int `json:"due_not_sent"`
	// The age of the oldest of those.
	OldestDueSeconds int64 `json:"oldest_due_seconds"`
	// Given up on in the last day.
	FailedLastDay int `json:"failed_last_day"`
}

// Storage is where photographs go.
type Storage struct {
	Writable  bool   `json:"writable"`
	FreeBytes uint64 `json:"free_bytes"`
	Error     string `json:"error,omitempty"`
}

// How late a message may be before it is a problem: the sender looks every
// twenty seconds, so ten minutes is not a busy moment.
const dueGrace = 10 * time.Minute

// Below this much free, the next big day of photographs may not fit.
const lowDisk = 500 << 20

// StatusOf reads the state of a running service.
func StatusOf(ctx context.Context, pool *pgxpool.Pool, shopID, release, attachmentsDir string) Status {
	st := Status{CheckedAt: time.Now(), Release: release, Problems: []string{}}
	bad := func(format string, a ...any) { st.Problems = append(st.Problems, fmt.Sprintf(format, a...)) }

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&st.Migrations); err != nil {
		bad("the database does not answer")
	} else if shopID != "" {
		err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				WITH last AS (
				  SELECT m.id, m.not_before,
				         (SELECT e.event FROM message_events e WHERE e.message_id = m.id ORDER BY e.at DESC LIMIT 1) AS event,
				         (SELECT e.at FROM message_events e WHERE e.message_id = m.id ORDER BY e.at DESC LIMIT 1) AS at
				  FROM outbound_messages m WHERE m.recipient <> ''
				)
				SELECT count(*) FILTER (WHERE event IN ('queued', 'retry') AND not_before < now() - $1::interval)::int,
				       coalesce(max(extract(epoch FROM now() - not_before)) FILTER (WHERE event IN ('queued', 'retry') AND not_before < now() - $1::interval), 0)::bigint,
				       count(*) FILTER (WHERE event = 'failed' AND at > now() - interval '1 day')::int
				FROM last`, fmt.Sprintf("%d seconds", int(dueGrace.Seconds()))).
				Scan(&st.Queue.DueNotSent, &st.Queue.OldestDueSeconds, &st.Queue.FailedLastDay)
		})
		if err != nil {
			bad("the message queue cannot be read")
		}
		if st.Queue.DueNotSent > 0 {
			bad("%d messages are past their time and have not gone; the oldest by %d minutes", st.Queue.DueNotSent, st.Queue.OldestDueSeconds/60)
		}
		if st.Queue.FailedLastDay > 0 {
			bad("%d messages were given up on in the last day", st.Queue.FailedLastDay)
		}
	}

	st.Storage = probeStorage(attachmentsDir)
	if !st.Storage.Writable {
		bad("photographs cannot be written to %s: %s", attachmentsDir, st.Storage.Error)
	} else if st.Storage.FreeBytes < lowDisk {
		bad("only %d MB is free where photographs are kept", st.Storage.FreeBytes>>20)
	}
	st.OK = len(st.Problems) == 0
	return st
}

// probeStorage writes a small file and takes it away again, which is what a
// photograph will need to do, and reads how much room is left.
func probeStorage(dir string) Storage {
	var s Storage
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		s.Error = err.Error()
		return s
	}
	name := f.Name()
	_, werr := f.WriteString("probe")
	cerr := f.Close()
	rerr := os.Remove(name)
	for _, e := range []error{werr, cerr, rerr} {
		if e != nil {
			s.Error = e.Error()
			return s
		}
	}
	s.Writable = true
	var fs syscall.Statfs_t
	if err := syscall.Statfs(filepath.Clean(dir), &fs); err == nil {
		s.FreeBytes = uint64(fs.Bavail) * uint64(fs.Bsize)
	}
	return s
}
