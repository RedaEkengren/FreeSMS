package workshop

import "testing"

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
