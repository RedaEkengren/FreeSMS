package workshop

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// Starting from what the shop already has.
//
// An import is a reviewed batch and not a loop of saves. A file is read and
// understood without writing anything; the preview says, row by row, what would
// be created, filled in, left alone or refused, and which rows may be somebody
// already known. Only then does applying write, in one transaction: either
// every row the person accepted is in, or none is, and a batch that failed
// halfway is still a previewed batch that can be applied again.
//
// A file is identified by what is in it. The same file uploaded twice is the
// same batch, and what a source system called a record is kept, so a corrected
// file naming the same references finds the records it made before instead of
// making them again. Opening stock is a dated entry in the ledger that says
// which batch it came from, and the same part on the same day is never added
// twice.
//
// Existing records are only filled in where they are empty. A value that
// differs is said, in the preview, and left as it was: replacing what a shop
// has typed with what an old system says is not a thing a batch does quietly.
//
// Repair history is not imported. It would have to be labelled as someone
// else's and could not carry invoice numbers or financial documents, and what
// a shop needs on its first day is its customers, its cars and its shelf.

// importPreviewRetention is how long an unapplied preview keeps what the file said.
const importPreviewRetention = 7 * 24 * time.Hour

// ImportCandidate is a customer a row may be, or an earlier row of the file.
type ImportCandidate struct {
	ID   string
	Name string
	Why  []string
}

// ImportRow is a row of a batch.
type ImportRow struct {
	No         int
	Ref        string
	Values     map[string]string
	Action     string
	Errors     []string
	Warnings   []string
	Fills      []string
	Candidates []ImportCandidate
	Decision   string
	Target     string
	Outcome    string
	Message    string
	EntityID   string
}

// NeedsDecision is whether the row may be somebody already known and nobody
// has said what to do about it.
func (r ImportRow) NeedsDecision() bool {
	return r.Action == "create" && len(r.Candidates) > 0 && r.Decision == "" && r.Outcome == ""
}

// Label is what the row would do, or did, as a catalogue key.
func (r ImportRow) Label() string {
	if r.Outcome != "" {
		return map[string]string{"created": "created", "updated": "filled in", "unchanged": "left as it was", "skipped": "skipped", "linked": "linked to an existing record", "failed": "refused"}[r.Outcome]
	}
	return map[string]string{"create": "would be created", "update": "would be filled in", "unchanged": "nothing to add", "error": "cannot be imported"}[r.Action]
}

// ImportBatch is a batch of rows from one file.
type ImportBatch struct {
	ID        string
	Kind      string
	FileName  string
	Encoding  string
	Status    string
	RowCount  int
	Created   int
	Updated   int
	Skipped   int
	Failed    int
	CreatedAt time.Time
	AppliedAt *time.Time
	By        string
	Rows      []ImportRow
	// Set when the file had already been uploaded and this is that batch.
	Existing bool
	// Columns of the file nobody knew what to do with.
	Unknown []string
}

// Pending is the rows still to be applied.
func (b ImportBatch) Pending() bool { return b.Status == "previewed" }

// Counts of what a preview found.
func (b ImportBatch) WouldCreate() int { return b.count("create") }
func (b ImportBatch) WouldUpdate() int { return b.count("update") }
func (b ImportBatch) WouldSkip() int   { return b.count("unchanged") }
func (b ImportBatch) WouldRefuse() int { return b.count("error") }
func (b ImportBatch) Undecided() int {
	n := 0
	for _, r := range b.Rows {
		if r.NeedsDecision() {
			n++
		}
	}
	return n
}
func (b ImportBatch) count(action string) int {
	n := 0
	for _, r := range b.Rows {
		if r.Action == action {
			n++
		}
	}
	return n
}

// ImportDecision is what to do about a row that may be somebody known.
type ImportDecision struct {
	Choice string // create, link, skip
	Target string // for link: a customer's id, or "row:N" for an earlier row of the file
}

type rowInfo struct {
	Errors     []string          `json:"errors,omitempty"`
	Warnings   []string          `json:"warnings,omitempty"`
	Fills      []string          `json:"fills,omitempty"`
	Candidates []ImportCandidate `json:"candidates,omitempty"`
	// What an update would write, by column.
	Entity string `json:"entity,omitempty"`
}

