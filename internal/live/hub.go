// Package live tells open screens that something changed.
//
// A committed change notifies Postgres's freesms_changes channel with the
// shop and what it touched -- "job:<id>", "board", "parts", "calendar" -- and
// never a value (migrations/0037_live.sql). The hub holds one connection
// listening on that channel and hands each notification to the open streams
// of that shop. A screen that hears its topic asks for itself again through
// its ordinary handler, so what it shows is still what its role may read.
package live

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Channel is the Postgres channel the triggers notify.
const Channel = "freesms_changes"

// Everything is the topic sent when the hub may have missed something -- it
// reconnected to the database -- so every screen asks again.
const Everything = "*"

// Hub fans notifications out to subscribers, per shop.
type Hub struct {
	pool *pgxpool.Pool
	log  *slog.Logger
	// The most streams held at once. Every one is a connection and a
	// goroutine; past this a screen is refused and polls instead.
	max int

	mu   sync.Mutex
	subs map[string]map[*Subscription]struct{}
	n    int
	done bool
}

// Subscription is one open screen's share of a shop's notifications.
type Subscription struct {
	// Topics, in the order they arrived. Closed when the hub stops.
	C    chan string
	shop string
}

// New makes a hub. It does nothing until Run.
func New(pool *pgxpool.Pool, log *slog.Logger, max int) *Hub {
	return &Hub{pool: pool, log: log, max: max, subs: map[string]map[*Subscription]struct{}{}}
}

// Subscribe opens a subscription to a shop's changes, or reports false when
// the hub is full or stopped.
func (h *Hub) Subscribe(shop string) (*Subscription, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.done || h.n >= h.max {
		return nil, false
	}
	s := &Subscription{C: make(chan string, 32), shop: shop}
	if h.subs[shop] == nil {
		h.subs[shop] = map[*Subscription]struct{}{}
	}
	h.subs[shop][s] = struct{}{}
	h.n++
	return s, true
}

// Unsubscribe ends a subscription. Safe to call after the hub has stopped.
func (h *Hub) Unsubscribe(s *Subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[s.shop][s]; !ok {
		return
	}
	delete(h.subs[s.shop], s)
	if len(h.subs[s.shop]) == 0 {
		delete(h.subs, s.shop)
	}
	h.n--
	close(s.C)
}

// Publish hands a topic to every subscription in a shop; shop "" is every
// shop. A subscription that is not keeping up is told to ask for everything
// rather than being blocked on.
func (h *Hub) Publish(shop, topic string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s, set := range h.subs {
		if shop != "" && s != shop {
			continue
		}
		for sub := range set {
			select {
			case sub.C <- topic:
			default:
				// Full: drop the oldest and say "everything", which covers
				// whatever was dropped.
				select {
				case <-sub.C:
				default:
				}
				select {
				case sub.C <- Everything:
				default:
				}
			}
		}
	}
}

// Run listens until ctx ends, reconnecting when the connection drops, then
// closes every subscription so the streams on them return.
func (h *Hub) Run(ctx context.Context) {
	defer h.stop()
	missed := false
	for ctx.Err() == nil {
		err := h.listen(ctx, missed)
		if ctx.Err() != nil {
			return
		}
		h.log.Warn("live: listening stopped, trying again", "error", err)
		missed = true
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (h *Hub) listen(ctx context.Context, missed bool) error {
	c, err := h.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	// Held for as long as it listens, so it leaves the pool for good: a
	// connection given back would carry the LISTEN to whoever had it next.
	conn := c.Hijack()
	defer conn.Close(context.WithoutCancel(ctx))
	if _, err := conn.Exec(ctx, "LISTEN "+Channel); err != nil {
		return err
	}
	if missed {
		// Whatever happened while nobody was listening is unknown.
		h.Publish("", Everything)
	}
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		shop, topic, ok := strings.Cut(n.Payload, " ")
		if !ok || shop == "" {
			continue
		}
		h.Publish(shop, topic)
	}
}

func (h *Hub) stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.done = true
	for shop, set := range h.subs {
		for s := range set {
			close(s.C)
		}
		delete(h.subs, shop)
	}
	h.n = 0
}
