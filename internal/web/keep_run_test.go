package web

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"
)

// keepRun runs one scenario of testdata/keep_harness.js against keep.js.
//
// The other keep.js tests read the source. These run it, because the bug they
// guard against was in what happens and when -- a draft deleted on the press
// of a button -- and no reading of the text says that.
//
// Node is on every GitHub runner, so in CI a missing one is a failure, not a
// skip: a test that quietly never runs is worse than none.
func keepRun(t *testing.T, scenario string, out any) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node is not installed, and these tests are required in CI")
		}
		t.Skip("node is not installed")
	}
	raw, err := exec.Command(node, "testdata/keep_harness.js", scenario, "static/keep.js").CombinedOutput()
	if err != nil {
		t.Fatalf("harness %s: %v\n%s", scenario, err, raw)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("harness %s said %s: %v", scenario, raw, err)
	}
}

// Pressing submit is not saving. The draft used to be deleted, locally and on
// the server, the moment the button was pressed -- so a refusal, an error page
// or a dropped connection left nothing to recover -- and the autosave still
// waiting to fire then brought the submitted draft back.
func TestSubmittingDoesNotDeleteTheDraft(t *testing.T) {
	var got struct {
		Draft   *string
		Deletes int
		Saves   int
		Tagged  bool
	}
	keepRun(t, "submit", &got)
	if got.Draft == nil {
		t.Error("the local draft was deleted on submit, before the server answered")
	}
	if got.Deletes != 0 {
		t.Errorf("submit asked the server to delete the draft %d times", got.Deletes)
	}
	if got.Saves != 0 {
		t.Errorf("an autosave fired after submit (%d); it lands after the save and resurrects the draft", got.Saves)
	}
	if !got.Tagged {
		t.Error("the form does not name its draft, so the server cannot say which one it saved")
	}
}

// The server's receipt is what lets a draft go, and every draft it names.
func TestAReceiptDeletesTheDraftsItNames(t *testing.T) {
	var got struct {
		Draft   *string
		Deletes []string
		Cookie  string
	}
	keepRun(t, "receipt", &got)
	if got.Draft != nil {
		t.Errorf("the draft survived its receipt: %s", *got.Draft)
	}
	if len(got.Deletes) != 2 {
		t.Errorf("deletes = %v, want one per name in the receipt", got.Deletes)
	}
	if got.Cookie != "other=1" {
		t.Errorf("cookie after reading = %q; the receipt should be gone and nothing else touched", got.Cookie)
	}
}

// No receipt -- a refusal, an error page -- and the draft stays.
func TestNoReceiptKeepsTheDraft(t *testing.T) {
	var got struct {
		Draft   *string
		Deletes int
	}
	keepRun(t, "refusal", &got)
	if got.Draft == nil || got.Deletes != 0 {
		t.Errorf("a page with no receipt deleted the draft (draft %v, %d deletes)", got.Draft, got.Deletes)
	}
}

// Every control that belongs to a kept form is kept, wherever it sits.
//
// The job line's fields live in the table and are joined to an empty form by
// the form attribute. Their events travel up through the table, never through
// the form, and the listener was on the form -- so typing a job line kept
// nothing, locally or on the server.
func TestAControlJoinedByTheFormAttributeIsKept(t *testing.T) {
	type kept struct {
		Local *string
		Saves int
	}
	var got struct{ Inside, Outside, Stranger kept }
	keepRun(t, "edits", &got)

	if got.Inside.Local == nil || got.Inside.Saves != 1 {
		t.Errorf("a control inside the form was not kept: %+v", got.Inside)
	}
	if got.Outside.Local == nil || got.Outside.Saves != 1 {
		t.Errorf("a control joined by the form attribute was not kept: %+v", got.Outside)
	}
	if got.Stranger.Local != nil || got.Stranger.Saves != 0 {
		t.Errorf("typing in another form saved this form's draft: %+v", got.Stranger)
	}
}

// The server's copy fills a field still holding what the page arrived with,
// not only an empty one. A job line's quantity starts at 1, its price at 0 and
// its selects on a choice, so a line restored from the server -- the phone
// lost, the browser cleared -- came back as its description alone.
func TestTheServersDraftFillsUntouchedFields(t *testing.T) {
	var got struct {
		Fresh struct{ Complaint, Quantity string }
		Typed struct{ Quantity string }
	}
	keepRun(t, "fromServer", &got)
	if got.Fresh.Complaint != "Rattles" || got.Fresh.Quantity != "1,5" {
		t.Errorf("restored %+v, want the complaint and the quantity of 1,5", got.Fresh)
	}
	if got.Typed.Quantity != "3" {
		t.Errorf("the server's copy overwrote a quantity the person had typed: %q", got.Typed.Quantity)
	}
}

type heldOffline struct {
	Prevented bool
	Held      []string
	Alerts    int
}

// The button pressed is part of what was said. Pass, attention and fail are
// three buttons on one form, and the queue built its body from the form
// alone -- so a "fail" pressed with no connection was sent later as no status.
func TestAHeldSubmissionKeepsTheButtonPressed(t *testing.T) {
	var got heldOffline
	keepRun(t, "pressed", &got)
	if len(got.Held) != 1 || got.Held[0] != "note=Worn+to+the+indicator&status=fail" {
		t.Errorf("held %q, want the note and status=fail", got.Held)
	}
}

// A browser that does not say which button was pressed cannot have the form
// held: guessing the answer is worse than saying it needs a connection.
func TestAFormAnsweredByItsButtonsIsNotHeldBlind(t *testing.T) {
	var got heldOffline
	keepRun(t, "unknownButton", &got)
	if len(got.Held) != 0 {
		t.Errorf("held %q without knowing which button was pressed", got.Held)
	}
	if !got.Prevented || got.Alerts != 1 {
		t.Errorf("the person was not stopped and told (prevented %v, %d alerts)", got.Prevented, got.Alerts)
	}
}

// A file cannot be put in a urlencoded body; it arrives as "[object File]".
func TestAFileIsNeverHeldAsText(t *testing.T) {
	var got heldOffline
	keepRun(t, "photo", &got)
	if len(got.Held) != 0 {
		t.Errorf("a form carrying a file was held as %q", got.Held)
	}
	if !got.Prevented || got.Alerts != 1 {
		t.Errorf("the person was not stopped and told (prevented %v, %d alerts)", got.Prevented, got.Alerts)
	}
}
