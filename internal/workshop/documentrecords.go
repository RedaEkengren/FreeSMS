package workshop

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// Document records (#150): a contract whose content is frozen when it is
// produced, and whose signing is a separate fact recorded after. The text is
// the shop's own, from a template version that never changes; the parties
// are copied onto it as they were; a hash of the whole is what a signature
// says it signed. A correction is a new document superseding the old, which
// stays readable. Migration 0071 freezes them by trigger, as invoices are.

// The kinds of document.
const (
	DocPurchaseAgreement    = "purchase_agreement"
	DocSalesContract        = "sales_contract"
	DocTestDriveAgreement   = "test_drive_agreement"
	DocConsignmentAgreement = "consignment_agreement"
	DocRentalAgreement      = "rental_agreement"
)

// DocumentKinds is every kind, in the order a page lists them.
var DocumentKinds = []string{DocPurchaseAgreement, DocConsignmentAgreement, DocSalesContract, DocTestDriveAgreement, DocRentalAgreement}

// documentKindLabels are the kinds as a page heads them, catalogue keys.
var documentKindLabels = map[string]string{
	DocPurchaseAgreement:    "Purchase agreement",
	DocSalesContract:        "Sales contract",
	DocTestDriveAgreement:   "Test-drive agreement",
	DocConsignmentAgreement: "Consignment agreement",
	DocRentalAgreement:      "Rental agreement",
}

// DocumentKindLabel is a kind's heading, a catalogue key.
func DocumentKindLabel(kind string) string { return documentKindLabels[kind] }

// The methods of signing.
const (
	SignedOnPaper   = "paper"
	SignedInPerson  = "in_person_click"
	SignedProvider  = "provider"
	documentLinkTTL = 14 // days, as an invoice's link
)

// defaultRetentionYears is how long a kind is kept with no setting: the
// bookkeeping law's seven years for what supports a sale or a purchase.
const defaultRetentionYears = 7

var (
	errDocKind       = fmt.Errorf("%w: that is not a kind of document", ErrInvalid)
	errDocFrozen     = fmt.Errorf("%w: a document is frozen once produced; a correction is a new one that supersedes it", ErrInvalid)
	errDocSuperseded = fmt.Errorf("%w: that document has been superseded; sign the one that replaced it", ErrInvalid)
	errDocChanged    = fmt.Errorf("%w: what was signed is not this document as it is; show it again and sign that", ErrInvalid)
	errDocSigned     = fmt.Errorf("%w: that party has already signed", ErrInvalid)
)

// ErrSignerUnavailable is a signing service that could not be reached. The
// document stays unsigned and everything else carries on: it can be signed
// on paper, or asked for again.
var ErrSignerUnavailable = errors.New("workshop: the signing service could not be reached")

// mayDocument checks roles, and for writing the scope's modules, for a kind
// of document: the buyer's purchases and consignments, the salesperson's
// sales and test drives, the rental desk's rentals -- and whoever runs the
// business, every one. Nobody else is shown a contract at all: what it says
// about a person is for those who made it with them. Reading is Core's, so a
// module switched off keeps its contracts readable; producing and signing
// are new work, and the module must run.
func mayDocument(roles access.Roles, scope access.Scope, kind string, writing bool) error {
	if err := mayReadDocument(roles, kind); err != nil {
		return err
	}
	if !writing {
		return nil
	}
	module := access.Dealer
	if kind == DocRentalAgreement {
		module = access.Rental
	}
	// Written out per module, so the permission contract sees the gate.
	if module == access.Rental {
		return scope.Require(access.Rental)
	}
	return scope.Require(access.Dealer)
}

// mayReadDocument is mayDocument for reading, which is Core's and asks no
// module: whoever makes a kind of contract reads it.
func mayReadDocument(roles access.Roles, kind string) error {
	var may bool
	switch kind {
	case DocPurchaseAgreement, DocConsignmentAgreement:
		may = roles.BuysVehicles()
	case DocSalesContract, DocTestDriveAgreement:
		may = roles.SellsVehicles()
	case DocRentalAgreement:
		may = roles.HandsOverRentals()
	default:
		return errDocKind
	}
	if !may {
		return access.ErrForbidden
	}
	return nil
}

// docError turns the database's refusals into sentences.
func docError(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch {
		case pg.Code == "23001" && strings.Contains(pg.Message, "superseded"):
			return errDocSuperseded
		case pg.Code == "23001":
			return errDocFrozen
		case pg.Code == "23514" && strings.Contains(pg.Message, "other than the document"):
			return errDocChanged
		case pg.Code == "23505" && pg.ConstraintName == "document_signatures_document_party_id_key":
			return errDocSigned
		case pg.Code == "23505" && pg.ConstraintName == "document_records_supersedes_id_key":
			return fmt.Errorf("%w: that document has already been superseded", ErrInvalid)
		}
	}
	return err
}

