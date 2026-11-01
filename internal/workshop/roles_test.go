package workshop_test

// everyRole is the exported business operations any signed-in role may call,
// each with the reason. "Any role" means any role the workshop has: the
// workshop's operations read through inWorkshop, which refuses a person
// holding no workshop role -- car sales, rental -- with ErrForbidden (#145). Read from the source by the permission contract
// (#144, internal/testsupport/contract.go), which fails on an operation that
// checks no role and is not here. Everything else that takes a scope checks its role
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
	"MyTime":             "the caller's own hours",
	"ProgressFor":        "hours and a time on a job, no customer data",
	"PlanFor":            "who is on a job and when, which a technician needs for their own work; no customer data",
	"Backlog":            "open jobs nobody is on, by number and registration; no customer data",
	"TasksFor":           "the work list of a job: steps and who did them, no price and no customer data",
	"OutsourcedFor":      "what is away at a supplier and when it is due back, which a technician needs to find the car or the part; money and invoice numbers are left out for any role but the counter and the parts desk",
	"DeliveryDraft":      "what was done on a job -- lines performed, steps, parts, checks; no customer personal data",
	"RecommendationsFor": "what the workshop suggests doing later on a job; no customer personal data",
	"PresenceFor":        "whether the car is here, which a technician needs before looking for it; no customer data",
	"PartsOnJob":         "which parts are priced and taken out on a job: numbers and names, no cost, no customer data",
	"FinalCheckFor":      "whether the work is signed off, which the technician needs before saying it is ready; no customer data",
	"MyLocale":           "the caller's own language", "SetMyLocale": "the caller's own language",
	"SetMyTheme": "the caller's own light or dark",
	"AlertSound": "the caller's own setting", "SetAlertSound": "the caller's own setting",
	"ScheduleFor": "the caller's own schedule and clocked hours", "MarkScheduleSeen": "the caller's own schedule",
	// Each role is given its own kind of item, decided inside by role; the
	// technician's carry the job and the shop's own words, never a customer.
	"WaitingFor": "what is waiting for the caller, per role", "CountWaiting": "how many of WaitingFor",

	// The job as the person holding the spanner sees it. Job carries the
	// vehicle and the work, and no customer field exists on it to leak.
	"JobByID": "vehicle and job, no customer data", "OpenJobs": "vehicle and job, no customer data",
	"TimeFor": "who clocked how long on a job, names of colleagues only",

	// What the technician reports and the front desk acts on.
	"ReportFinding": "a technician's report", "FindingsFor": "the reports on a job",
	"RequestPart": "a technician's request", "PartRequestsFor": "the requests on a job",
	"RequestParts": "a technician's request, with how many",

	// The inspection is done by whoever has the car. The customer's link,
	// prices and decisions are the front desk's and checked there.
	"StartInspection": "inspection work", "SetItem": "inspection work",
	"CompleteInspection": "inspection work", "AttachPhoto": "inspection work",
	"InspectionsFor": "inspection work", "InspectionsForID": "inspection work",
	"Templates":   "the checklists to start one from",
	"PhotoInShop": "whether a photograph belongs to the shop, for serving it",

	// Which roles a shop has to give: a list of role names, the same for
	// everybody in the shop, and no person's data.
	"RolesOffered": "the roles the shop's modules have, no person's data",

	"ImportKindsFor":  "the kinds of record the shop's modules have, no person's data",
	"SubjectsOffered": "the subjects the shop's modules have, no person's data",

	// Protocols other than a job's: delegate to endProtocol, which reads the
	// subject and checks the role for it (subjectAllows).
	"CompleteProtocol": "delegates to endProtocol, which checks the role for the protocol's subject",
	"AbandonProtocol":  "delegates to endProtocol, which checks the role for the protocol's subject",

	// Delegates to TakeInWith, which checks.
	"TakeIn": "delegates to TakeInWith",
}
