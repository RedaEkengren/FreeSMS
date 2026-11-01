package workshop_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// core is the exported operations of this package that are not the
// workshop module's but Core's, each with the reason (#143). They keep
// working whatever the shop runs: a shop that has switched the workshop off
// still reads its invoices, takes its payments and keeps its customers.
//
// Everything else that takes a scope belongs to the workshop and checks that
// the shop runs it -- through inWorkshop, scope.Require or scope.Uses, itself
// or in what it calls -- where the data is read, not in a handler.
var core = map[string]string{
	// Documents and money: issued whatever made them, corrected, paid and
	// handed to the accountant as long as the books are kept.
	"DocumentByID": "an issued document", "CreditNote": "correcting an issued document",
	"IssueCredit": "correcting an issued document", "PreviewCredit": "correcting an issued document",
	"EligibleLines": "correcting an issued document", "CorrectionsFor": "correcting an issued document",
	"RebillTo": "correcting an issued document", "EInvoice": "an issued document, electronically",
	"DeliveriesFor": "how a document reached the customer", "SendDocument": "sending an issued document",
	"HandDocumentOver": "handing an issued document over", "RevokeDocumentLinks": "an issued document's links",
	"UndeliveredDocuments": "documents not yet with the customer",
	"BalanceFor":           "what is owed on a document", "Receivables": "what is owed",
	"RecordPayment": "money received", "ReversePayment": "money received, taken back",
	"FollowUpFor": "chasing what is owed", "Worklist": "chasing what is owed", "TakeFollowUp": "chasing what is owed",
	"LogContact": "chasing what is owed", "RecordPromise": "chasing what is owed", "Dispute": "chasing what is owed",
	"ResolveDispute": "chasing what is owed", "RecordRefund": "chasing what is owed",
	"PrepareReminder": "chasing what is owed", "QueueReminder": "chasing what is owed",
	"DeskPeople": "who chases what is owed",
	"ContactFor": "the customer on a job's document, read when chasing it: history, kept readable",
	"DayFor":     "the day's money", "RecordSettlement": "the day's money", "ReviewDay": "the day's money",
	"AccountingExports": "the books", "ExportAccounting": "the books",
	"Summary":           "the figures: invoiced is every module's; the hours read are history",
	"MessagesThisMonth": "what sending cost, whatever it was about", "ShopSignature": "how the shop signs a message",

	// Customers and vehicles: the same car across every module.
	"CreateCustomer": "customers", "CreateCustomerChecked": "customers", "FindCustomers": "customers",
	"BrowseCustomers": "customers", "CustomerByID": "customers", "EditCustomer": "customers",
	"AddCustomerPerson": "customers", "RemoveCustomerPerson": "customers",
	"CustomerDuplicates": "customers", "LikelySame": "customers", "PreviewCustomerMerge": "customers",
	"MergeCustomers": "customers",
	"Search":         "vehicles", "VehicleByID": "vehicles, with their history", "EditVehicle": "vehicles",
	"VehicleRevision": "vehicles", "VehicleChanges": "vehicles", "ChangeRegistration": "vehicles",
	"CorrectRegistration": "vehicles", "SellVehicle": "vehicles", "PreviewVehicleMerge": "vehicles",
	"MergeVehicles": "vehicles",

	// One's own.
	"SaveDraft": "one's own", "LoadDraft": "one's own", "DiscardDraft": "one's own",
	"ClaimIdempotency": "one's own", "CompleteIdempotency": "one's own", "ReleaseIdempotency": "one's own",
	"MyLocale": "one's own", "SetMyLocale": "one's own", "SetMyTheme": "one's own",
	"AlertSound": "one's own", "SetAlertSound": "one's own",

	// People, and when they work.
	"Staff": "people", "AddStaff": "people", "SetStaffActive": "people", "SetStaffPassword": "people",
	"ScheduleFor": "when people work", "StaffWeek": "when people work", "SetRota": "when people work",
	"ChangeShift": "when people work", "SetPlanner": "when people work", "MarkScheduleSeen": "when people work",
	"SetAbsence": "when people work", "ClearAbsence": "when people work",

	// Privacy: every module's personal data, whatever is switched on.
	"ExportPerson": "privacy", "ErasePerson": "privacy", "Erasures": "privacy", "DeleteAttachment": "privacy",
	"StartShopExport": "privacy", "FinishShopExport": "privacy", "WriteShopExport": "privacy",
	"ShopExports": "privacy", "ShopExportByID": "privacy",

	// The shop itself.
	"ShopDetailsFor": "the shop", "SaveDetails": "the shop",
	"ModulesFor": "what the shop runs", "ModuleChanges": "what the shop runs", "SwitchModule": "what the shop runs",
	"ReadinessFor": "getting started", "ReadinessStatus": "getting started", "InvoiceExample": "getting started",
	"PreviewImport": "bringing records in", "ApplyImport": "bringing records in", "DiscardImport": "bringing records in",
	"ImportBatches": "bringing records in", "ImportBatchByID": "bringing records in",
}

// The ways an operation says it is the workshop's.
var moduleCheck = regexp.MustCompile(`inWorkshop\(|scope\.Require\(access\.|scope\.Uses\(access\.`)

// Every exported operation that takes a scope is Core's, with a reason, or
// checks that the shop runs the workshop -- itself or in a function of this
// package it calls (#143).
func TestEveryWorkshopOperationChecksTheModule(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	bodies := map[string]string{}
	type op struct{ name, file string }
	var ops []op
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			src, _ := os.ReadFile(name)
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv != nil || fn.Body == nil {
					continue
				}
				bodies[fn.Name.Name] = string(src[fset.Position(fn.Body.Pos()).Offset:fset.Position(fn.Body.End()).Offset])
				if fn.Name.IsExported() && takesScope(fn) {
					ops = append(ops, op{fn.Name.Name, filepath.Base(name)})
				}
			}
		}
	}
	checks := moduleChecker(bodies)
	for _, o := range ops {
		_, isCore := core[o.name]
		switch c := checks(o.name); {
		case !c && !isCore:
			t.Errorf("%s (%s) takes a scope and checks no module; route it through inWorkshop, "+
				"or add it to core with the reason it is not the workshop's", o.name, o.file)
		case c && isCore:
			t.Errorf("%s (%s) is listed as Core and checks the workshop; a Core operation must keep "+
				"working when the workshop is off", o.name, o.file)
		}
	}
	for name := range core {
		if _, ok := bodies[name]; !ok {
			t.Errorf("core lists %s, which is not an operation of this package any more", name)
		}
	}
	if len(ops) < 250 {
		t.Errorf("only %d operations found; the parser has stopped seeing them", len(ops))
	}
}

// moduleChecker answers whether a function checks the module, directly or
// through a function of the package it calls.
func moduleChecker(bodies map[string]string) func(string) bool {
	memo := map[string]bool{}
	visiting := map[string]bool{}
	var checks func(string) bool
	checks = func(name string) bool {
		if v, ok := memo[name]; ok {
			return v
		}
		if visiting[name] {
			return false
		}
		visiting[name] = true
		body := bodies[name]
		found := moduleCheck.MatchString(body)
		for callee := range bodies {
			if found {
				break
			}
			if callee != name && strings.Contains(body, callee+"(") &&
				regexp.MustCompile(`\b`+callee+`\(`).MatchString(body) && checks(callee) {
				found = true
			}
		}
		visiting[name] = false
		memo[name] = found
		return found
	}
	return checks
}