// DocumentTemplate is a template, at its latest version.
type DocumentTemplate struct {
	ID        string
	Kind      string
	Name      string
	VersionID string
	Version   int
	Body      string
}

// SaveDocumentTemplate writes a template's next version -- a new template,
// with id empty. The versions before stay as they were, so every document
// made from one still says which words it was made from. The shop's words,
// kept by whoever runs the shop, as the checklists are.
func SaveDocumentTemplate(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id, kind, name, body string) (DocumentTemplate, error) {
	if !scope.Role.RunsTheShop() {
		return DocumentTemplate{}, access.ErrForbidden
	}
	if !slices.Contains(DocumentKinds, kind) {
		return DocumentTemplate{}, errDocKind
	}
	name, body = strings.TrimSpace(name), strings.TrimSpace(body)
	switch {
	case name == "" || utf8.RuneCountInString(name) > 120:
		return DocumentTemplate{}, fmt.Errorf("%w: a template needs a name, in 120 characters or fewer", ErrInvalid)
	case body == "" || utf8.RuneCountInString(body) > 20000:
		return DocumentTemplate{}, fmt.Errorf("%w: a template needs its text, in 20 000 characters or fewer", ErrInvalid)
	case id != "" && !looksLikeUUID(id):
		return DocumentTemplate{}, ErrNotFound
	}
	t := DocumentTemplate{ID: id, Kind: kind, Name: name, Body: body}
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if t.ID == "" {
			if err := tx.QueryRow(ctx, `INSERT INTO document_templates (shop_id, kind, name, created_by) VALUES ($1, $2, $3, NULLIF($4, '')::uuid) RETURNING id`,
				scope.ShopID, kind, name, scope.UserID).Scan(&t.ID); err != nil {
				return fmt.Errorf("create template: %w", err)
			}
		} else {
			var was string
			err := tx.QueryRow(ctx, `SELECT kind FROM document_templates WHERE id = $1 FOR UPDATE`, t.ID).Scan(&was)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return fmt.Errorf("read template: %w", err)
			}
			if was != kind {
				return fmt.Errorf("%w: a template stays the kind it was made for", ErrInvalid)
			}
			if _, err := tx.Exec(ctx, `UPDATE document_templates SET name = $2 WHERE id = $1`, t.ID, name); err != nil {
				return fmt.Errorf("rename template: %w", err)
			}
		}
		return tx.QueryRow(ctx, `
			INSERT INTO document_template_versions (shop_id, template_id, version, body, created_by)
			SELECT $1, $2, coalesce(max(version), 0) + 1, $3, NULLIF($4, '')::uuid FROM document_template_versions WHERE template_id = $2
			RETURNING id, version`, scope.ShopID, t.ID, body, scope.UserID).Scan(&t.VersionID, &t.Version)
	})
	return t, err
}

