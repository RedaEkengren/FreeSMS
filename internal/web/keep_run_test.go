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