// PreviewImport reads a file and says what importing it would do, without
// writing any of it. The same file again returns the batch it already is.
func PreviewImport(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, kind, fileName string, raw []byte) (ImportBatch, error) {
	if !scope.Role.RunsTheShop() {
		return ImportBatch{}, access.ErrForbidden
	}
	parsed, err := ParseImport(kind, raw)
	if err != nil {
		return ImportBatch{}, err
	}
	fileName = strings.TrimSpace(fileName)
	if i := strings.LastIndexAny(fileName, `/\`); i >= 0 {
		fileName = fileName[i+1:]
	}
	if len(fileName) > 200 {
		fileName = fileName[:200]
	}
	sum := sha256.Sum256(raw)

	var id string
	var existing bool
	err = database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT id::text FROM import_batches WHERE kind = $1 AND file_sha256 = $2 AND status <> 'discarded'`,
			kind, sum[:]).Scan(&id)
		if err == nil {
			existing = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("look for the file: %w", err)
		}
		rows, err := classifyImportTx(ctx, tx, kind, parsed.Rows)
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO import_batches (shop_id, kind, file_name, file_sha256, encoding, row_count, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id::text`,
			scope.ShopID, kind, fileName, sum[:], parsed.Encoding, len(rows), scope.UserID).Scan(&id); err != nil {
			return fmt.Errorf("record the batch: %w", err)
		}
		for _, r := range rows {
			data, _ := json.Marshal(r.Values)
			info, _ := json.Marshal(rowInfo{Errors: r.Errors, Warnings: r.Warnings, Fills: r.Fills, Candidates: r.Candidates})
			if _, err := tx.Exec(ctx, `
				INSERT INTO import_rows (shop_id, batch_id, row_no, ref, data, info, action)
				VALUES ($1, $2, $3, nullif($4, ''), $5, $6, $7)`,
				scope.ShopID, id, r.No, r.Ref, data, info, r.Action); err != nil {
				return fmt.Errorf("record row %d: %w", r.No, err)
			}
		}
		return nil
	})
	if err != nil {
		return ImportBatch{}, err
	}
	b, err := ImportBatchByID(ctx, pool, scope, id)
	b.Existing, b.Unknown = existing, parsed.Unknown
	return b, err
}

// classifyImportTx decides, for each parsed row, what applying it would do.
func classifyImportTx(ctx context.Context, tx pgx.Tx, kind string, parsed []ParsedRow) ([]ImportRow, error) {
	rows := make([]ImportRow, len(parsed))
	for i, p := range parsed {
		rows[i] = ImportRow{No: p.No, Ref: p.Ref, Values: p.Values, Errors: p.Errors, Warnings: p.Warnings}
		if len(p.Errors) > 0 {
			rows[i].Action = "error"
		}
	}
	var err error
	switch kind {
	case "customers":
		err = classifyCustomers(ctx, tx, rows)
	case "vehicles":
		err = classifyVehicles(ctx, tx, rows)
	case "parts":
		err = classifyParts(ctx, tx, rows)
	case "stock":
		err = classifyStock(ctx, tx, rows)
	}
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].Action == "" {
			rows[i].Action = "create"
		}
		if len(rows[i].Errors) > 0 {
			rows[i].Action = "error"
		}
	}
	return rows, nil
}

func refEntityTx(ctx context.Context, tx pgx.Tx, kind, ref string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT entity_id::text FROM import_refs WHERE kind = $1 AND external_id = $2`, kind, ref).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("look up reference: %w", err)
	}
	return id, nil
}

func classifyCustomers(ctx context.Context, tx pgx.Tx, rows []ImportRow) error {
	type seen struct {
		no                 int
		org, email, digits string
	}
	var earlier []seen
	refAt := map[string]int{}
	for i := range rows {
		r := &rows[i]
		if r.Action == "error" {
			continue
		}
		if at, dup := refAt[r.Ref]; dup {
			r.Errors = append(r.Errors, fmt.Sprintf("ref %s is also on row %d", r.Ref, at))
			continue
		}
		refAt[r.Ref] = r.No
		v := r.Values
		id, err := refEntityTx(ctx, tx, "customers", r.Ref)
		if err != nil {
			return err
		}
		if id != "" {
			// Known from an earlier batch: fill in what is missing.
			var name, org, vat, phone, email, a1, a2, zip, city string
			if err := tx.QueryRow(ctx, `
				SELECT coalesce(CASE WHEN c.kind = 'company' THEN c.company_name END, p.display_name, ''), coalesce(c.org_number, ''), coalesce(c.vat_number, ''),
				       coalesce(p.phone, ''), coalesce(p.email, ''), coalesce(c.address_line1, ''), coalesce(c.address_line2, ''),
				       coalesce(c.postal_code, ''), coalesce(c.city, '')
				FROM customers c LEFT JOIN people p ON p.id = c.person_id WHERE c.id = $1`, id).Scan(&name, &org, &vat, &phone, &email, &a1, &a2, &zip, &city); err != nil {
				return fmt.Errorf("read customer: %w", err)
			}
			r.Fills, r.Warnings = fillsAndDifferences(r.Warnings, [][3]string{
				{"org_number", org, v["org_number"]}, {"vat_number", vat, v["vat_number"]}, {"phone", phone, v["phone"]},
				{"email", email, v["email"]}, {"address", a1, v["address"]}, {"address2", a2, v["address2"]},
				{"postal_code", zip, v["postal_code"]}, {"city", city, v["city"]}, {"name", name, v["name"]}})
			r.Action = "unchanged"
			if len(r.Fills) > 0 {
				r.Action = "update"
			}
			continue
		}
		cands, err := duplicatesTx(ctx, tx, v["org_number"], v["email"], v["phone"], "")
		if err != nil {
			return err
		}
		for _, c := range cands {
			r.Candidates = append(r.Candidates, ImportCandidate{ID: c.ID, Name: c.Name, Why: c.Why})
		}
		org, digits := normaliseOrg(v["org_number"]), digitsOf(v["phone"])
		for _, e := range earlier {
			var why []string
			if org != "" && org == e.org {
				why = append(why, whyOrg)
			}
			if v["email"] != "" && v["email"] == e.email {
				why = append(why, whyEmail)
			}
			if len(digits) >= 6 && digits == e.digits {
				why = append(why, whyPhone)
			}
			if len(why) > 0 {
				r.Candidates = append(r.Candidates, ImportCandidate{ID: "row:" + strconv.Itoa(e.no), Name: fmt.Sprintf("row %d", e.no), Why: why})
			}
		}
		earlier = append(earlier, seen{r.No, org, v["email"], digits})
		r.Action = "create"
	}
	return nil
}