// DocumentTemplates lists the templates of a kind at their latest
// versions, for those who make that kind of document.
func DocumentTemplates(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, kind string) ([]DocumentTemplate, error) {
	if err := mayDocument(scope.Role, scope, kind, false); err != nil {
		return nil, err
	}
	var out []DocumentTemplate
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT t.id::text, t.kind, t.name, v.id::text, v.version, v.body
			FROM document_templates t
			JOIN LATERAL (SELECT * FROM document_template_versions v WHERE v.template_id = t.id ORDER BY v.version DESC LIMIT 1) v ON true
			WHERE t.kind = $1 ORDER BY t.name`, kind)
		if err != nil {
			return fmt.Errorf("read templates: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var t DocumentTemplate
			if err := rows.Scan(&t.ID, &t.Kind, &t.Name, &t.VersionID, &t.Version, &t.Body); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

// DocumentPartyRole is a party's part in a document: the buyer, the
// seller, the renter.
type DocumentPartyRole struct {
	PartyID string
	Role    string // buyer, seller, renter, consignor, driver, guarantor
}

// NewDocumentRecord is a document to produce: of which kind, about what,
// from which template version -- the one previewed, not whichever is newest
// -- with the parties and the data its text is filled from.
type NewDocumentRecord struct {
	Kind              string
	SubjectKind       string
	SubjectID         string
	TemplateVersionID string
	Title             string
	Parties           []DocumentPartyRole
	// Filled into the template where it says {name}. The shop's own words
	// around them; the values are what the module that produces it knows.
	Data       map[string]string
	Supersedes string
}

var documentSubjects = []string{"sale", "purchase", "rental_booking", "consignment", "test_drive"}
var documentPartyRoles = []string{"buyer", "seller", "renter", "consignor", "driver", "guarantor"}

// render fills a template's {name} placeholders from data. Deterministic: the
// same template version and data always give the same text, so a document
// can be shown to say what its data says. A placeholder with no value is
// left as written, visibly.
func render(body string, data map[string]string) string {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	pairs := make([]string, 0, 2*len(keys))
	for _, k := range keys {
		pairs = append(pairs, "{"+k+"}", data[k])
	}
	return strings.NewReplacer(pairs...).Replace(body)
}

// DocumentParty is a party as a document names it, and whether they have
// signed.
type DocumentParty struct {
	ID        string
	PartyID   string
	Role      string
	Name      string
	OrgNumber string
	Address   string
	Signature *DocumentSignature
}

// DocumentSignature is the evidence of one party's signing.
type DocumentSignature struct {
	Method      string
	SignedAt    time.Time
	RecordedBy  string
	ProviderRef string
	EvidenceKey string
	WitnessedBy string
}

// DocumentRecord is a produced document as it is read.
type DocumentRecord struct {
	ID              string
	Kind            string
	SubjectKind     string
	SubjectID       string
	TemplateVersion int
	TemplateName    string
	Title           string
	Body            string
	Data            map[string]string
	Hash            string // hex
	SupersedesID    string
	SupersededByID  string
	CreatedAt       time.Time
	CreatedBy       string
	ErasedAt        *time.Time
	Parties         []DocumentParty
}

// Signing is where a document stands: unsigned, partly signed or fully
// signed, from its signatures.
func (d DocumentRecord) Signing() string {
	signed := 0
	for _, p := range d.Parties {
		if p.Signature != nil {
			signed++
		}
	}
	switch {
	case signed == 0:
		return "unsigned"
	case signed < len(d.Parties):
		return "partly signed"
	}
	return "fully signed"
}

// SigningLabel is Signing as a page says it, a catalogue key.
func (d DocumentRecord) SigningLabel() string {
	switch d.Signing() {
	case "unsigned":
		return "Not signed by anybody yet"
	case "partly signed":
		return "Signed by some of the parties"
	}
	return "Signed by every party"
}

// KindLabel is the document's kind as a page heads it, a catalogue key.
func (d DocumentRecord) KindLabel() string { return DocumentKindLabel(d.Kind) }

// RoleLabel is a party's part in a document, a catalogue key.
func (p DocumentParty) RoleLabel() string {
	return map[string]string{
		"buyer": "The buyer", "seller": "The seller", "renter": "The renter", "consignor": "The consignor",
		"driver": "The driver", "guarantor": "The guarantor",
	}[p.Role]
}

// MethodLabel is how a party signed, a catalogue key.
func (s DocumentSignature) MethodLabel() string {
	return map[string]string{
		SignedOnPaper: "on paper", SignedInPerson: "in person, on the screen", SignedProvider: "through a signing service",
	}[s.Method]
}

// ShortHash is the start of the hash, printed on a paper copy so a scan can
// be matched to the version signed.
func (d DocumentRecord) ShortHash() string {
	if len(d.Hash) < 12 {
		return d.Hash
	}
	return d.Hash[:12]
}

// contentHash is the hash of everything a document says, in a fixed order:
// what a signature names as what was signed.
func contentHash(kind, title, body string, data map[string]string, parties []DocumentParty) []byte {
	type party struct{ Role, Name, OrgNumber, Address string }
	ps := make([]party, len(parties))
	for i, p := range parties {
		ps[i] = party{p.Role, p.Name, p.OrgNumber, p.Address}
	}
	// encoding/json writes a map's keys in order, so this is the same bytes
	// for the same content.
	b, _ := json.Marshal(struct {
		Kind, Title, Body string
		Data              map[string]string
		Parties           []party
	}{kind, title, body, data, ps})
	sum := sha256.Sum256(b)
	return sum[:]
}

// PreviewDocument renders what a document would say, without producing it.
func PreviewDocument(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, kind, templateVersionID string, data map[string]string) (string, error) {
	if err := mayDocument(scope.Role, scope, kind, true); err != nil {
		return "", err
	}
	if !looksLikeUUID(templateVersionID) {
		return "", ErrNotFound
	}
	var body string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		body, err = templateBodyTx(ctx, tx, templateVersionID, kind)
		return err
	})
	return render(body, data), err
}

func templateBodyTx(ctx context.Context, tx pgx.Tx, versionID, kind string) (string, error) {
	var body string
	err := tx.QueryRow(ctx, `
		SELECT v.body FROM document_template_versions v JOIN document_templates t ON t.id = v.template_id
		WHERE v.id = $1 AND t.kind = $2`, versionID, kind).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: that template is not one for this kind of document", ErrInvalid)
	}
	return body, err
}

// ProduceDocument freezes a document: the template version's text filled
// from the data, the parties copied as they are now, and the hash of it all.
// A correction names the document it supersedes, of the same kind and the
// same subject; the old one stays, and can no longer be signed.
func ProduceDocument(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, n NewDocumentRecord) (string, error) {
	if err := mayDocument(scope.Role, scope, n.Kind, true); err != nil {
		return "", err
	}
	n.Title = strings.TrimSpace(n.Title)
	switch {
	case !slices.Contains(documentSubjects, n.SubjectKind):
		return "", fmt.Errorf("%w: that is not something a document can be about", ErrInvalid)
	case !looksLikeUUID(n.SubjectID) || !looksLikeUUID(n.TemplateVersionID) || (n.Supersedes != "" && !looksLikeUUID(n.Supersedes)):
		return "", ErrNotFound
	case n.Title == "" || utf8.RuneCountInString(n.Title) > 200:
		return "", fmt.Errorf("%w: a document needs a title, in 200 characters or fewer", ErrInvalid)
	case len(n.Parties) == 0:
		return "", fmt.Errorf("%w: a document is between parties; name at least one", ErrInvalid)
	}
	for _, p := range n.Parties {
		if !looksLikeUUID(p.PartyID) {
			return "", ErrNotFound
		}
		if !slices.Contains(documentPartyRoles, p.Role) {
			return "", fmt.Errorf("%w: that is not a part a party can have in a document", ErrInvalid)
		}
	}
	if n.Data == nil {
		n.Data = map[string]string{}
	}
	var id string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		tpl, err := templateBodyTx(ctx, tx, n.TemplateVersionID, n.Kind)
		if err != nil {
			return err
		}
		if n.Supersedes != "" {
			var same bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM document_records WHERE id = $1 AND kind = $2 AND subject_id = $3)`,
				n.Supersedes, n.Kind, n.SubjectID).Scan(&same); err != nil {
				return fmt.Errorf("read what it supersedes: %w", err)
			}
			if !same {
				return fmt.Errorf("%w: a correction supersedes a document of the same kind about the same thing", ErrInvalid)
			}
		}
		// Naming a party copies its name and address onto the document, so
		// it is reading the party: only one the caller may see (PartyByID),
		// and read the way the party's own page reads it.
		seen := partyRolesSeen(scope.Role, PartyRoles)
		parties := make([]DocumentParty, len(n.Parties))
		for i, p := range n.Parties {
			var visible bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM party_roles r WHERE r.party_id = ANY((SELECT party_ids_read_as(party_now($1)))::uuid[]) AND r.role = ANY($2))`,
				p.PartyID, seen).Scan(&visible); err != nil {
				return fmt.Errorf("read party: %w", err)
			}
			if !visible {
				return ErrNotFound
			}
			var pa Party
			err := tx.QueryRow(ctx, `SELECT`+partyColumns+partyJoins+` WHERE pa.id = $1`, p.PartyID, false).Scan(
				&pa.ID, &pa.Kind, &pa.Name, &pa.OrgNumber, &pa.Phone, &pa.Email, &pa.Address, &pa.CustomerID, &pa.MergedInto, &pa.Revision)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return fmt.Errorf("read party: %w", err)
			}
			if pa.MergedInto != "" {
				return errPartyMerged
			}
			parties[i] = DocumentParty{PartyID: p.PartyID, Role: p.Role, Name: pa.Name, OrgNumber: pa.OrgNumber, Address: pa.Address}
		}
		body := render(tpl, n.Data)
		data, err := json.Marshal(n.Data)
		if err != nil {
			return err
		}
		hash := contentHash(n.Kind, n.Title, body, n.Data, parties)
		if err := tx.QueryRow(ctx, `
			INSERT INTO document_records (shop_id, kind, subject_kind, subject_id, template_version_id, title, body, data,
			                              content_sha256, supersedes_id, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NULLIF($10, '')::uuid, $11) RETURNING id`,
			scope.ShopID, n.Kind, n.SubjectKind, n.SubjectID, n.TemplateVersionID, n.Title, body, data, hash,
			n.Supersedes, scope.UserID).Scan(&id); err != nil {
			return docError(err)
		}
		for i, p := range parties {
			if _, err := tx.Exec(ctx, `
				INSERT INTO document_record_parties (shop_id, document_id, party_id, position, role, name, org_number, address)
				VALUES ($1, $2, $3, $4, $5, $6, nullif($7, ''), nullif($8, ''))`,
				scope.ShopID, id, p.PartyID, i+1, p.Role, p.Name, p.OrgNumber, p.Address); err != nil {
				if isUniqueViolation(err, "document_record_parties_document_id_party_id_role_key") {
					return fmt.Errorf("%w: a party is named once in each part", ErrInvalid)
				}
				return fmt.Errorf("name party: %w", err)
			}
		}
		return nil
	})
	return id, err
}

// DocumentRecordByID reads a document for whoever may read its kind.
func DocumentRecordByID(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string) (DocumentRecord, error) {
	if !looksLikeUUID(id) {
		return DocumentRecord{}, ErrNotFound
	}
	var d DocumentRecord
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var kind string
		err := tx.QueryRow(ctx, `SELECT kind FROM document_records WHERE id = $1`, id).Scan(&kind)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read document: %w", err)
		}
		if err := mayDocument(scope.Role, scope, kind, false); err != nil {
			return err
		}
		return documentRecordTx(ctx, tx, id, &d)
	})
	return d, err
}

// documentRecordTx reads a document whole, after the caller has decided it
// may be read.
func documentRecordTx(ctx context.Context, tx pgx.Tx, id string, d *DocumentRecord) error {
	var hash []byte
	var data []byte
	if err := tx.QueryRow(ctx, `
		SELECT r.id::text, r.kind, r.subject_kind, r.subject_id::text, v.version, t.name, r.title,
		       coalesce(r.body, ''), r.data, r.content_sha256, coalesce(r.supersedes_id::text, ''),
		       coalesce((SELECT n.id::text FROM document_records n WHERE n.supersedes_id = r.id), ''),
		       r.created_at, coalesce(p.display_name, ''), r.erased_at
		FROM document_records r
		JOIN document_template_versions v ON v.id = r.template_version_id
		JOIN document_templates t ON t.id = v.template_id
		LEFT JOIN users u ON u.id = r.created_by LEFT JOIN people p ON p.id = u.person_id
		WHERE r.id = $1`, id).Scan(&d.ID, &d.Kind, &d.SubjectKind, &d.SubjectID, &d.TemplateVersion, &d.TemplateName, &d.Title,
		&d.Body, &data, &hash, &d.SupersedesID, &d.SupersededByID, &d.CreatedAt, &d.CreatedBy, &d.ErasedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("read document: %w", err)
	}
	d.Hash = hex.EncodeToString(hash)
	if data != nil {
		if err := json.Unmarshal(data, &d.Data); err != nil {
			return fmt.Errorf("read the document's data: %w", err)
		}
	}
	rows, err := tx.Query(ctx, `
		SELECT dp.id::text, dp.party_id::text, dp.role, dp.name, coalesce(dp.org_number, ''), coalesce(dp.address, ''),
		       s.method, s.signed_at, coalesce(rp.display_name, ''), coalesce(s.provider_ref, ''), coalesce(s.evidence_key, ''),
		       coalesce(s.witnessed_by, '')
		FROM document_record_parties dp
		LEFT JOIN document_signatures s ON s.document_party_id = dp.id
		LEFT JOIN users ru ON ru.id = s.recorded_by LEFT JOIN people rp ON rp.id = ru.person_id
		WHERE dp.document_id = $1 ORDER BY dp.position`, id)
	if err != nil {
		return fmt.Errorf("read parties: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p DocumentParty
		var method *string
		var s DocumentSignature
		var at *time.Time
		if err := rows.Scan(&p.ID, &p.PartyID, &p.Role, &p.Name, &p.OrgNumber, &p.Address,
			&method, &at, &s.RecordedBy, &s.ProviderRef, &s.EvidenceKey, &s.WitnessedBy); err != nil {
			return fmt.Errorf("scan party: %w", err)
		}
		if method != nil {
			s.Method, s.SignedAt = *method, *at
			p.Signature = &s
		}
		d.Parties = append(d.Parties, p)
	}
	return rows.Err()
}

// SignatureEvidence is what a signing recorded by hand, or reported by a
// provider, rests on. Hash is the document's hash as the signer was shown
// it: a signature of anything else is refused.
type SignatureEvidence struct {
	Method      string
	Hash        string
	ProviderRef string
	// The scan of a paper copy, as stored.
	EvidenceKey string
	// Who saw it signed on paper.
	WitnessedBy string
}

// RecordSignature records that a party on a document signed it. Refused for
// a document a correction has superseded, for a hash that is not the
// document's, and for a party who has already signed -- a scan uploaded a
// second time adds nothing, and says so.
func RecordSignature(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, documentID, documentPartyID string, e SignatureEvidence) error {
	if !looksLikeUUID(documentID) || !looksLikeUUID(documentPartyID) {
		return ErrNotFound
	}
	switch e.Method {
	case SignedOnPaper, SignedInPerson, SignedProvider:
	default:
		return fmt.Errorf("%w: signed on paper, in person or through a provider", ErrInvalid)
	}
	e.WitnessedBy = strings.TrimSpace(e.WitnessedBy)
	if e.Method == SignedOnPaper && e.EvidenceKey == "" && e.WitnessedBy == "" {
		return fmt.Errorf("%w: a paper signature needs the scan or who saw it signed", ErrInvalid)
	}
	if e.Method == SignedProvider && strings.TrimSpace(e.ProviderRef) == "" {
		return fmt.Errorf("%w: a provider's signature needs its reference", ErrInvalid)
	}
	if utf8.RuneCountInString(e.WitnessedBy) > 120 || len(e.ProviderRef) > 200 || len(e.EvidenceKey) > 200 {
		return fmt.Errorf("%w: the evidence is longer than it can be", ErrInvalid)
	}
	hash, err := hex.DecodeString(strings.TrimSpace(e.Hash))
	if err != nil || len(hash) != sha256.Size {
		return errDocChanged
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var kind string
		err := tx.QueryRow(ctx, `SELECT kind FROM document_records WHERE id = $1`, documentID).Scan(&kind)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read document: %w", err)
		}
		if err := mayDocument(scope.Role, scope, kind, true); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO document_signatures (shop_id, document_id, document_party_id, method, recorded_by,
			                                 provider_ref, evidence_key, witnessed_by, document_sha256)
			VALUES ($1, $2, $3, $4, $5, nullif($6, ''), nullif($7, ''), nullif($8, ''), $9)`,
			scope.ShopID, documentID, documentPartyID, e.Method, scope.UserID, strings.TrimSpace(e.ProviderRef),
			e.EvidenceKey, e.WitnessedBy, hash)
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23514" && strings.Contains(pg.Message, "not on that document") {
			return ErrNotFound
		}
		return docError(err)
	})
}

