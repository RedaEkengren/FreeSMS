package workshop

import (
	"strings"
	"testing"
)

// The case the whole state machine exists for: a customer who says no once the
// car is already in pieces still owes the diagnostic time, so declined leads
// to an invoice rather than to closed.
func TestDeclinedAfterTeardownIsStillBillable(t *testing.T) {
	if !CanTransition(StateInProgress, StateDeclined) {
		t.Error("a job in progress cannot be declined; a customer changing their mind mid-strip has nowhere to go")
	}
	if !CanTransition(StateDeclined, StateInvoiced) {
		t.Error("a declined job cannot be invoiced; declined would mean the work was free")
	}
}

func TestClosedIsTerminal(t *testing.T) {
	if len(NextStates(StateClosed)) != 0 {
		t.Errorf("a closed order can still move to %v; reopening one must be a new order", NextStates(StateClosed))
	}
	if len(NextStates(StateCancelled)) != 0 {
		t.Errorf("a cancelled order can still move to %v", NextStates(StateCancelled))
	}
}

// A car handed back for more work is on a lift again, not finished.
func TestReadyCanGoBackToTheWorkshop(t *testing.T) {
	if !CanTransition(StateReady, StateInProgress) {
		t.Error("a car that has been handed back cannot be worked on again")
	}
}

func TestIllegalTransitionsAreRefused(t *testing.T) {
	for _, c := range []struct{ from, to State }{
		{StateDraft, StateInvoiced},
		{StateDraft, StateApproved},
		{StateAwaitingApproval, StateReady},
		{StateInvoiced, StateInProgress},
		{StateClosed, StateDraft},
	} {
		if CanTransition(c.from, c.to) {
			t.Errorf("%s -> %s is allowed and should not be", c.from, c.to)
		}
	}
}

// The error has to say what can be done, not only what cannot. A refusal that
// leaves somebody guessing gets worked around.
func TestIllegalTransitionErrorNamesTheAlternatives(t *testing.T) {
	err := ErrIllegalTransition{From: StateDraft, To: StateInvoiced}
	msg := err.Error()
	for _, want := range []string{"draft", "invoiced", "estimated"} {
		if !contains(msg, want) {
			t.Errorf("the error does not mention %q: %s", want, msg)
		}
	}

	terminal := ErrIllegalTransition{From: StateClosed, To: StateDraft}
	if !contains(terminal.Error(), "new order") {
		t.Errorf("the closed-order error does not say what to do instead: %s", terminal.Error())
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

// A button that always fails teaches the person tapping it that the interface
// does not know what it is doing.
func TestCancelIsNotOfferedOnAnOrderWithWork(t *testing.T) {
	withWork := AvailableStates(StateDraft, true)
	for _, s := range withWork {
		if s == StateCancelled {
			t.Error("cancelled is offered on an order that has work on it, and would be refused")
		}
	}
	if len(withWork) == 0 {
		t.Error("an order with work has nowhere to go at all")
	}

	empty := AvailableStates(StateDraft, false)
	var sawCancel bool
	for _, s := range empty {
		if s == StateCancelled {
			sawCancel = true
		}
	}
	if !sawCancel {
		t.Error("cancelled is not offered on an empty draft, where it is the right action")
	}
}

// Every state the column allows needs words, including the rare ones. A switch
// that omits cancelled works for months and then puts "cancelled" on a screen
// in front of a customer.
//
// The list is taken from transitions, which is the same set as the CHECK
// constraint, so a state added to the machine without words fails here.
func TestEveryStateHasWords(t *testing.T) {
	for state := range transitions {
		if _, ok := stateLabels[state]; !ok {
			t.Errorf("state %q has no label; the screen would show the identifier", state)
		}
		if _, ok := stateActions[state]; !ok {
			t.Errorf("state %q has no action; a button would be labelled with the identifier", state)
		}
	}
	// The two maps describe the same machine, so neither may carry a state the
	// machine does not have.
	for state := range stateLabels {
		if _, ok := transitions[state]; !ok {
			t.Errorf("stateLabels has %q, which is not a state", state)
		}
	}
	for state := range stateActions {
		if _, ok := transitions[state]; !ok {
			t.Errorf("stateActions has %q, which is not a state", state)
		}
	}
}

// Nothing a person reads may be an identifier. The underscore is the tell:
// every one of these values has one, and no sentence in the catalogue does.
func TestNoLabelIsAnIdentifier(t *testing.T) {
	for state := range transitions {
		for name, got := range map[string]string{
			"Label":  state.Label(),
			"Action": state.Action(),
		} {
			if got == string(state) {
				t.Errorf("%s(%q) returned the identifier itself", name, state)
			}
			if strings.Contains(got, "_") {
				t.Errorf("%s(%q) = %q, which is an identifier and not words", name, state, got)
			}
		}
	}
}

// A state nobody planned for must still render something. An empty badge looks
// like the order has no state at all, which is a worse lie than an odd word.
func TestAnUnknownStateFallsBackToItsValue(t *testing.T) {
	unknown := State("impounded")
	if got := unknown.Label(); got != "impounded" {
		t.Errorf("Label = %q, want the raw value", got)
	}
	if got := unknown.Action(); got != "impounded" {
		t.Errorf("Action = %q, want the raw value", got)
	}
}