// fillsAndDifferences compares what is stored with what the file says: a
// stored value that is empty and a file value that is not is filled in; two
// that differ are warned about and left alone.
func fillsAndDifferences(warnings []string, cols [][3]string) (fills, warns []string) {
	warns = warnings
	for _, c := range cols {
		name, have, file := c[0], c[1], c[2]
		switch {
		case file == "":
		case have == "":
			fills = append(fills, name)
		case have != file && !strings.EqualFold(have, file):
			warns = append(warns, fmt.Sprintf("%s is %q here and %q in the file; left as it is", name, have, file))
		}
	}
	return
}

func classifyVehicles(ctx context.Context, tx pgx.Tx, rows []ImportRow) error {
	regAt := map[string]int{}
	vinAt := map[string]int{}
	for i := range rows {
		r := &rows[i]
		if r.Action == "error" {
			continue
		}
		v := r.Values
		if at, dup := regAt[r.Ref]; dup {
			r.Errors = append(r.Errors, fmt.Sprintf("the registration is also on row %d", at))
			continue
		}
		regAt[r.Ref] = r.No
		if vin := v["vin"]; vin != "" {
			if at, dup := vinAt[vin]; dup {
				r.Errors = append(r.Errors, fmt.Sprintf("the chassis number is also on row %d", at))
				continue
			}
			vinAt[vin] = r.No
		}
		var vehicleID string
		err := tx.QueryRow(ctx, `SELECT vehicle_id::text FROM vehicle_registrations WHERE normalised = $1 AND valid_to IS NULL`, r.Ref).Scan(&vehicleID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("look up registration: %w", err)
		}
		if vin := v["vin"]; vin != "" {
			var byVin string
			err := tx.QueryRow(ctx, `SELECT id::text FROM vehicles WHERE vin = $1`, vin).Scan(&byVin)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("look up chassis number: %w", err)
			}
			if byVin != "" && vehicleID != "" && byVin != vehicleID {
				r.Errors = append(r.Errors, "the chassis number belongs to a different vehicle than the registration does")
				continue
			}
			if byVin != "" && vehicleID == "" {
				r.Errors = append(r.Errors, "the chassis number is already on a vehicle with another registration; correct that one instead")
				continue
			}
		}
		owner, ownerKnown := "", true
		if ref := v["owner_ref"]; ref != "" {
			if owner, err = refEntityTx(ctx, tx, "customers", ref); err != nil {
				return err
			}
			ownerKnown = owner != ""
			if !ownerKnown {
				r.Warnings = append(r.Warnings, fmt.Sprintf("the owner %s is not known; the vehicle is created without an owner", ref))
			}
		}
		if vehicleID == "" {
			r.Action = "create"
			continue
		}
		var make_, model, engine, vin string
		var year *int16
		var currentOwner string
		if err := tx.QueryRow(ctx, `
			SELECT coalesce(v.make, ''), coalesce(v.model, ''), coalesce(v.engine, ''), coalesce(v.vin, ''), v.model_year,
			       coalesce((SELECT o.customer_id::text FROM vehicle_ownership o WHERE o.vehicle_id = v.id AND o.owned_to IS NULL), '')
			FROM vehicles v WHERE v.id = $1`, vehicleID).Scan(&make_, &model, &engine, &vin, &year, &currentOwner); err != nil {
			return fmt.Errorf("read vehicle: %w", err)
		}
		yearHave := ""
		if year != nil {
			yearHave = strconv.Itoa(int(*year))
		}
		r.Fills, r.Warnings = fillsAndDifferences(r.Warnings, [][3]string{
			{"make", make_, v["make"]}, {"model", model, v["model"]}, {"engine", engine, v["engine"]},
			{"vin", vin, v["vin"]}, {"year", yearHave, v["year"]}})
		if owner != "" {
			switch {
			case currentOwner == "":
				r.Fills = append(r.Fills, "owner")
			case currentOwner != owner:
				r.Warnings = append(r.Warnings, "the vehicle has another owner here; left as it is")
			}
		}
		if km := v["odometer_km"]; km != "" {
			var have bool
			if err := tx.QueryRow(ctx, `SELECT exists(SELECT 1 FROM odometer_readings WHERE vehicle_id = $1 AND km = $2::bigint)`, vehicleID, km).Scan(&have); err != nil {
				return fmt.Errorf("read odometer: %w", err)
			}
			if !have {
				r.Fills = append(r.Fills, "odometer")
			}
		}
		r.Action = "unchanged"
		if len(r.Fills) > 0 {
			r.Action = "update"
		}
	}
	return nil
}

