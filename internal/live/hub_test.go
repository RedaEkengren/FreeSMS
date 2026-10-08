package live

import (
	"io"
	"log/slog"
	"testing"
)

func TestASubscriptionHearsItsShopAndNoOther(t *testing.T) {
	h := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), 10)
	a, _ := h.Subscribe("A")
	b, _ := h.Subscribe("B")
	h.Publish("A", "board")
	if got := <-a.C; got != "board" {
		t.Errorf("A heard %q", got)
	}
	select {
	case got := <-b.C:
		t.Errorf("B heard A's %q", got)
	default:
	}
}

// A screen that is not keeping up is not waited for: what it missed is
// replaced by "everything", which covers it.
func TestASlowScreenIsToldEverythingRatherThanWaitedFor(t *testing.T) {
	h := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), 10)
	s, _ := h.Subscribe("A")
	for i := 0; i < cap(s.C)+5; i++ {
		h.Publish("A", "board")
	}
	last := ""
	for len(s.C) > 0 {
		last = <-s.C
	}
	if last != Everything {
		t.Errorf("a full screen was last told %q, want %q", last, Everything)
	}
}

func TestTheHubRefusesPastItsLimitAndAfterStopping(t *testing.T) {
	h := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), 1)
	s, ok := h.Subscribe("A")
	if !ok {
		t.Fatal("the first screen was refused")
	}
	if _, ok := h.Subscribe("A"); ok {
		t.Error("a screen past the limit was taken")
	}
	h.stop()
	if _, open := <-s.C; open {
		t.Error("stopping left a stream open")
	}
	h.Unsubscribe(s) // after stopping: must not close twice
	if _, ok := h.Subscribe("A"); ok {
		t.Error("a stopped hub took a screen")
	}
}