// Signer asks somebody to sign a document: through a provider's service,
// or not at all, when it is signed on paper and recorded by hand. A
// provider's answer comes back later, as a RecordSignature with its
// reference.
type Signer interface {
	// Request asks the party to sign the document as it is, and returns the
	// provider's reference to follow it by; "" when there is nothing to
	// follow.
	Request(ctx context.Context, doc DocumentRecord, party DocumentParty) (ref string, err error)
}

// ManualSigner is signing on paper: print the document, have it signed,
// and record it -- upload the scan, or say who saw it signed. It asks
// nobody, so it never fails.
type ManualSigner struct{}

// Request has nothing to send.
func (ManualSigner) Request(context.Context, DocumentRecord, DocumentParty) (string, error) {
	return "", nil
}

// RequestSignature asks signer for a party's signature on a document. If
// the signer cannot be reached, the document stays unsigned and nothing
// else is held up: the error says so, wrapping ErrSignerUnavailable, and it
// can be signed on paper instead or asked for again.
func RequestSignature(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, signer Signer, documentID, documentPartyID string) (string, error) {
	if !looksLikeUUID(documentID) || !looksLikeUUID(documentPartyID) {
		return "", ErrNotFound
	}
	var d DocumentRecord
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var kind string
		err := tx.QueryRow(ctx, `SELECT kind FROM document_records WHERE id = $1`, documentID).Scan(&kind)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read document: %w", err)
		}
		if err := mayDocument(scope.Role, scope, kind, true); err != nil {
			return err
		}
		return documentRecordTx(ctx, tx, documentID, &d)
	})
	if err != nil {
		return "", err
	}
	if d.SupersededByID != "" {
		return "", errDocSuperseded
	}
	i := slices.IndexFunc(d.Parties, func(p DocumentParty) bool { return p.ID == documentPartyID })
	if i < 0 {
		return "", ErrNotFound
	}
	if d.Parties[i].Signature != nil {
		return "", errDocSigned
	}
	// Outside the transaction: a provider that hangs holds no lock.
	ref, err := signer.Request(ctx, d, d.Parties[i])
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrSignerUnavailable, err)
	}
	return ref, nil
}