func classifyParts(ctx context.Context, tx pgx.Tx, rows []ImportRow) error {
	numAt := map[string]int{}
	for i := range rows {
		r := &rows[i]
		if r.Action == "error" {
			continue
		}
		v := r.Values
		if at, dup := numAt[r.Ref]; dup {
			r.Errors = append(r.Errors, fmt.Sprintf("the part number is also on row %d", at))
			continue
		}
		numAt[r.Ref] = r.No
		var id, name, unit, location string
		var cost, price *int64
		var minimum float64
		err := tx.QueryRow(ctx, `
			SELECT id::text, name, unit, coalesce(location, ''), cost_minor, price_minor, coalesce(minimum_quantity, 0)::float8
			FROM parts WHERE number = $1`, r.Ref).Scan(&id, &name, &unit, &location, &cost, &price, &minimum)
		if errors.Is(err, pgx.ErrNoRows) {
			var owner string
			err := tx.QueryRow(ctx, `SELECT p.number FROM part_codes c JOIN parts p ON p.id = c.part_id WHERE c.code = $1`, r.Ref).Scan(&owner)
			switch {
			case err == nil:
				r.Errors = append(r.Errors, fmt.Sprintf("%s is already a code of part %s", r.Ref, owner))
			case !errors.Is(err, pgx.ErrNoRows):
				return fmt.Errorf("check codes: %w", err)
			default:
				r.Action = "create"
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("look up part: %w", err)
		}
		if v["unit"] != unit {
			r.Errors = append(r.Errors, fmt.Sprintf("the unit is %s here and %s in the file; a part is not recounted in another unit", unit, v["unit"]))
			continue
		}
		have := func(p *int64) string {
			if p == nil {
				return ""
			}
			return strconv.FormatInt(*p, 10)
		}
		minHave := ""
		if minimum > 0 {
			minHave = strconv.FormatInt(int64(minimum*1000+0.5), 10)
		}
		r.Fills, r.Warnings = fillsAndDifferences(r.Warnings, [][3]string{
			{"cost", have(cost), v["cost"]}, {"price", have(price), v["price"]}, {"minimum", minHave, v["minimum"]},
			{"location", location, v["location"]}, {"name", name, v["name"]}})
		r.Action = "unchanged"
		if len(r.Fills) > 0 {
			r.Action = "update"
		}
	}
	return nil
}

func classifyStock(ctx context.Context, tx pgx.Tx, rows []ImportRow) error {
	type key struct{ part, day string }
	seen := map[key]int{}
	for i := range rows {
		r := &rows[i]
		if r.Action == "error" {
			continue
		}
		v := r.Values
		k := key{r.Ref, v["as_of"]}
		if at, dup := seen[k]; dup {
			r.Errors = append(r.Errors, fmt.Sprintf("the same part on the same day is also on row %d", at))
			continue
		}
		seen[k] = r.No
		var partID string
		err := tx.QueryRow(ctx, `
			SELECT id::text FROM parts WHERE number = $1
			UNION ALL SELECT part_id::text FROM part_codes WHERE code = $1 LIMIT 1`, r.Ref).Scan(&partID)
		if errors.Is(err, pgx.ErrNoRows) {
			r.Errors = append(r.Errors, fmt.Sprintf("there is no part %s; import the parts first", r.Ref))
			continue
		}
		if err != nil {
			return fmt.Errorf("look up part: %w", err)
		}
		var recorded *string
		var already int64
		if err := tx.QueryRow(ctx, `
			SELECT (sum(quantity * 1000))::bigint::text, count(*) FROM stock_movements
			WHERE part_id = $1 AND import_batch_id IS NOT NULL AND (moved_at AT TIME ZONE (SELECT timezone FROM shops LIMIT 1))::date = $2::date`,
			partID, v["as_of"]).Scan(&recorded, &already); err != nil {
			return fmt.Errorf("read earlier opening stock: %w", err)
		}
		if already > 0 {
			if recorded != nil && *recorded == v["quantity"] {
				r.Action = "unchanged"
				r.Warnings = append(r.Warnings, "this opening balance is already recorded")
			} else {
				r.Errors = append(r.Errors, "opening stock for this part and day is already recorded with another quantity; correct it with a stocktake")
			}
			continue
		}
		var others int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM stock_movements WHERE part_id = $1`, partID).Scan(&others); err != nil {
			return fmt.Errorf("read stock history: %w", err)
		}
		if others > 0 {
			r.Warnings = append(r.Warnings, "this part already has stock history here; the opening balance is added to it")
		}
		r.Action = "create"
	}
	return nil
}

// ImportBatchByID reads a batch with its rows.
func ImportBatchByID(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string) (ImportBatch, error) {
	if !scope.Role.RunsTheShop() {
		return ImportBatch{}, access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return ImportBatch{}, ErrNotFound
	}
	var b ImportBatch
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		b, err = importBatchTx(ctx, tx, id, true)
		return err
	})
	return b, err
}

func importBatchTx(ctx context.Context, tx pgx.Tx, id string, withRows bool) (ImportBatch, error) {
	var b ImportBatch
	err := tx.QueryRow(ctx, `
		SELECT b.id::text, b.kind, b.file_name, b.encoding, b.status, b.row_count, b.created_count, b.updated_count, b.skipped_count,
		       b.failed_count, b.created_at, b.applied_at, coalesce(p.display_name, '')
		FROM import_batches b LEFT JOIN users u ON u.id = coalesce(b.applied_by, b.created_by) LEFT JOIN people p ON p.id = u.person_id
		WHERE b.id = $1`, id).Scan(&b.ID, &b.Kind, &b.FileName, &b.Encoding, &b.Status, &b.RowCount, &b.Created, &b.Updated,
		&b.Skipped, &b.Failed, &b.CreatedAt, &b.AppliedAt, &b.By)
	if errors.Is(err, pgx.ErrNoRows) {
		return b, ErrNotFound
	}
	if err != nil {
		return b, fmt.Errorf("read batch: %w", err)
	}
	if !withRows {
		return b, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT row_no, coalesce(ref, ''), data, info, action, coalesce(decision, ''), coalesce(decision_target, ''),
		       coalesce(outcome, ''), coalesce(message, ''), coalesce(entity_id::text, '')
		FROM import_rows WHERE batch_id = $1 ORDER BY row_no`, id)
	if err != nil {
		return b, fmt.Errorf("read rows: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r ImportRow
		var data, info []byte
		if err := rows.Scan(&r.No, &r.Ref, &data, &info, &r.Action, &r.Decision, &r.Target, &r.Outcome, &r.Message, &r.EntityID); err != nil {
			return b, fmt.Errorf("scan row: %w", err)
		}
		if data != nil {
			_ = json.Unmarshal(data, &r.Values)
		}
		if info != nil {
			var ri rowInfo
			_ = json.Unmarshal(info, &ri)
			r.Errors, r.Warnings, r.Fills, r.Candidates = ri.Errors, ri.Warnings, ri.Fills, ri.Candidates
		}
		b.Rows = append(b.Rows, r)
	}
	return b, rows.Err()
}

// ImportBatches lists the latest batches, without their rows.
func ImportBatches(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]ImportBatch, error) {
	if !scope.Role.RunsTheShop() {
		return nil, access.ErrForbidden
	}
	var out []ImportBatch
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id::text FROM import_batches ORDER BY created_at DESC LIMIT 30`)
		if err != nil {
			return fmt.Errorf("list batches: %w", err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, id := range ids {
			b, err := importBatchTx(ctx, tx, id, false)
			if err != nil {
				return err
			}
			out = append(out, b)
		}
		return nil
	})
	return out, err
}

// DiscardImport throws a previewed batch away, and what the file said with it.
func DiscardImport(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return ErrNotFound
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM import_batches WHERE id = $1 FOR UPDATE`, id).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("lock batch: %w", err)
		}
		if status != "previewed" {
			return fmt.Errorf("%w: only a batch that has not been applied can be discarded", ErrInvalid)
		}
		return wipeBatchTx(ctx, tx, id, "discarded")
	})
}

