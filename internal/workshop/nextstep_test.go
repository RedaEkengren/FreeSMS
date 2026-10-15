package workshop_test

import (
	"slices"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

func TestTheNextStepFollowsTheJob(t *testing.T) {
	full := workshop.StepFacts{HasCustomer: true, HasTechnician: true, HasPromise: true}
	with := func(f workshop.StepFacts, change func(*workshop.StepFacts)) workshop.StepFacts { change(&f); return f }

	for _, c := range []struct {
		name   string
		facts  workshop.StepFacts
		action string
		owner  workshop.Owner
	}{
		{"a draft is priced", with(full, func(f *workshop.StepFacts) { f.State = workshop.StateDraft }), "Price the work", workshop.OwnerDesk},
		{"an estimate is sent", with(full, func(f *workshop.StepFacts) { f.State = workshop.StateEstimated }), "Send the estimate for approval", workshop.OwnerDesk},
		{"a quote is put forward", with(full, func(f *workshop.StepFacts) {
			f.State, f.QuoteKnown, f.PendingLines = workshop.StateAwaitingApproval, true, 2
		}), "Put the quote to the customer", workshop.OwnerDesk},
		{"a quote out waits for the customer", with(full, func(f *workshop.StepFacts) {
			f.State, f.QuoteKnown, f.PendingLines, f.QuoteOut = workshop.StateAwaitingApproval, true, 2, true
		}), "Wait for the customer's answer, or write down what they said by telephone", workshop.OwnerCustomer},
		{"an answered quote is approved", with(full, func(f *workshop.StepFacts) {
			f.State, f.QuoteKnown = workshop.StateAwaitingApproval, true
		}), "Mark it approved", workshop.OwnerDesk},
		{"where the quote is not read it is the customer's", with(full, func(f *workshop.StepFacts) { f.State = workshop.StateAwaitingApproval }),
			"Get the customer's answer", workshop.OwnerCustomer},
		{"an approved job is started", with(full, func(f *workshop.StepFacts) { f.State = workshop.StateApproved }), "Start the work", workshop.OwnerTechnician},
		{"work in progress ends in a final check", with(full, func(f *workshop.StepFacts) {
			f.State, f.FinalRequired = workshop.StateInProgress, true
		}), "Do the final check", workshop.OwnerTechnician},
		{"after the final check it is ready", with(full, func(f *workshop.StepFacts) {
			f.State, f.FinalRequired, f.FinalDone = workshop.StateInProgress, true, true
		}), "Finish the work and mark it ready", workshop.OwnerTechnician},
		{"a part asked for is the parts desk's", with(full, func(f *workshop.StepFacts) {
			f.State, f.OpenRequests = workshop.StateInProgress, 1
		}), "Get the part that was asked for", workshop.OwnerParts},
		{"ready is the counter's", with(full, func(f *workshop.StepFacts) { f.State = workshop.StateReady }),
			"Tell the customer, hand the car over and invoice", workshop.OwnerDesk},
		{"declined is invoiced for what was done", with(full, func(f *workshop.StepFacts) { f.State = workshop.StateDeclined }),
			"Invoice what was done", workshop.OwnerDesk},
		{"declined with nothing done is cancelled", with(full, func(f *workshop.StepFacts) {
			f.State, f.NothingDone = workshop.StateDeclined, true
		}), "Cancel the job: nothing was done", workshop.OwnerDesk},
		{"invoiced waits for the money", with(full, func(f *workshop.StepFacts) { f.State = workshop.StateInvoiced }),
			"Take the payment and close the job", workshop.OwnerDesk},
		{"closed has no next step", with(full, func(f *workshop.StepFacts) { f.State = workshop.StateClosed }), "", ""},
		{"cancelled has none either", workshop.StepFacts{State: workshop.StateCancelled}, "", ""},
	} {
		got := workshop.NextStep(c.facts)
		if got.Action != c.action || got.Owner != c.owner {
			t.Errorf("%s: %q by %q, want %q by %q", c.name, got.Action, got.Owner, c.action, c.owner)
		}
	}
}

// What is unknown is said, one thing at a time, and never for a finished job.
func TestTheNextStepSaysWhatIsMissing(t *testing.T) {
	got := workshop.NextStep(workshop.StepFacts{State: workshop.StateDraft}).Missing
	if want := []string{"no customer", "nobody on it", "no promised time"}; !slices.Equal(got, want) {
		t.Errorf("missing %v, want %v", got, want)
	}
	if got := workshop.NextStep(workshop.StepFacts{State: workshop.StateDraft, HasCustomer: true, HasTechnician: true, HasPromise: true}).Missing; len(got) != 0 {
		t.Errorf("a complete job is missing %v", got)
	}
	if got := workshop.NextStep(workshop.StepFacts{State: workshop.StateClosed}).Missing; len(got) != 0 {
		t.Errorf("a closed job is missing %v", got)
	}
}
