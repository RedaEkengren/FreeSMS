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
	// Identity records (#153): who a party is, read across modules.
	// Recording one checks the module of its purpose.
	"IdentityRecordsFor": "that a party's identity was checked", "ReadIdentityValue": "an identity value, read under its purpose",
	"EndIdentityPurpose": "an identity value's purpose ended, which removes it", "SetIdentityRetention": "how long identity values are kept",
	"RotateIdentityKeys": "the keys identity values are sealed with",
	// The outbox (#154): the calls of every module, in front of the owner.
	"OutboxNeedingAttention": "calls to the outside world that need a person", "RetryOutbox": "a call sent again",
	"FinishOutboxByHand": "a call done by hand", "CancelOutbox": "a call cancelled",
	// Vehicle lookup (#155): the log of every module's lookups. Looking up
	// checks the module of its purpose.
	"VehicleLookupLog": "who looked a registration up, in any module",
	// Cash invoices (#156): a manual invoice paid on issue is Core's, as a
	// manual invoice is; the register of payment flows and the figure
	// against the cash register threshold are the business's money.
	"IssueManualAndPay": "a sale entered by hand, paid at the counter", "PaymentFlows": "the ways money is taken in person",
	"AddPaymentFlow": "the ways money is taken in person", "CheckPaymentFlow": "a payment flow checked by the shop's adviser",
	"InPersonTurnoverFor": "money taken in person this financial year", "CounterPaymentFlows": "the ways money is taken in person", "SetCashRegisterThreshold": "the cash register threshold",
	// Audit events (#151): the trail of every module's transitions.
	"AuditTrail":   "who did what and why, whichever module it was in",
	"DocumentByID": "an issued document", "CreditNote": "correcting an issued document",
	"IssueCredit": "correcting an issued document", "PreviewCredit": "correcting an issued document",
	"EligibleLines": "correcting an issued document", "CorrectionsFor": "correcting an issued document",
	"RebillTo": "correcting an issued document", "EInvoice": "an issued document, electronically",
	"IssueManual": "a sale entered by hand, which no module records (#146)",
	// Parties (#148): one record of each person or company across modules.
	"PartyByID": "parties, which every module deals with", "PartyOfCustomer": "parties: a customer's own",
	"EditParty": "parties", "MergeParties": "parties, the same one entered twice",
	// The availability ledger (#149): what the business owns, whichever
	// module books it. Booking checks the module of the use.
	"CreateResource": "resources, the business's own things", "RetireResource": "resources",
	"ResourceBlocks": "when a resource is taken, by any module", "OverrideBlock": "replanning across modules",
	// Contracts (#150): the shop's own wording and how long it keeps them.
	// Producing, reading and signing check the module of the kind.
	"SaveDocumentTemplate": "the shop's contract templates, for every module", "DocumentRetention": "how long contracts are kept",
	"SetDocumentRetention": "how long contracts are kept",
	"ChecklistTemplates":   "checklists, for every module's inspections (#147)", "ChecklistTemplateByID": "checklists",
	"SaveChecklistTemplate": "checklists", "SetChecklistActive": "checklists", "SubjectsOffered": "what a checklist may be made for",
	"ImportKindsFor":      "bringing records in: which kinds, by the modules",
	"SetTemplateSubjects": "what a checklist is for, across modules; each subject named needs its module, checked per subject (#147)",
	"DeliveriesFor":       "how a document reached the customer", "SendDocument": "sending an issued document",
	"HandDocumentOver": "handing an issued document over", "RevokeDocumentLinks": "an issued document's links",
	"UndeliveredDocuments": "documents not yet with the customer",
	"BalanceFor":           "what is owed on a document", "Receivables": "what is owed",
	"InvoiceByPaymentReference": "the invoice a payment is for, by its reference",
	"RecordPayment":             "money received", "ReversePayment": "money received, taken back",
	// The payment journal (#152): money of every module's, recorded.
	// Opening a deposit or a payout checks the module of its purpose.
	"DepositBalanceFor": "money held, readable whatever is switched off", "ApplyDeposit": "money held, applied to what is owed",
	"RefundPayment": "money received, given back", "AddStatementLines": "the bank's statement",
	"MatchPayment": "the bank's statement", "UndoMatch": "the bank's statement", "Unreconciled": "the bank's statement",
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
	// The one search and the home page (#159, #157): Core's, asking each
	// module's provider only while the shop runs it.
	"SearchAll": "the one search across every module", "HomeFor": "the home page across every module",
	"WaitingFor": "the one queue across every module (#158)", "CountWaiting": "the one queue across every module (#158)",
	"Search": "vehicles", "VehicleByID": "vehicles, with their history", "EditVehicle": "vehicles",
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