func wipeBatchTx(ctx context.Context, tx pgx.Tx, id, status string) error {
	if _, err := tx.Exec(ctx, `UPDATE import_rows SET data = NULL, info = NULL WHERE batch_id = $1`, id); err != nil {
		return fmt.Errorf("wipe rows: %w", err)
	}
	if status == "discarded" {
		if _, err := tx.Exec(ctx, `UPDATE import_batches SET status = 'discarded' WHERE id = $1`, id); err != nil {
			return fmt.Errorf("discard batch: %w", err)
		}
	}
	return nil
}

// ErrImportApplied is returned for a batch that has already been applied: the
// caller reads its report instead of repeating it.
var ErrImportApplied = errors.New("workshop: this batch has been applied already")

// ApplyImport writes a previewed batch, all of it or none of it. Rows that
// cannot be imported are never written; they are reported, and applying with
// some of them is something the caller has to say it means (skipFailed). A row
// that may be somebody already known is not written until somebody has said
// which.
func ApplyImport(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string, decisions map[int]ImportDecision, skipFailed bool) (ImportBatch, error) {
	if !scope.Role.RunsTheShop() {
		return ImportBatch{}, access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return ImportBatch{}, ErrNotFound
	}
	var out ImportBatch
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM import_batches WHERE id = $1 FOR UPDATE`, id).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("lock batch: %w", err)
		}
		switch status {
		case "applied":
			return ErrImportApplied
		case "discarded":
			return fmt.Errorf("%w: that batch was discarded", ErrInvalid)
		}
		b, err := importBatchTx(ctx, tx, id, true)
		if err != nil {
			return err
		}
		failed := 0
		var undecided []string
		for i := range b.Rows {
			r := &b.Rows[i]
			if d, ok := decisions[r.No]; ok && len(r.Candidates) > 0 && r.Action == "create" {
				switch d.Choice {
				case "create", "skip":
					r.Decision, r.Target = d.Choice, ""
				case "link":
					valid := false
					for _, c := range r.Candidates {
						valid = valid || c.ID == d.Target
					}
					if !valid {
						return fmt.Errorf("%w: row %d cannot be linked to that", ErrInvalid, r.No)
					}
					r.Decision, r.Target = "link", d.Target
				default:
					return fmt.Errorf("%w: row %d: choose create, link or skip", ErrInvalid, r.No)
				}
			}
			if r.Action == "error" {
				failed++
			}
			if r.NeedsDecision() {
				undecided = append(undecided, strconv.Itoa(r.No))
			}
		}
		if len(undecided) > 0 {
			return fmt.Errorf("%w: say what to do about row %s: it may be somebody who is already here", ErrInvalid, strings.Join(undecided, ", "))
		}
		if failed > 0 && !skipFailed {
			return fmt.Errorf("%w: %d rows cannot be imported; correct the file, or choose to import the others", ErrInvalid, failed)
		}
		entities := map[int]string{}
		for i := range b.Rows {
			r := &b.Rows[i]
			if err := applyImportRowTx(ctx, tx, scope, b, r, entities); err != nil {
				return fmt.Errorf("row %d: %w", r.No, err)
			}
			entities[r.No] = r.EntityID
			switch r.Outcome {
			case "created":
				b.Created++
			case "updated":
				b.Updated++
			case "failed":
				b.Failed++
			default:
				b.Skipped++
			}
			if _, err := tx.Exec(ctx, `
				UPDATE import_rows SET decision = nullif($3, ''), decision_target = nullif($4, ''), outcome = $5,
				       entity_id = nullif($6, '')::uuid, message = nullif($7, '') WHERE batch_id = $1 AND row_no = $2`,
				id, r.No, r.Decision, r.Target, r.Outcome, r.EntityID, truncate(r.Message, 500)); err != nil {
				return fmt.Errorf("record row %d: %w", r.No, err)
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE import_batches SET status = 'applied', applied_by = $2, applied_at = clock_timestamp(),
			       created_count = $3, updated_count = $4, skipped_count = $5, failed_count = $6 WHERE id = $1`,
			id, scope.UserID, b.Created, b.Updated, b.Skipped, b.Failed); err != nil {
			return fmt.Errorf("close the batch: %w", err)
		}
		if err := wipeBatchTx(ctx, tx, id, "applied"); err != nil {
			return err
		}
		var err2 error
		out, err2 = importBatchTx(ctx, tx, id, true)
		return err2
	})
	return out, err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func recordRefTx(ctx context.Context, tx pgx.Tx, scope access.Scope, batchID, kind, external, entity string) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO import_refs (shop_id, kind, external_id, entity_id, batch_id) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (shop_id, kind, external_id) DO NOTHING`, scope.ShopID, kind, external, entity, batchID); err != nil {
		return fmt.Errorf("remember the reference: %w", err)
	}
	return nil
}

func applyImportRowTx(ctx context.Context, tx pgx.Tx, scope access.Scope, b ImportBatch, r *ImportRow, entities map[int]string) error {
	if r.Action == "error" {
		r.Outcome, r.Message = "failed", strings.Join(r.Errors, "; ")
		return nil
	}
	if r.Action == "unchanged" {
		r.Outcome = "unchanged"
		r.Message = strings.Join(r.Warnings, "; ")
		return nil
	}
	reason := "Import " + b.FileName
	v := r.Values
	switch b.Kind {
	case "customers":
		return applyCustomerTx(ctx, tx, scope, b, r, entities, reason)
	case "vehicles":
		return applyVehicleTx(ctx, tx, scope, b, r, reason)
	case "parts":
		return applyPartTx(ctx, tx, scope, b, r)
	case "stock":
		var partID string
		if err := tx.QueryRow(ctx, `
			SELECT id::text FROM parts WHERE number = $1 UNION ALL SELECT part_id::text FROM part_codes WHERE code = $1 LIMIT 1`, r.Ref).Scan(&partID); err != nil {
			return fmt.Errorf("find the part: %w", err)
		}
		var cost *int64
		if c := v["unit_cost"]; c != "" {
			n, _ := strconv.ParseInt(c, 10, 64)
			cost = &n
		}
		qty, _ := strconv.ParseInt(v["quantity"], 10, 64)
		if err := tx.QueryRow(ctx, `
			INSERT INTO stock_movements (shop_id, part_id, kind, quantity, unit_cost_minor, moved_by, moved_at, note, import_batch_id)
			VALUES ($1, $2, 'counted', $3::numeric / 1000, $4, $5,
			        ($6::date + time '12:00') AT TIME ZONE (SELECT timezone FROM shops WHERE id = $1), $7, $8)
			RETURNING id::text`,
			scope.ShopID, partID, qty, cost, scope.UserID, v["as_of"], "Opening stock, "+b.FileName, b.ID).Scan(&r.EntityID); err != nil {
			return fmt.Errorf("record the opening stock: %w", err)
		}
		r.Outcome = "created"
		return nil
	}
	return fmt.Errorf("unknown kind %q", b.Kind)
}

func applyCustomerTx(ctx context.Context, tx pgx.Tx, scope access.Scope, b ImportBatch, r *ImportRow, entities map[int]string, reason string) error {
	v := r.Values
	if r.Action == "update" {
		id, err := refEntityTx(ctx, tx, "customers", r.Ref)
		if err != nil {
			return err
		}
		if err := fillCustomerTx(ctx, tx, scope, id, v, r.Fills, reason); err != nil {
			return err
		}
		r.EntityID, r.Outcome, r.Message = id, "updated", "filled in: "+strings.Join(r.Fills, ", ")
		return nil
	}
	switch r.Decision {
	case "skip":
		r.Outcome, r.Message = "skipped", "chosen to skip"
		return nil
	case "link":
		target := r.Target
		if strings.HasPrefix(target, "row:") {
			n, _ := strconv.Atoi(strings.TrimPrefix(target, "row:"))
			target = entities[n]
			if target == "" {
				return errors.New("the earlier row it was linked to was not imported")
			}
		}
		if err := recordRefTx(ctx, tx, scope, b.ID, "customers", r.Ref, target); err != nil {
			return err
		}
		r.EntityID, r.Outcome, r.Message = target, "linked", "same as an existing customer"
		return nil
	}
	id, err := createCustomerTx(ctx, tx, scope, NewCustomer{
		Kind: v["type"], Name: v["name"], ContactName: v["contact"], OrgNumber: v["org_number"], VATNumber: v["vat_number"],
		Phone: v["phone"], Email: v["email"], AddressLine1: v["address"], AddressLine2: v["address2"],
		PostalCode: v["postal_code"], City: v["city"]})
	if err != nil {
		return err
	}
	if err := recordRefTx(ctx, tx, scope, b.ID, "customers", r.Ref, id); err != nil {
		return err
	}
	r.EntityID, r.Outcome = id, "created"
	return nil
}

func fillCustomerTx(ctx context.Context, tx pgx.Tx, scope access.Scope, id string, v map[string]string, fills []string, reason string) error {
	var personID *string
	var name, kind string
	if err := tx.QueryRow(ctx, `SELECT person_id::text, kind, coalesce(company_name, '') FROM customers WHERE id = $1`, id).Scan(&personID, &kind, &name); err != nil {
		return fmt.Errorf("read customer: %w", err)
	}
	for _, f := range fills {
		var q, field string
		switch f {
		case "org_number", "vat_number", "postal_code", "city":
			q, field = fmt.Sprintf(`UPDATE customers SET %s = $2, revision = revision + 1, updated_at = now() WHERE id = $1`, f), f
		case "address":
			q, field = `UPDATE customers SET address_line1 = $2, revision = revision + 1, updated_at = now() WHERE id = $1`, "address_line1"
		case "address2":
			q, field = `UPDATE customers SET address_line2 = $2, revision = revision + 1, updated_at = now() WHERE id = $1`, "address_line2"
		case "phone", "email":
			if personID == nil {
				contact := v["contact"]
				if contact == "" {
					contact = v["name"]
				}
				var pid string
				if err := tx.QueryRow(ctx, `INSERT INTO people (shop_id, display_name) VALUES ($1, $2) RETURNING id::text`, scope.ShopID, contact).Scan(&pid); err != nil {
					return fmt.Errorf("create contact: %w", err)
				}
				if _, err := tx.Exec(ctx, `UPDATE customers SET person_id = $2 WHERE id = $1`, id, pid); err != nil {
					return fmt.Errorf("attach contact: %w", err)
				}
				personID = &pid
			}
			if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE people SET %s = $2 WHERE id = $1`, f), *personID, v[f]); err != nil {
				return fmt.Errorf("fill %s: %w", f, err)
			}
			if err := logChangeTx(ctx, tx, scope, "customer", id, *personID, f, "", v[f], reason); err != nil {
				return err
			}
			continue
		case "name":
			if kind == "company" && name == "" {
				q, field = `UPDATE customers SET company_name = $2, revision = revision + 1, updated_at = now() WHERE id = $1`, "name"
			} else {
				continue
			}
		default:
			continue
		}
		if _, err := tx.Exec(ctx, q, id, v[f]); err != nil {
			return fmt.Errorf("fill %s: %w", f, err)
		}
		if err := logChangeTx(ctx, tx, scope, "customer", id, "", field, "", v[f], reason); err != nil {
			return err
		}
	}
	return nil
}

