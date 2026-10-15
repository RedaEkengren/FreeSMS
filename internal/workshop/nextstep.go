package workshop

// Owner is who the next step on a job is waiting for.
type Owner string

const (
	OwnerDesk       Owner = "desk"
	OwnerTechnician Owner = "technician"
	OwnerCustomer   Owner = "customer"
	OwnerParts      Owner = "parts"
)

// Label is the owner as a catalogue key.
func (o Owner) Label() string {
	switch o {
	case OwnerDesk:
		return "the front desk"
	case OwnerTechnician:
		return "the technician"
	case OwnerCustomer:
		return "the customer"
	case OwnerParts:
		return "the parts desk"
	}
	return ""
}

// StepFacts is what NextStep needs to know about a job. All of it is already
// read for the job page or the board; none of it is customer personal data, so
// a technician can be shown the answer.
//
// Fields the caller cannot know are left at their zero value and the step
// falls back to what the state alone says. The board does not read quotes, for
// instance, and the technician's page does not either.
type StepFacts struct {
	State State

	HasCustomer   bool
	HasTechnician bool
	HasPromise    bool

	// The customer-borne lines nobody has answered for, and whether a quote
	// has been put forward. QuoteKnown is false where this was not read.
	QuoteKnown    bool
	PendingLines  int
	QuoteOut      bool
	FinalRequired bool
	FinalDone     bool
	OpenRequests  int
}

// Step is where a job stands and what happens next.
type Step struct {
	// The stage, as a catalogue key.
	Stage string
	// What is not known about the job yet, as catalogue keys. Never blocks
	// anything by itself: a car arrives with whatever is known, and the action
	// that needs a fact is the one that insists on it.
	Missing []string
	// What to do next, as a catalogue key, and who does it. Empty when the job
	// is finished.
	Action string
	Owner  Owner
}

// NextStep says where a job is and what to do next. It is derived each time
// from facts that are stored anyway, and stored nowhere itself: a column
// saying "next step" would have to be kept in step with everything it is
// computed from, and the day it was not, it would be believed.
func NextStep(f StepFacts) Step {
	st := Step{Stage: f.State.Label()}
	switch f.State {
	case StateClosed, StateCancelled:
		return st
	}
	if !f.HasCustomer {
		st.Missing = append(st.Missing, "no customer")
	}
	if !f.HasTechnician {
		st.Missing = append(st.Missing, "nobody on it")
	}
	if !f.HasPromise {
		st.Missing = append(st.Missing, "no promised time")
	}

	set := func(action string, o Owner) Step { st.Action, st.Owner = action, o; return st }
	switch f.State {
	case StateDraft:
		return set("Price the work", OwnerDesk)
	case StateEstimated:
		return set("Send the estimate for approval", OwnerDesk)
	case StateAwaitingApproval:
		switch {
		case f.QuoteKnown && f.PendingLines > 0 && !f.QuoteOut:
			return set("Put the quote to the customer", OwnerDesk)
		case f.QuoteKnown && f.PendingLines > 0:
			return set("Wait for the customer's answer, or write down what they said by telephone", OwnerCustomer)
		case f.QuoteKnown:
			return set("Mark it approved", OwnerDesk)
		}
		return set("Get the customer's answer", OwnerCustomer)
	case StateApproved:
		return set("Start the work", OwnerTechnician)
	case StateInProgress:
		if f.OpenRequests > 0 {
			return set("Get the part that was asked for", OwnerParts)
		}
		if f.FinalRequired && !f.FinalDone {
			return set("Do the final check", OwnerTechnician)
		}
		return set("Finish the work and mark it ready", OwnerTechnician)
	case StateAwaitingParts:
		return set("Get the part that was asked for", OwnerParts)
	case StateReady:
		return set("Tell the customer, hand the car over and invoice", OwnerDesk)
	case StateDeclined:
		return set("Invoice what was done", OwnerDesk)
	case StateInvoiced:
		return set("Take the payment and close the job", OwnerDesk)
	}
	return st
}
