package server

import (
	"math"
	"sync"
	"time"
)

// bucket is a token bucket: it lets burst requests through at once and
// refills at rate a second, so a caller that keeps within the rate is never
// refused and one that hammers is told how long to wait. It is one for the
// whole installation, not one for each caller: behind the reverse proxy
// every request has the proxy's address, and a header naming the caller is
// whatever the caller writes, so a limit by address would be one nobody
// could rely on. It is there to keep a public feed from being what takes
// the database down (#183); a site that sends If-None-Match costs one query
// and stays well inside it.
//
// A nil bucket lets everything through.
type bucket struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
	rate   float64
	burst  float64
	now    func() time.Time
}

func newBucket(ratePerSecond float64, burst int, now func() time.Time) *bucket {
	return &bucket{tokens: float64(burst), last: now(), rate: ratePerSecond, burst: float64(burst), now: now}
}

// take spends a token. When there is none it says how long until there is.
func (b *bucket) take() (ok bool, wait time.Duration) {
	if b == nil {
		return true, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	at := b.now()
	if elapsed := at.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(b.burst, b.tokens+elapsed*b.rate)
	}
	b.last = at
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / b.rate * float64(time.Second))
}
