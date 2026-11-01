package workshop_test

// core is the exported operations of this package that are not the
// workshop module's but Core's, each with the reason (#143). They keep
// working whatever the shop runs: a shop that has switched the workshop off
// still reads its invoices, takes its payments and keeps its customers.
//
// Everything else that takes a scope belongs to the workshop and checks that
// the shop runs it -- through inWorkshop, scope.Require or scope.Uses, itself
// or in what it calls -- where the data is read, not in a handler. The
// permission contract (#144, internal/testsupport/contract.go) reads this map
// from the source and fails on anything else.
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
	"SetStaffRoles": "people", "RolesOffered": "people: which roles there are to give",
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
