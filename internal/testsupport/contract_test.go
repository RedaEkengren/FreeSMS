package testsupport

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// notWalked is every package under internal/ the permission contract does
// not read, each with the reason. Short on purpose: a package not here is
// walked, so a new module is covered the day it is created.
var notWalked = map[string]string{
	"access":      "the scope itself",
	"database":    "the plumbing every operation goes through: InScope and Reading take a scope to set it on the transaction, not to decide with it",
	"testsupport": "test helpers, imported only by tests",
	"money":       "arithmetic on amounts; no scope, no rows",
	"i18n":        "words and their translations",
	"barcode":     "an encoder",
	"sie":         "a file format",
	"ubl":         "a file format",
	"config":      "the environment, read once at start",
	"web":         "templates and static files",
	"site":        "the public website, which has no shop data",
}

// Every exported function under internal/ that takes an access.Scope checks
// its role and its module, or its package says why not (#144). The rules,
// and the helper names they recognise, are written at the top of
// contract.go.
func TestEveryOperationSaysWhoMayCallItAndInWhichModule(t *testing.T) {
	report, err := CheckContracts("..", notWalked)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(report.Problems)
	for _, p := range report.Problems {
		t.Error(p)
	}
	// The parser has stopped seeing them if these fall.
	if report.Checked["workshop"] < 250 || report.Checked["auth"] < 3 {
		t.Errorf("checked %v; the walk has stopped finding the operations", report.Checked)
	}
	for dir := range notWalked {
		if _, err := os.Stat(filepath.Join("..", dir)); err != nil {
			t.Errorf("notWalked names %s, which is not a package any more", dir)
		}
	}
}

// The contract catches what it is for, on packages written to break it.
func TestTheContractCatchesWhatItIsFor(t *testing.T) {
	report, err := CheckContracts(filepath.Join("testdata", "contract"), nil)
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(report.Problems, "\n")
	for _, want := range []string{
		"nomaps: takes scopes and declares no everyRole map",
		"nomaps: takes scopes and declares no core map",
		"Leak (unchecked/unchecked.go) takes a scope and checks no role",
		"Leak (unchecked/unchecked.go) takes a scope and checks no module",
		"Both (unchecked/unchecked.go) checks a role and is also listed in everyRole",
		"Both (unchecked/unchecked.go) checks a module and is also listed in core",
		"unchecked: core lists Gone, which is not an operation of the package any more",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("not reported: %s\ngot:\n%s", want, all)
		}
	}
	for _, p := range report.Problems {
		if strings.HasPrefix(p, "good") || strings.Contains(p, "(good/") {
			t.Errorf("a package that keeps the contract was reported: %s", p)
		}
	}
	if len(report.Problems) != 7 {
		t.Errorf("%d problems, want 7:\n%s", len(report.Problems), all)
	}
	if report.Checked["good"] != 4 {
		t.Errorf("good: %d operations checked, want 4 -- the generated one skipped", report.Checked["good"])
	}
}

// A call cycle used to make the answer depend on map order: walking A, then
// B, then back to A cached B as unchecked although B reaches the check
// through A. Asked many times over, with fresh checkers, every answer is the
// same.
func TestTheModuleCheckThroughACycleDoesNotDependOnOrder(t *testing.T) {
	bodies := map[string]string{
		"A":      "B(scope) helper(scope)",
		"B":      "A(scope)",
		"helper": "scope.Require(access.Workshop)",
		"Lone":   "nothing()",
	}
	for i := 0; i < 200; i++ {
		checks := moduleChecker(bodies)
		for _, name := range []string{"A", "B", "helper"} {
			if !checks(name) {
				t.Fatalf("run %d: %s reaches the check and was not seen to", i, name)
			}
		}
		if checks("Lone") {
			t.Fatalf("run %d: Lone checks nothing and was said to", i)
		}
	}
}