// CreateDocumentRecordLink makes a link the customer can open the document
// on, for a fortnight, as an invoice's; any link made before is closed.
// The token is returned once and stored only as its hash.
func CreateDocumentRecordLink(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, documentID string) (string, error) {
	if !looksLikeUUID(documentID) {
		return "", ErrNotFound
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var kind string
		err := tx.QueryRow(ctx, `SELECT kind FROM document_records WHERE id = $1`, documentID).Scan(&kind)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read document: %w", err)
		}
		if err := mayDocument(scope.Role, scope, kind, false); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE document_record_links SET revoked_at = clock_timestamp(), revoked_by = $2
			WHERE document_id = $1 AND revoked_at IS NULL`, documentID, scope.UserID); err != nil {
			return fmt.Errorf("close earlier links: %w", err)
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO document_record_links (shop_id, document_id, token_sha256, created_by, expires_at)
			VALUES ($1, $2, $3, $4, clock_timestamp() + make_interval(days => $5))`,
			scope.ShopID, documentID, sum[:], scope.UserID, documentLinkTTL)
		return err
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

// OpenDocumentRecordLink opens a document on the customer's link. A wrong,
// expired or closed link is one answer. Opening is recorded.
func OpenDocumentRecordLink(ctx context.Context, pool *pgxpool.Pool, shopID, token string) (DocumentRecord, error) {
	if token == "" {
		return DocumentRecord{}, ErrShareNotUsable
	}
	sum := sha256.Sum256([]byte(token))
	var d DocumentRecord
	err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(ctx, `
			UPDATE document_record_links SET opens = opens + 1, last_opened_at = clock_timestamp(),
			       first_opened_at = coalesce(first_opened_at, clock_timestamp())
			WHERE token_sha256 = $1 AND revoked_at IS NULL AND expires_at > now()
			RETURNING document_id::text`, sum[:]).Scan(&id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrShareNotUsable
			}
			return fmt.Errorf("open link: %w", err)
		}
		return documentRecordTx(ctx, tx, id, &d)
	})
	if err != nil {
		return DocumentRecord{}, err
	}
	// The customer reads the document, not who at the shop recorded what.
	d.CreatedBy = ""
	for i := range d.Parties {
		if s := d.Parties[i].Signature; s != nil {
			s.RecordedBy, s.EvidenceKey = "", ""
		}
	}
	return d, nil
}

