package workshop

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
)

// The queue's order for a mixed set, pinned: urgency, then due time --
// something due before something not -- then module, then job number,
// then as given. And a provider that fails leaves only its own items out,
// a module switched off none, and the same item from two roles' providers
// is there once.
func TestTheQueuesOrderAndItsFailures(t *testing.T) {
	soon := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	later := soon.Add(time.Hour)
	give := func(items ...ActionItem) func(context.Context, *pgxpool.Pool, access.Scope) ([]ActionItem, error) {
		return func(context.Context, *pgxpool.Pool, access.Scope) ([]ActionItem, error) { return items, nil }
	}
	saved := actionProviders
	defer func() { actionProviders = saved }()
	actionProviders = []ActionProvider{
		{Name: "core", Items: give(ActionItem{Kind: "c1", Urgency: UrgencySoon}, ActionItem{Kind: "c0", Urgency: UrgencyNow})},
		{Module: access.Rental, Name: "rental", Items: give(ActionItem{Kind: "r1", Module: access.Rental, Urgency: UrgencySoon, Due: later},
			ActionItem{Kind: "r0", Module: access.Rental, Urgency: UrgencySoon, Due: soon})},
		{Module: access.Dealer, Name: "dealer", Items: func(context.Context, *pgxpool.Pool, access.Scope) ([]ActionItem, error) {
			return []ActionItem{{Kind: "lost"}}, errors.New("the dealer's query failed")
		}},
		{Module: access.Workshop, Name: "workshop", Items: give(ActionItem{Kind: "w2", Module: access.Workshop, Number: 2, Urgency: UrgencySoon},
			ActionItem{Kind: "w1", Module: access.Workshop, Number: 1, Urgency: UrgencySoon},
			ActionItem{Kind: "w1", Module: access.Workshop, Number: 1, Urgency: UrgencySoon})},
	}
	scope := access.Scope{ShopID: "s", UserID: "u", Role: access.Of(access.RoleOwner), Modules: access.Modules}
	items, err := WaitingFor(context.Background(), nil, scope)
	var incomplete *IncompleteError
	if !errors.As(err, &incomplete) || len(incomplete.Failed) != 1 {
		t.Errorf("a provider failed: %v, want an IncompleteError naming it", err)
	}
	var got []string
	for _, it := range items {
		got = append(got, it.Kind)
	}
	want := "c0 r0 r1 c1 w1 w2"
	if s := join(got); s != want {
		t.Errorf("the order: %s, want %s", s, want)
	}
	scope.Modules = []access.Module{access.Workshop}
	items, err = WaitingFor(context.Background(), nil, scope)
	if err != nil || join(kindsOf(items)) != "c0 c1 w1 w2" {
		t.Errorf("rental and dealer switched off: %v, %v", kindsOf(items), err)
	}
}

func kindsOf(items []ActionItem) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.Kind)
	}
	return out
}

func join(s []string) string {
	out := ""
	for i, k := range s {
		if i > 0 {
			out += " "
		}
		out += k
	}
	return out
}
