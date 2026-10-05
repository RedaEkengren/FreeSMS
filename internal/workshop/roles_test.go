package workshop_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// everyRole is the exported business operations any signed-in role may call,
// each with the reason. Everything else that takes a scope checks its role
// itself, where the data is read or written -- not in the handler that
// happens to call it today.
//
// Tenant isolation is separate and stays underneath: row level security
// keeps every one of these inside the caller's shop whatever its role.
var everyRole = map[string]string{
	// One's own.
	"SaveDraft": "the caller's own draft", "LoadDraft": "the caller's own draft",
	"DiscardDraft":        "the caller's own draft",
	"ClaimIdempotency":    "the caller's own request key",
	"CompleteIdempotency": "the caller's own request key",
	"ReleaseIdempotency":  "the caller's own request key",
	"ClockIn":             "the caller's own clock", "ClockOut": "the caller's own clock",
	"MyTime":         "the caller's own hours",
	"ProgressFor":    "hours and a time on a job, no customer data",
	"ChangePassword": "the caller's own password, checked against the current one",

	// The job as the person holding the spanner sees it. Job carries the
	// vehicle and the work, and no customer field exists on it to leak.
	"JobByID": "vehicle and job, no customer data", "OpenJobs": "vehicle and job, no customer data",
	"TimeFor": "who clocked how long on a job, names of colleagues only",

	// What the technician reports and the front desk acts on.
	"ReportFinding": "a technician's report", "FindingsFor": "the reports on a job",
	"RequestPart": "a technician's request", "PartRequestsFor": "the requests on a job",

	// The inspection is done by whoever has the car. The customer's link,
	// prices and decisions are the front desk's and checked there.
	"StartInspection": "inspection work", "SetItem": "inspection work",
	"CompleteInspection": "inspection work", "AttachPhoto": "inspection work",
	"InspectionsFor": "inspection work", "InspectionsForID": "inspection work",
	"Templates":   "the checklists to start one from",
	"PhotoInShop": "whether a photograph belongs to the shop, for serving it",

	// Delegates to TakeInWith, which checks.
	"TakeIn": "delegates to TakeInWith",
}

// Every exported operation that takes a scope either checks the role itself
// or is in everyRole with a reason.
//
// Found otherwise: AddLine, InvoicesFor, the time library, the customer's
// links and auth.Deactivate relied on the one handler that called them. None
// was reachable over HTTP by the wrong role; the next caller would not have
// known it had to check.
func TestEveryOperationSaysWhoMayCallIt(t *testing.T) {
	checked := 0
	for _, dir := range []string{".", "../auth"} {
		fset := token.NewFileSet()
		pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
			return !strings.HasSuffix(fi.Name(), "_test.go")
		}, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", dir, err)
		}
		for _, pkg := range pkgs {
			for name, file := range pkg.Files {
				src, _ := os.ReadFile(name)
				for _, decl := range file.Decls {
					fn, ok := decl.(*ast.FuncDecl)
					if !ok || fn.Recv != nil || !fn.Name.IsExported() || !takesScope(fn) {
						continue
					}
					checked++
					body := string(src[fset.Position(fn.Body.Pos()).Offset:fset.Position(fn.Body.End()).Offset])
					checks := strings.Contains(body, "scope.Role.") || strings.Contains(body, "(scope.Role,")
					_, open := everyRole[fn.Name.Name]
					switch {
					case !checks && !open:
						t.Errorf("%s (%s) takes a scope and checks no role; check it, or add it to everyRole with the reason",
							fn.Name.Name, filepath.Base(name))
					case checks && open:
						t.Errorf("%s checks a role and is also listed as open to every role; one of the two is wrong",
							fn.Name.Name)
					}
				}
			}
		}
	}
	if checked < 60 {
		t.Errorf("only %d operations found; the parser has stopped seeing them", checked)
	}
}

func takesScope(fn *ast.FuncDecl) bool {
	for _, p := range fn.Type.Params.List {
		if se, ok := p.Type.(*ast.SelectorExpr); ok && se.Sel.Name == "Scope" {
			if x, ok := se.X.(*ast.Ident); ok && x.Name == "access" {
				return true
			}
		}
	}
	return false
}