// DocumentRetention is how long each kind is kept before a person's
// erasure may blank it, in years: the latest setting, or seven.
func DocumentRetention(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) (map[string]int, error) {
	if !scope.Role.RunsTheShop() {
		return nil, access.ErrForbidden
	}
	out := map[string]int{}
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = retentionTx(ctx, tx)
		return err
	})
	return out, err
}

func retentionTx(ctx context.Context, tx pgx.Tx) (map[string]int, error) {
	out := map[string]int{}
	for _, k := range DocumentKinds {
		out[k] = defaultRetentionYears
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT ON (kind) kind, years FROM document_retention_settings ORDER BY kind, set_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("read retention: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var y int
		if err := rows.Scan(&k, &y); err != nil {
			return nil, err
		}
		out[k] = y
	}
	return out, rows.Err()
}

// SetDocumentRetention sets how long a kind is kept, as a row: what it was
// before stays. Whoever runs the shop, who answers for it.
func SetDocumentRetention(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, kind string, years int) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if !slices.Contains(DocumentKinds, kind) {
		return errDocKind
	}
	if years < 1 || years > 30 {
		return fmt.Errorf("%w: between 1 and 30 years", ErrInvalid)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO document_retention_settings (shop_id, kind, years, set_by) VALUES ($1, $2, $3, $4)`,
			scope.ShopID, kind, years, scope.UserID)
		return err
	})
}

// eraseDocumentsTx takes a person being erased off the documents naming
// them -- those past their kind's retention, and only those: the rest the
// law has the shop keep, whole, scan included, until their retention ends,
// and the erasure says so. Within retention the bookkeeping law wins;
// after it, the erasure does.
//
// Past retention, what goes:
//   - what the document said, its text and data, whole: it names everyone
//     on it, and a contract's text cannot be blanked one person at a time
//     with any certainty that nothing about them is left;
//   - the erased person's own naming as a party -- only theirs: the other
//     party asked for nothing and stays named;
//   - their name in the title, a heading somebody typed;
//   - the scan of the signed paper copy, from every signature on it. A scan
//     is a picture of the whole contract, them and their signature
//     included, and a picture cannot be blanked in part, so it goes as the
//     text does. The other party has lost nothing they are owed: the shop
//     need no longer keep it, their own copy is theirs, and the hash stays
//     to match it to what was signed.
//
// What stays of a blanked document is that it existed: its kind, its
// dates, its hash, and each signature -- who, how, when, recorded by
// whom. A document already blanked for somebody else still loses this
// person's naming and their name in its title. Returns how many were
// blanked and kept, and the scans whose files the caller removes once the
// transaction has committed.
func eraseDocumentsTx(ctx context.Context, tx pgx.Tx, personID string) (blanked, kept int, scans []string, err error) {
	years, err := retentionTx(ctx, tx)
	if err != nil {
		return 0, 0, nil, err
	}
	rows, err := tx.Query(ctx, `
		SELECT r.id::text, r.kind, r.created_at, r.erased_at IS NOT NULL, r.title,
		       array_agg(dp.id::text) FILTER (WHERE pa.kind = 'private'),
		       array_agg(dp.name) FILTER (WHERE pa.kind = 'private')
		FROM document_records r JOIN document_record_parties dp ON dp.document_id = r.id
		JOIN parties pa ON pa.id = dp.party_id LEFT JOIN customers c ON c.id = pa.customer_id
		WHERE pa.person_id = $1 OR c.person_id = $1
		GROUP BY r.id`, personID)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("read documents: %w", err)
	}
	type doc struct {
		id, kind, title string
		at              time.Time
		erased          bool
		parties, names  []string
	}
	var docs []doc
	for rows.Next() {
		var d doc
		if err := rows.Scan(&d.id, &d.kind, &d.at, &d.erased, &d.title, &d.parties, &d.names); err != nil {
			rows.Close()
			return 0, 0, nil, err
		}
		docs = append(docs, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, nil, err
	}
	for _, d := range docs {
		if !d.erased && time.Now().Before(d.at.AddDate(years[d.kind], 0, 0)) {
			kept++
			continue
		}
		title := withoutNames(d.title, d.names)
		if !d.erased {
			if _, err := tx.Exec(ctx, `UPDATE document_records SET body = NULL, data = NULL, title = $2, erased_at = now() WHERE id = $1`,
				d.id, title); err != nil {
				return 0, 0, nil, fmt.Errorf("blank document: %w", err)
			}
			srows, err := tx.Query(ctx, `
				UPDATE document_signatures s SET evidence_key = NULL
				FROM (SELECT id, evidence_key FROM document_signatures WHERE document_id = $1 AND evidence_key IS NOT NULL FOR UPDATE) old
				WHERE s.id = old.id RETURNING old.evidence_key`, d.id)
			if err != nil {
				return 0, 0, nil, fmt.Errorf("remove the scans: %w", err)
			}
			for srows.Next() {
				var k string
				if err := srows.Scan(&k); err != nil {
					srows.Close()
					return 0, 0, nil, err
				}
				scans = append(scans, k)
			}
			srows.Close()
			if err := srows.Err(); err != nil {
				return 0, 0, nil, fmt.Errorf("remove the scans: %w", err)
			}
			blanked++
		} else if title != d.title {
			if _, err := tx.Exec(ctx, `UPDATE document_records SET title = $2 WHERE id = $1`, d.id, title); err != nil {
				return 0, 0, nil, fmt.Errorf("blank the title: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE document_record_parties SET name = $2, org_number = NULL, address = NULL WHERE id = ANY($1)`,
			d.parties, ErasedName); err != nil {
			return 0, 0, nil, fmt.Errorf("blank the person on the document: %w", err)
		}
	}
	return blanked, kept, slices.Compact(slices.Sorted(slices.Values(scans))), nil
}

// withoutNames is a title with each of names taken out of it, and each part
// of them two letters or longer -- "Köpeavtal – Svensson" names Anna
// Svensson as surely as her whole name does -- in any case. Taking out a
// word that only looked like a name is the safer mistake. Too long, or
// nothing left, it is ErasedName.
func withoutNames(title string, names []string) string {
	var words []string
	for _, n := range names {
		if n == ErasedName {
			continue
		}
		words = append(words, strings.TrimSpace(n))
		for _, w := range strings.Fields(n) {
			if utf8.RuneCountInString(w) >= 2 {
				words = append(words, w)
			}
		}
	}
	if len(words) == 0 {
		return title
	}
	// Longest first, so a whole name goes as one rather than word by word.
	slices.SortFunc(words, func(a, b string) int { return utf8.RuneCountInString(b) - utf8.RuneCountInString(a) })
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = regexp.QuoteMeta(w)
	}
	out := regexp.MustCompile(`(?i)(`+strings.Join(quoted, "|")+`)`).ReplaceAllString(title, ErasedName)
	if out = strings.TrimSpace(out); out == "" || utf8.RuneCountInString(out) > 200 {
		return ErasedName
	}
	return out
}