func applyVehicleTx(ctx context.Context, tx pgx.Tx, scope access.Scope, b ImportBatch, r *ImportRow, reason string) error {
	v := r.Values
	owner := ""
	if ref := v["owner_ref"]; ref != "" {
		var err error
		if owner, err = refEntityTx(ctx, tx, "customers", ref); err != nil {
			return err
		}
	}
	var vehicleID string
	err := tx.QueryRow(ctx, `SELECT vehicle_id::text FROM vehicle_registrations WHERE normalised = $1 AND valid_to IS NULL`, r.Ref).Scan(&vehicleID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("look up registration: %w", err)
	}
	created := vehicleID == ""
	var year *int
	if y := v["year"]; y != "" {
		n, _ := strconv.Atoi(y)
		year = &n
	}
	if created {
		if err := tx.QueryRow(ctx, `
			INSERT INTO vehicles (shop_id, make, model, model_year, engine, vin)
			VALUES ($1, nullif($2,''), nullif($3,''), $4, nullif($5,''), nullif($6,'')) RETURNING id::text`,
			scope.ShopID, v["make"], v["model"], year, v["engine"], v["vin"]).Scan(&vehicleID); err != nil {
			return fmt.Errorf("create vehicle: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO vehicle_registrations (shop_id, vehicle_id, registration, normalised) VALUES ($1, $2, $3, $4)`,
			scope.ShopID, vehicleID, v["registration"], r.Ref); err != nil {
			return fmt.Errorf("record registration: %w", err)
		}
		if owner != "" {
			if err := recordOwnerTx(ctx, tx, scope, vehicleID, owner); err != nil {
				return err
			}
		}
	} else {
		for _, f := range r.Fills {
			var q string
			var arg any = v[f]
			switch f {
			case "make", "model", "engine", "vin":
				q = fmt.Sprintf(`UPDATE vehicles SET %s = $2, revision = revision + 1, updated_at = now() WHERE id = $1`, f)
			case "year":
				q, arg = `UPDATE vehicles SET model_year = $2, revision = revision + 1, updated_at = now() WHERE id = $1`, year
			case "owner":
				if err := recordOwnerTx(ctx, tx, scope, vehicleID, owner); err != nil {
					return err
				}
				continue
			default:
				continue
			}
			if _, err := tx.Exec(ctx, q, vehicleID, arg); err != nil {
				return fmt.Errorf("fill %s: %w", f, err)
			}
			if err := logChangeTx(ctx, tx, scope, "vehicle", vehicleID, "", f, "", v[f], reason); err != nil {
				return err
			}
		}
	}
	if km := v["odometer_km"]; km != "" && (created || listHas(r.Fills, "odometer")) {
		readAt := "now()"
		args := []any{scope.ShopID, vehicleID, km}
		if d := v["odometer_date"]; d != "" {
			readAt = "($4::date + time '12:00') AT TIME ZONE (SELECT timezone FROM shops WHERE id = $1)"
			args = append(args, d)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO odometer_readings (shop_id, vehicle_id, km, source, read_at, decreasing, note)
			SELECT $1, $2, $3::bigint, 'import', `+readAt+`,
			       exists(SELECT 1 FROM odometer_readings WHERE vehicle_id = $2 AND km > $3::bigint AND read_at < `+readAt+`),
			       'Import `+strings.ReplaceAll(b.FileName, "'", "''")+`'`, args...); err != nil {
			return fmt.Errorf("record odometer: %w", err)
		}
	}
	if err := recordRefTx(ctx, tx, scope, b.ID, "vehicles", r.Ref, vehicleID); err != nil {
		return err
	}
	r.EntityID = vehicleID
	if created {
		r.Outcome = "created"
		if v["owner_ref"] != "" && owner == "" {
			r.Message = "created without an owner: " + v["owner_ref"] + " is not known"
		}
	} else {
		r.Outcome, r.Message = "updated", "filled in: "+strings.Join(r.Fills, ", ")
	}
	return nil
}

func listHas(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func applyPartTx(ctx context.Context, tx pgx.Tx, scope access.Scope, b ImportBatch, r *ImportRow) error {
	v := r.Values
	num := func(col string) *int64 {
		if v[col] == "" {
			return nil
		}
		n, _ := strconv.ParseInt(v[col], 10, 64)
		return &n
	}
	var id string
	if r.Action == "create" {
		if err := tx.QueryRow(ctx, `
			INSERT INTO parts (shop_id, number, name, unit, location, cost_minor, price_minor, minimum_quantity)
			VALUES ($1, $2, $3, $4, nullif($5, ''), $6, $7, coalesce($8, 0)::numeric / 1000) RETURNING id::text`,
			scope.ShopID, v["number"], v["name"], v["unit"], v["location"], num("cost"), num("price"), num("minimum")).Scan(&id); err != nil {
			return fmt.Errorf("create part: %w", err)
		}
		r.Outcome = "created"
	} else {
		if err := tx.QueryRow(ctx, `SELECT id::text FROM parts WHERE number = $1`, r.Ref).Scan(&id); err != nil {
			return fmt.Errorf("find part: %w", err)
		}
		for _, f := range r.Fills {
			var q string
			var arg any
			switch f {
			case "cost":
				q, arg = `UPDATE parts SET cost_minor = $2, updated_at = now() WHERE id = $1`, num("cost")
			case "price":
				q, arg = `UPDATE parts SET price_minor = $2, updated_at = now() WHERE id = $1`, num("price")
			case "minimum":
				q, arg = `UPDATE parts SET minimum_quantity = $2::numeric / 1000, updated_at = now() WHERE id = $1`, num("minimum")
			case "location":
				q, arg = `UPDATE parts SET location = $2, updated_at = now() WHERE id = $1`, v["location"]
			default:
				continue
			}
			if _, err := tx.Exec(ctx, q, id, arg); err != nil {
				return fmt.Errorf("fill %s: %w", f, err)
			}
		}
		r.Outcome, r.Message = "updated", "filled in: "+strings.Join(r.Fills, ", ")
	}
	r.EntityID = id
	return recordRefTx(ctx, tx, scope, b.ID, "parts", r.Ref, id)
}

// expireImportsTx wipes the previews nobody applied, so a file's contents do
// not sit in the database because somebody changed their mind.
func expireImportsTx(ctx context.Context, tx pgx.Tx, after time.Duration) (int64, error) {
	rows, err := tx.Query(ctx, `
		SELECT id::text FROM import_batches WHERE status = 'previewed' AND created_at < now() - $1::interval`,
		fmt.Sprintf("%d seconds", int(after.Seconds())))
	if err != nil {
		return 0, fmt.Errorf("find old previews: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := wipeBatchTx(ctx, tx, id, "discarded"); err != nil {
			return 0, err
		}
	}
	return int64(len(ids)), nil
}

// Title is what the row is about, in a line.
func (r ImportRow) Title() string {
	v := r.Values
	if v == nil {
		return r.Ref
	}
	var parts []string
	add := func(keys ...string) {
		for _, k := range keys {
			if v[k] != "" {
				parts = append(parts, v[k])
			}
		}
	}
	switch {
	case v["part_number"] != "":
		add("part_number")
		if v["quantity"] != "" {
			n, _ := strconv.ParseInt(v["quantity"], 10, 64)
			parts = append(parts, fmt.Sprintf("× %s", strings.TrimRight(strings.TrimRight(fmt.Sprintf("%d.%03d", n/1000, n%1000), "0"), ".")))
		}
		add("as_of")
	case v["registration"] != "":
		add("registration", "make", "model")
	case v["number"] != "":
		add("number", "name")
	default:
		add("name")
	}
	if len(parts) == 0 {
		return r.Ref
	}
	return strings.Join(parts, " ")
}

// ImportColumns are the columns of a kind of file, in the template's order.
func ImportColumns(kind string) []string { return importColumns[kind] }

// ImportRequired is whether a column cannot be left out of a kind of file.
func ImportRequired(kind, column string) bool {
	for _, c := range importRequired[kind] {
		if c == column {
			return true
		}
	}
	return false
}

// ImportKindLabel is a kind of file, as a catalogue key.
func ImportKindLabel(kind string) string {
	switch kind {
	case "customers":
		return "customers"
	case "vehicles":
		return "vehicles"
	case "parts":
		return "parts"
	case "stock":
		return "opening stock"
	}
	return kind
}
