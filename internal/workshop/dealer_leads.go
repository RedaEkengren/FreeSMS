package workshop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// Leads (#172): every enquiry about a car becomes a lead with the car or
// what the person wants, a salesperson and a next step, so nobody's
// follow-up lives only in their head. Where a lead stands, who has it and
// what is next are its latest events (lead_events); each contact with the
// person is a row (lead_contacts). A salesperson reads their own leads and
// the ones nobody has; whoever runs the shop reads them all -- in the
// query, not on the page.

// Where a lead stands.
const (
	LeadNew             = "new"
	LeadContacted       = "contacted"
	LeadTestDriveBooked = "test_drive_booked"
	LeadOfferMade       = "offer_made"
	LeadWon             = "won"
	LeadLost            = "lost"
)

// LeadStatuses is every status, in the order a lead goes through them.
var LeadStatuses = []string{LeadNew, LeadContacted, LeadTestDriveBooked, LeadOfferMade, LeadWon, LeadLost}

// LeadChannels is every way an enquiry arrives.
var LeadChannels = []string{"walk_in", "phone", "ad_form", "website", "email", "other"}

// LeadLostReasons is the fixed list a lost lead says why from; "other"
// says it in the note.
var LeadLostReasons = []string{"bought_elsewhere", "price", "financing", "car_sold", "trade_in_value", "no_response",
	"changed_mind", "other"}

// LeadContactHows is every way a person was reached, or reached the shop.
var LeadContactHows = []string{"phone", "sms", "email", "in_person", "other"}

// defaultLeadErasureMonths is how long a lead that never became a deal
// keeps the person's details until the shop says otherwise.
const defaultLeadErasureMonths = 12

var leadLabels = map[string]string{
	LeadNew: "New", LeadContacted: "Contacted", LeadTestDriveBooked: "Test drive booked", LeadOfferMade: "Offer made",
	LeadWon: "Won", LeadLost: "Lost",
	"walk_in": "Walk-in", "phone": "Phone", "ad_form": "Ad form", "website": "Website", "email": "Email", "other": "Other",
	"bought_elsewhere": "Bought elsewhere", "price": "Price", "financing": "Financing", "car_sold": "The car was sold",
	"trade_in_value": "Trade-in value", "no_response": "No response", "changed_mind": "Changed their mind",
	"sms": "Text message", "in_person": "In person",
}

// LeadLabel is a status, channel, lost reason or way of contact in words,
// as a catalogue key.
func LeadLabel(key string) string { return leadLabels[key] }

// Lead is one lead, as a salesperson reads it.
type Lead struct {
	ID          string
	PartyID     string
	PartyName   string
	Name        string
	Phone       string
	Email       string
	StockUnitID string
	// The car's plate, or its make and model, when the lead is about one.
	Car         string
	CarStatus   StockStatus
	Wishes      string
	Channel     string
	Status      string
	LostReason  string
	LostNote    string
	Salesperson string
	// The salesperson's name, for the page.
	SalespersonName string
	NextStepOn      *time.Time
	NextStep        string
	CreatedAt       time.Time
	LastActivityAt  time.Time
	Erased          bool
	Contacts        []LeadContact
	// The deal made from it (#175) that stands: the newest not cancelled.
	DealID string
}

// Open reports whether the lead is still being worked.
func (l Lead) Open() bool { return l.Status != LeadWon && l.Status != LeadLost }

// CarGone reports whether the car the open lead is about has been reserved
// or sold to someone, so the salesperson can offer another.
func (l Lead) CarGone() bool {
	if !l.Open() || l.StockUnitID == "" {
		return false
	}
	switch l.CarStatus {
	case StockReserved, StockSold, StockDelivered, StockClosed, StockWrittenOff:
		return true
	}
	return false
}

// Contact is who the lead is from, as the page names them.
func (l Lead) Contact() string {
	if l.PartyName != "" {
		return l.PartyName
	}
	return l.Name
}

// LeadContact is one contact with the person.
type LeadContact struct {
	At   time.Time
	By   string
	How  string
	Note string
}

// NewLead is an enquiry to record.
type NewLead struct {
	// A party already known, or a name with a phone number or an email.
	PartyID string
	Name    string
	Phone   string
	Email   string
	// The car, or none and what they want.
	StockUnitID string
	Wishes      string
	Channel     string
	// Who has it: empty for nobody yet.
	Salesperson string
	// What was said, as the first contact.
	Note string
	// The same request again finds the lead it made.
	Key string
	// Recorded beside the open leads from the same person, as another car
	// they ask about, rather than offered to be added to one.
	Alongside bool
}

// DuplicateLeadError is CreateLead's answer when an open lead from the
// same phone number or email exists: the leads the caller may see, and
// how many others hold someone else.
type DuplicateLeadError struct {
	Leads  []Lead
	Hidden int
}

func (e *DuplicateLeadError) Error() string {
	return fmt.Sprintf("workshop: %d open lead(s) from the same person", len(e.Leads)+e.Hidden)
}

// Is makes it ErrInvalid to whoever does not ask for it by name.
func (e *DuplicateLeadError) Is(target error) bool { return target == ErrInvalid }

var (
	errLeadContact  = fmt.Errorf("%w: say who it is: a name with a phone number or an email", ErrInvalid)
	errLeadCar      = fmt.Errorf("%w: say which car, or what they are looking for", ErrInvalid)
	errLeadChannel  = fmt.Errorf("%w: say how the enquiry came in", ErrInvalid)
	errLeadText     = fmt.Errorf("%w: a name is at most 200 characters, a phone number 40, an email 200, wishes 1000 and a note 500", ErrInvalid)
	errLeadPhone    = fmt.Errorf("%w: that phone number has too few or too many digits", ErrInvalid)
	errLeadEmail    = fmt.Errorf("%w: that is not an email address", ErrInvalid)
	errLeadStatus   = fmt.Errorf("%w: that is not where a lead can be moved", ErrInvalid)
	errLeadWon      = fmt.Errorf("%w: a lead is won by the deal made from it", ErrInvalid)
	errLeadLost     = fmt.Errorf("%w: say why the lead was lost", ErrInvalid)
	errLeadClosed   = fmt.Errorf("%w: that lead is closed; take it up again first", ErrInvalid)
	errLeadSeller   = fmt.Errorf("%w: that person does not sell cars here", ErrInvalid)
	errLeadKey      = fmt.Errorf("%w: that request was already used for another enquiry", ErrInvalid)
	errLeadHow      = fmt.Errorf("%w: say how they were reached", ErrInvalid)
	errLeadErased   = fmt.Errorf("%w: that lead's details have been erased", ErrInvalid)
	errLeadMonths   = fmt.Errorf("%w: keep a lead's details from one month to ten years", ErrInvalid)
	errLeadNextStep = fmt.Errorf("%w: a next step is a day, and what to do in 500 characters or fewer", ErrInvalid)
)

// leadPhoneDigits is a phone number's digits as a second enquiry from the
// same person is recognised: a Swedish number written with +46 or 0046 is
// the same number as with 0.
func leadPhoneDigits(raw string) string {
	raw = strings.TrimSpace(raw)
	var b strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	d := b.String()
	switch {
	case strings.HasPrefix(raw, "+46"):
		d = "0" + strings.TrimPrefix(d, "46")
	case strings.HasPrefix(d, "0046"):
		d = "0" + strings.TrimPrefix(d, "0046")
	}
	return d
}

// leadSeer is the user whose leads the caller may see, or empty for all:
// whoever runs the shop sees every lead, a salesperson their own and the
// ones nobody has.
func leadSeer(scope access.Scope) string {
	if scope.Role.RunsTheShop() {
		return ""
	}
	return scope.UserID
}

// leadVisible is the condition a lead's current state n meets for the
// caller, whose leadSeer is the parameter at position p.
func leadVisible(p int) string {
	return fmt.Sprintf(`($%d = '' OR n.salesperson_id IS NULL OR n.salesperson_id::text = $%d)`, p, p)
}

const leadColumns = `
	SELECT l.id, coalesce(l.party_id::text, ''), coalesce(pn.name, ''), coalesce(l.contact_name, ''),
	       coalesce(l.contact_phone, ''), coalesce(l.contact_email, ''), coalesce(l.stock_unit_id::text, ''),
	       ` + carNameSQL + `, coalesce(u.status, ''),
	       coalesce(l.wishes, ''), l.channel, coalesce(n.status, 'new'), coalesce(n.lost_reason, ''), coalesce(n.lost_note, ''),
	       coalesce(n.salesperson_id::text, ''), coalesce(sp.display_name, ''), n.next_step_on, coalesce(n.next_step, ''),
	       l.created_at, n.last_activity_at, l.erased_at IS NOT NULL,
	       coalesce((SELECT s.id::text FROM vehicle_sales s WHERE s.lead_id = l.id
	                 AND NOT EXISTS (SELECT 1 FROM vehicle_sale_events e WHERE e.sale_id = s.id AND e.kind = 'cancelled')
	                 ORDER BY s.created_at DESC LIMIT 1), '')
	FROM leads l
	JOIN lead_now n ON n.lead_id = l.id
	LEFT JOIN stock_units u ON u.id = l.stock_unit_id
	LEFT JOIN vehicles v ON v.id = u.vehicle_id
	LEFT JOIN vehicle_registrations reg ON reg.vehicle_id = v.id AND reg.valid_to IS NULL
	LEFT JOIN users su ON su.id = n.salesperson_id LEFT JOIN people sp ON sp.id = su.person_id
	LEFT JOIN LATERAL (
		SELECT coalesce(CASE WHEN pa.kind = 'company' THEN coalesce(c.company_name, pa.company_name) END, cp.display_name, pp.display_name) AS name
		FROM parties pa LEFT JOIN customers c ON c.id = pa.customer_id
		LEFT JOIN people cp ON cp.id = c.person_id LEFT JOIN people pp ON pp.id = pa.person_id
		WHERE pa.id = l.party_id) pn ON true`

func scanLead(row pgx.Row) (Lead, error) {
	var l Lead
	var status string
	err := row.Scan(&l.ID, &l.PartyID, &l.PartyName, &l.Name, &l.Phone, &l.Email, &l.StockUnitID, &l.Car, &status,
		&l.Wishes, &l.Channel, &l.Status, &l.LostReason, &l.LostNote, &l.Salesperson, &l.SalespersonName, &l.NextStepOn,
		&l.NextStep, &l.CreatedAt, &l.LastActivityAt, &l.Erased, &l.DealID)
	l.CarStatus = StockStatus(status)
	return l, err
}

// leadTx reads a lead the caller may see, with its contacts.
func leadTx(ctx context.Context, tx pgx.Tx, scope access.Scope, id string) (Lead, error) {
	l, err := scanLead(tx.QueryRow(ctx, leadColumns+` WHERE l.id = $1 AND `+leadVisible(2), id, leadSeer(scope)))
	if errors.Is(err, pgx.ErrNoRows) {
		return Lead{}, ErrNotFound
	}
	if err != nil {
		return Lead{}, fmt.Errorf("read lead: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT c.at, coalesce(p.display_name, ''), c.how, coalesce(c.note, '')
		FROM lead_contacts c LEFT JOIN users u ON u.id = c.by_user LEFT JOIN people p ON p.id = u.person_id
		WHERE c.lead_id = $1 ORDER BY c.at, c.id`, id)
	if err != nil {
		return Lead{}, fmt.Errorf("read the lead's contacts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c LeadContact
		if err := rows.Scan(&c.At, &c.By, &c.How, &c.Note); err != nil {
			return Lead{}, err
		}
		l.Contacts = append(l.Contacts, c)
	}
	return l, rows.Err()
}

// sellerTx checks that a user sells cars here.
func sellerTx(ctx context.Context, tx pgx.Tx, userID string) error {
	if !looksLikeUUID(userID) {
		return errLeadSeller
	}
	var ok bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM users u WHERE u.id = $1 AND u.active
		               AND (u.role IN ('owner', 'admin', 'sales')
		                    OR EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = u.id AND r.role IN ('owner', 'admin', 'sales'))))`,
		userID).Scan(&ok); err != nil {
		return fmt.Errorf("read the salesperson: %w", err)
	}
	if !ok {
		return errLeadSeller
	}
	return nil
}

func leadEventTx(ctx context.Context, tx pgx.Tx, scope access.Scope, id, kind string, cols map[string]any) error {
	var status, reason, note, seller any
	var on any
	status, reason, note, seller, on = cols["status"], cols["lost_reason"], cols["note"], cols["salesperson_id"], cols["next_step_on"]
	_, err := tx.Exec(ctx, `
		INSERT INTO lead_events (shop_id, lead_id, kind, status, lost_reason, note, salesperson_id, next_step_on, by_user)
		VALUES ($1, $2, $3, $4, $5, $6, $7::uuid, $8::date, NULLIF($9, '')::uuid)`,
		scope.ShopID, id, kind, status, reason, note, seller, on, scope.UserID)
	if err != nil {
		return fmt.Errorf("record what happened to the lead: %w", err)
	}
	return nil
}

func nonEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// CreateLead records an enquiry, and its first contact when there is a
// note. An open lead from the same phone number or email is offered rather
// than a second made (*DuplicateLeadError), unless the enquiry is about
// another car and says so (Alongside); then it shares the person's party.
// A salesperson may take a lead for themselves or leave it for anyone;
// whoever runs the shop gives it to any salesperson. For whoever sells
// cars, in a shop that sells them.
func CreateLead(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, n NewLead) (Lead, error) {
	if !scope.Role.SellsVehicles() {
		return Lead{}, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return Lead{}, err
	}
	n.Name, n.Phone, n.Email = strings.TrimSpace(n.Name), strings.TrimSpace(n.Phone), strings.ToLower(strings.TrimSpace(n.Email))
	n.Wishes, n.Note, n.Key = strings.TrimSpace(n.Wishes), strings.TrimSpace(n.Note), strings.TrimSpace(n.Key)
	if (n.PartyID != "" && !looksLikeUUID(n.PartyID)) || (n.StockUnitID != "" && !looksLikeUUID(n.StockUnitID)) {
		return Lead{}, ErrNotFound
	}
	if !slices.Contains(LeadChannels, n.Channel) {
		return Lead{}, errLeadChannel
	}
	if utf8.RuneCountInString(n.Name) > 200 || utf8.RuneCountInString(n.Phone) > 40 || utf8.RuneCountInString(n.Email) > 200 ||
		utf8.RuneCountInString(n.Wishes) > 1000 || utf8.RuneCountInString(n.Note) > 1000 || len(n.Key) > 100 {
		return Lead{}, errLeadText
	}
	digits := ""
	if n.Phone != "" {
		if digits = leadPhoneDigits(n.Phone); len(digits) < 5 || len(digits) > 20 {
			return Lead{}, errLeadPhone
		}
	}
	if n.Email != "" && (!strings.Contains(n.Email, "@") || strings.ContainsAny(n.Email, " \t")) {
		return Lead{}, errLeadEmail
	}
	if n.PartyID == "" && (n.Name == "" || (digits == "" && n.Email == "")) {
		return Lead{}, errLeadContact
	}
	if n.StockUnitID == "" && n.Wishes == "" {
		return Lead{}, errLeadCar
	}
	if n.Salesperson != "" && n.Salesperson != scope.UserID && !scope.Role.RunsTheShop() {
		return Lead{}, access.ErrForbidden
	}
	var out Lead
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if n.Key != "" {
			// The same enquiry sent again: the lead it made, whatever it
			// says now. The key is locked first, so a sending still in
			// flight has committed by the time it is looked for.
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('leadkey:' || $1, 0))`, n.Key); err != nil {
				return err
			}
			var id, unit string
			err := tx.QueryRow(ctx, `SELECT id, coalesce(stock_unit_id::text, '') FROM leads WHERE request_key = $1`, n.Key).Scan(&id, &unit)
			if err == nil {
				if unit != n.StockUnitID {
					return errLeadKey
				}
				out, err = leadTx(ctx, tx, scope, id)
				return err
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("read the request: %w", err)
			}
		}
		if n.PartyID != "" {
			if err := partySeenTx(ctx, tx, scope, n.PartyID); err != nil {
				return err
			}
		}
		if n.StockUnitID != "" {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM stock_units WHERE id = $1)`, n.StockUnitID).Scan(&exists); err != nil {
				return fmt.Errorf("read the car: %w", err)
			}
			if !exists {
				return ErrNotFound
			}
		}
		if n.Salesperson != "" {
			if err := sellerTx(ctx, tx, n.Salesperson); err != nil {
				return err
			}
		}
		// Two enquiries from the same person at the same moment are one
		// question: the second waits and finds the first.
		for _, k := range []string{digits, n.Email, n.PartyID} {
			if k != "" {
				if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('lead:' || $1, 0))`, k); err != nil {
					return err
				}
			}
		}
		party := n.PartyID
		if digits != "" || n.Email != "" || n.PartyID != "" {
			rows, err := tx.Query(ctx, leadColumns+`
				WHERE l.erased_at IS NULL AND coalesce(n.status, 'new') NOT IN ('won', 'lost')
				  AND (($1 <> '' AND l.contact_phone_digits = $1) OR ($2 <> '' AND l.contact_email = $2)
				       OR ($3 <> '' AND l.party_id::text = $3))
				ORDER BY l.created_at`, digits, n.Email, n.PartyID)
			if err != nil {
				return fmt.Errorf("look for the same person's leads: %w", err)
			}
			var same []Lead
			for rows.Next() {
				l, err := scanLead(rows)
				if err != nil {
					rows.Close()
					return err
				}
				same = append(same, l)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			if len(same) > 0 && !n.Alongside {
				dup := &DuplicateLeadError{}
				seer := leadSeer(scope)
				for _, l := range same {
					if seer == "" || l.Salesperson == "" || l.Salesperson == seer {
						dup.Leads = append(dup.Leads, l)
					} else {
						dup.Hidden++
					}
				}
				return dup
			}
			// Another car asked about by the same person: the same person.
			for _, l := range same {
				if party == "" && l.PartyID != "" {
					party = l.PartyID
				}
			}
		}
		var id string
		if err := tx.QueryRow(ctx, `
			INSERT INTO leads (shop_id, party_id, contact_name, contact_phone, contact_phone_digits, contact_email, stock_unit_id,
			                   wishes, channel, request_key, created_by)
			VALUES ($1, CASE WHEN $2 <> '' THEN party_now($2::uuid) END, nullif($3, ''), nullif($4, ''), nullif($5, ''), nullif($6, ''),
			        NULLIF($7, '')::uuid, nullif($8, ''), $9, nullif($10, ''), NULLIF($11, '')::uuid)
			RETURNING id`,
			scope.ShopID, party, n.Name, n.Phone, digits, n.Email, n.StockUnitID, n.Wishes, n.Channel, n.Key, scope.UserID).Scan(&id); err != nil {
			return fmt.Errorf("record the lead: %w", err)
		}
		if err := leadEventTx(ctx, tx, scope, id, "status", map[string]any{"status": LeadNew}); err != nil {
			return err
		}
		if n.Salesperson != "" {
			if err := leadEventTx(ctx, tx, scope, id, "assigned", map[string]any{"salesperson_id": n.Salesperson}); err != nil {
				return err
			}
		}
		if n.Note != "" {
			how := map[string]string{"walk_in": "in_person", "phone": "phone", "email": "email"}[n.Channel]
			if how == "" {
				how = "other"
			}
			if _, err := tx.Exec(ctx, `INSERT INTO lead_contacts (shop_id, lead_id, by_user, how, note) VALUES ($1, $2, NULLIF($3, '')::uuid, $4, $5)`,
				scope.ShopID, id, scope.UserID, how, n.Note); err != nil {
				return fmt.Errorf("record what was said: %w", err)
			}
		}
		var err error
		out, err = leadTx(ctx, tx, scope, id)
		return err
	})
	return out, err
}

// LeadByID reads a lead the caller may see: their own, one nobody has, or
// any for whoever runs the shop. Reading is Core's: a shop that stops
// selling cars keeps what it was asked.
func LeadByID(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string) (Lead, error) {
	if !scope.Role.SellsVehicles() {
		return Lead{}, access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return Lead{}, ErrNotFound
	}
	var out Lead
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = leadTx(ctx, tx, scope, id)
		return err
	})
	return out, err
}

// Leads lists the leads the caller may see, open ones first by their next
// step, then the closed by when they last moved. Closed ones only with
// closed. Reading is Core's.
func Leads(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, closed bool) ([]Lead, error) {
	if !scope.Role.SellsVehicles() {
		return nil, access.ErrForbidden
	}
	var out []Lead
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, leadColumns+`
			WHERE `+leadVisible(1)+` AND ($2 OR coalesce(n.status, 'new') NOT IN ('won', 'lost'))
			ORDER BY coalesce(n.status, 'new') IN ('won', 'lost'), n.next_step_on NULLS LAST, n.last_activity_at DESC
			LIMIT 500`, leadSeer(scope), closed)
		if err != nil {
			return fmt.Errorf("list leads: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			l, err := scanLead(rows)
			if err != nil {
				return err
			}
			out = append(out, l)
		}
		return rows.Err()
	})
	return out, err
}

// workLeadTx reads a lead to change, locked, as the caller may see it: a
// lead whose details are erased takes nothing more.
func workLeadTx(ctx context.Context, tx pgx.Tx, scope access.Scope, id string) (Lead, error) {
	if !looksLikeUUID(id) {
		return Lead{}, ErrNotFound
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('lead:' || $1::uuid::text, 0))`, id); err != nil {
		return Lead{}, err
	}
	l, err := leadTx(ctx, tx, scope, id)
	if err != nil {
		return Lead{}, err
	}
	if l.Erased {
		return Lead{}, errLeadErased
	}
	return l, nil
}

// RecordLeadContact records that the person was reached, or reached the
// shop, how and what was said. A new lead is contacted from then on.
func RecordLeadContact(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id, how, note string) error {
	if !scope.Role.SellsVehicles() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return err
	}
	if !slices.Contains(LeadContactHows, how) {
		return errLeadHow
	}
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > 1000 {
		return errLeadText
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		l, err := workLeadTx(ctx, tx, scope, id)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO lead_contacts (shop_id, lead_id, by_user, how, note) VALUES ($1, $2, NULLIF($3, '')::uuid, $4, nullif($5, ''))`,
			scope.ShopID, l.ID, scope.UserID, how, note); err != nil {
			return fmt.Errorf("record the contact: %w", err)
		}
		if l.Status == LeadNew {
			return leadEventTx(ctx, tx, scope, l.ID, "status", map[string]any{"status": LeadContacted})
		}
		return nil
	})
}

// MoveLead moves a lead to where it stands now. Lost says why, from the
// fixed list, with a note when it is "other"; a lost lead may be taken up
// again. Won is the deal's to say (#175), never a tap. Moving to where it
// already is does nothing.
func MoveLead(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id, to, reason, note string) error {
	if !scope.Role.SellsVehicles() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return err
	}
	if !slices.Contains(LeadStatuses, to) {
		return errLeadStatus
	}
	if to == LeadWon {
		return errLeadWon
	}
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > 500 {
		return errLeadText
	}
	if to == LeadLost && (!slices.Contains(LeadLostReasons, reason) || (reason == "other" && note == "")) {
		return errLeadLost
	}
	if to != LeadLost {
		reason, note = "", ""
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		l, err := workLeadTx(ctx, tx, scope, id)
		if err != nil {
			return err
		}
		if l.Status == to {
			return nil
		}
		if l.Status == LeadWon {
			return errLeadClosed
		}
		if to == LeadNew {
			return errLeadStatus
		}
		return leadEventTx(ctx, tx, scope, l.ID, "status", map[string]any{"status": to, "lost_reason": nonEmpty(reason), "note": nonEmpty(note)})
	})
}

// AssignLead gives a lead to a salesperson, or to nobody with an empty
// one. A salesperson may take a lead nobody has, or give up their own;
// whoever runs the shop gives any lead to anyone who sells cars.
func AssignLead(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id, to string) error {
	if !scope.Role.SellsVehicles() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return err
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		l, err := workLeadTx(ctx, tx, scope, id)
		if err != nil {
			return err
		}
		if l.Salesperson == to {
			return nil
		}
		if !scope.Role.RunsTheShop() && to != "" && to != scope.UserID {
			return access.ErrForbidden
		}
		if to != "" {
			if err := sellerTx(ctx, tx, to); err != nil {
				return err
			}
		}
		return leadEventTx(ctx, tx, scope, l.ID, "assigned", map[string]any{"salesperson_id": nonEmpty(to)})
	})
}

// SetLeadNextStep sets the day of the lead's next step and what it is, or
// clears it with a nil day. From that day the lead is waiting for its
// salesperson (dealerWaiting).
func SetLeadNextStep(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string, on *time.Time, what string) error {
	if !scope.Role.SellsVehicles() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return err
	}
	what = strings.TrimSpace(what)
	if utf8.RuneCountInString(what) > 500 || (on == nil && what != "") {
		return errLeadNextStep
	}
	var day any
	if on != nil {
		day = on.Format(time.DateOnly)
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		l, err := workLeadTx(ctx, tx, scope, id)
		if err != nil {
			return err
		}
		if !l.Open() {
			return errLeadClosed
		}
		return leadEventTx(ctx, tx, scope, l.ID, "next_step", map[string]any{"next_step_on": day, "note": nonEmpty(what)})
	})
}

// ReassignLeads gives every open lead one salesperson has to another, or
// to nobody: for when somebody leaves. It returns how many moved. For
// whoever runs the shop.
func ReassignLeads(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, from, to string) (int, error) {
	if !scope.Role.RunsTheShop() {
		return 0, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return 0, err
	}
	if !looksLikeUUID(from) || (to != "" && !looksLikeUUID(to)) {
		return 0, ErrNotFound
	}
	var moved int
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if to != "" {
			if err := sellerTx(ctx, tx, to); err != nil {
				return err
			}
		}
		rows, err := tx.Query(ctx, `
			SELECT l.id FROM leads l JOIN lead_now n ON n.lead_id = l.id
			WHERE n.salesperson_id = $1 AND coalesce(n.status, 'new') NOT IN ('won', 'lost') AND l.erased_at IS NULL
			ORDER BY l.id`, from)
		if err != nil {
			return fmt.Errorf("read the leads: %w", err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('lead:' || $1::uuid::text, 0))`, id); err != nil {
				return err
			}
			if err := leadEventTx(ctx, tx, scope, id, "assigned", map[string]any{"salesperson_id": nonEmpty(to)}); err != nil {
				return err
			}
		}
		moved = len(ids)
		return nil
	})
	return moved, err
}

// SellersOfCars is everyone who may be given a lead, for a salesperson's
// form: name and id. For whoever sells cars.
func SellersOfCars(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]StaffMember, error) {
	if !scope.Role.SellsVehicles() {
		return nil, access.ErrForbidden
	}
	var out []StaffMember
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT u.id, p.display_name FROM users u JOIN people p ON p.id = u.person_id
			WHERE u.active AND (u.role IN ('owner', 'admin', 'sales')
			      OR EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = u.id AND r.role IN ('owner', 'admin', 'sales')))
			ORDER BY p.display_name`)
		if err != nil {
			return fmt.Errorf("list salespeople: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var m StaffMember
			if err := rows.Scan(&m.UserID, &m.Name); err != nil {
				return err
			}
			m.You = m.UserID == scope.UserID
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

// SetLeadErasure sets how many months a lead that never became a deal
// keeps the person's details after it last moved. For whoever runs the
// shop.
func SetLeadErasure(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, months int) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return err
	}
	if months < 1 || months > 120 {
		return errLeadMonths
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO lead_settings (shop_id, erase_after_months, changed_by) VALUES ($1, $2, NULLIF($3, '')::uuid)`,
			scope.ShopID, months, scope.UserID)
		return err
	})
}

// LeadErasureMonths is how long a lead that never became a deal keeps the
// person's details. For whoever sells cars. Reading is Core's.
func LeadErasureMonths(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) (int, error) {
	if !scope.Role.SellsVehicles() {
		return 0, access.ErrForbidden
	}
	var months int
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT coalesce((SELECT erase_after_months FROM lead_settings ORDER BY seq DESC LIMIT 1), $1)`,
			defaultLeadErasureMonths).Scan(&months)
	})
	return months, err
}

// sweepLeadsTx erases the person from every lead that never became a deal
// and has not moved for the shop's period: the name, the phone, the email,
// the party, the wishes and what was written about them. Where the lead
// came from, which car, who had it and how it ended stay, so the figures
// still count it. Run by the retention sweep.
func sweepLeadsTx(ctx context.Context, tx pgx.Tx) (int64, error) {
	var months int
	if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT erase_after_months FROM lead_settings ORDER BY seq DESC LIMIT 1), $1)`,
		defaultLeadErasureMonths).Scan(&months); err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `
		SELECT l.id FROM leads l JOIN lead_now n ON n.lead_id = l.id
		WHERE l.erased_at IS NULL AND coalesce(n.status, 'new') <> 'won'
		  AND n.last_activity_at < clock_timestamp() - make_interval(months => $1)`, months)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	if err := eraseLeadsTx(ctx, tx, ids); err != nil {
		return 0, err
	}
	return int64(len(ids)), nil
}

// dealerWaiting is car sales' part of the action queue: a lead whose next
// step is due, for its salesperson; one whose car has been reserved or
// sold, for its salesperson, to offer another; and a new lead nobody has,
// for everyone who sells cars. Never who is asking: the car, or that it is
// any car.
func dealerWaiting(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]ActionItem, error) {
	if !scope.Role.SellsVehicles() {
		return nil, nil
	}
	var out []ActionItem
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		// In one statement with a test drive out past its end (#173), for
		// everyone who sells cars: the car is the shop's whoever booked it.
		rows, err := tx.Query(ctx, `
			SELECT CASE WHEN n.salesperson_id IS NULL THEN 'lead_unassigned'
			            WHEN u.status IN ('reserved', 'sold', 'delivered', 'closed', 'written_off') THEN 'lead_car_gone'
			            ELSE 'lead_follow_up' END,
			       'lead', l.id::text, `+carNameSQL+`,
			       coalesce(n.next_step_on::timestamp AT TIME ZONE (SELECT timezone FROM shops), l.created_at), l.created_at
			FROM leads l
			JOIN lead_now n ON n.lead_id = l.id
			LEFT JOIN stock_units u ON u.id = l.stock_unit_id
			LEFT JOIN vehicles v ON v.id = u.vehicle_id
			LEFT JOIN vehicle_registrations reg ON reg.vehicle_id = v.id AND reg.valid_to IS NULL
			WHERE l.erased_at IS NULL AND coalesce(n.status, 'new') NOT IN ('won', 'lost')
			  AND ((n.salesperson_id IS NULL AND coalesce(n.status, 'new') = 'new')
			       OR (n.salesperson_id::text = $1
			           AND (u.status IN ('reserved', 'sold', 'delivered', 'closed', 'written_off')
			                OR n.next_step_on <= (clock_timestamp() AT TIME ZONE (SELECT timezone FROM shops))::date)))
			UNION ALL
			`+testDrivesOverdue+`
			ORDER BY 5, 6`, scope.UserID)
		if err != nil {
			return fmt.Errorf("read the leads waiting: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			it := ActionItem{Module: access.Dealer, Urgency: UrgencySoon}
			var at time.Time
			if err := rows.Scan(&it.Kind, &it.SubjectKind, &it.SubjectID, &it.Text, &it.Due, &at); err != nil {
				return err
			}
			if it.SubjectKind == "test_drive" {
				it.Urgency = UrgencyNow
			} else if it.Text == "" && it.Kind != "lead_car_gone" {
				// What they want is theirs to say, and not a car's name.
				it.Kind += "_any"
			}
			out = append(out, it)
		}
		return rows.Err()
	})
	return out, err
}

// StockCar is a car in stock as a salesperson picks it: its unit, its
// plate or make and model, and where its deal stands. Never what it cost.
type StockCar struct {
	UnitID string
	Car    string
	Status StockStatus
}

// StatusLabel is where the car's deal stands, as a catalogue key.
func (c StockCar) StatusLabel() string { return c.Status.Label() }

// CarsInStock is every car whose cycle is open, for whoever sells cars to
// tie a lead or a deal to. Reading is Core's.
func CarsInStock(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]StockCar, error) {
	if !scope.Role.SellsVehicles() && !scope.Role.BuysVehicles() {
		return nil, access.ErrForbidden
	}
	var out []StockCar
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT u.id, `+carNameSQL+`, u.status
			FROM stock_units u JOIN vehicles v ON v.id = u.vehicle_id
			LEFT JOIN vehicle_registrations reg ON reg.vehicle_id = v.id AND reg.valid_to IS NULL
			WHERE u.closed_at IS NULL
			ORDER BY 2`)
		if err != nil {
			return fmt.Errorf("list cars in stock: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var c StockCar
			var status string
			if err := rows.Scan(&c.UnitID, &c.Car, &status); err != nil {
				return err
			}
			c.Status = StockStatus(status)
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

// theirLeadsAndDrivesTx finds a person's leads and the test drives they
// took: by their party -- on its own or as a customer, and any merged into
// one -- and, for a lead entered with only a name, by the phone number or
// email they gave, as a second enquiry from them would be recognised. A
// lead that names a party is that party's only: two people who share a
// family phone do not get each other's leads.
func theirLeadsAndDrivesTx(ctx context.Context, tx pgx.Tx, personID string) (leads, drives []string, err error) {
	var phone, email string
	if err := tx.QueryRow(ctx, `SELECT coalesce(phone, ''), lower(coalesce(email::text, '')) FROM people WHERE id = $1`, personID).
		Scan(&phone, &email); err != nil {
		return nil, nil, fmt.Errorf("read person: %w", err)
	}
	digits := ""
	if phone != "" {
		digits = leadPhoneDigits(phone)
	}
	const theirs = `
		WITH theirs AS (
		  SELECT pa.id FROM parties pa LEFT JOIN customers c ON c.id = pa.customer_id
		  WHERE pa.person_id = $1 OR c.person_id = $1)`
	rows, err := tx.Query(ctx, theirs+`
		SELECT l.id FROM leads l
		WHERE l.party_id IN (SELECT id FROM theirs) OR party_now(l.party_id) IN (SELECT id FROM theirs)
		   OR (l.party_id IS NULL AND (($2 <> '' AND l.contact_phone_digits = $2) OR ($3 <> '' AND l.contact_email = $3)))
		ORDER BY l.created_at`, personID, digits, email)
	if err != nil {
		return nil, nil, fmt.Errorf("read their leads: %w", err)
	}
	if leads, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return nil, nil, err
	}
	rows, err = tx.Query(ctx, theirs+`
		SELECT d.id FROM test_drives d
		WHERE d.driver_party_id IN (SELECT id FROM theirs) OR party_now(d.driver_party_id) IN (SELECT id FROM theirs)
		ORDER BY d.starts_at`, personID)
	if err != nil {
		return nil, nil, fmt.Errorf("read their test drives: %w", err)
	}
	drives, err = pgx.CollectRows(rows, pgx.RowTo[string])
	return leads, drives, err
}

// exportLeadsAndDrivesTx adds a person's leads and test drives to what
// they are given (ExportPerson): one statement for the leads and one for
// the drives, however many there are, dated on the shop's own day.
func exportLeadsAndDrivesTx(ctx context.Context, tx pgx.Tx, personID string, e *PersonExport) error {
	leads, drives, err := theirLeadsAndDrivesTx(ctx, tx, personID)
	if err != nil {
		return err
	}
	if len(leads) > 0 {
		rows, err := tx.Query(ctx, `
			SELECT l.created_at, l.channel, `+carNameSQL+`,
			       coalesce(l.wishes, ''), coalesce(l.contact_name, ''), coalesce(l.contact_phone, ''), coalesce(l.contact_email, ''),
			       coalesce(n.status, 'new'),
			       ARRAY(SELECT to_char(c.at AT TIME ZONE (SELECT timezone FROM shops), 'YYYY-MM-DD') || ' ' || c.how || coalesce(': ' || c.note, '')
			             FROM lead_contacts c WHERE c.lead_id = l.id ORDER BY c.at, c.id)
			FROM leads l JOIN lead_now n ON n.lead_id = l.id
			LEFT JOIN stock_units u ON u.id = l.stock_unit_id LEFT JOIN vehicles v ON v.id = u.vehicle_id
			LEFT JOIN vehicle_registrations reg ON reg.vehicle_id = v.id AND reg.valid_to IS NULL
			WHERE l.id = ANY($1::uuid[]) ORDER BY l.created_at`, leads)
		if err != nil {
			return fmt.Errorf("read leads: %w", err)
		}
		for rows.Next() {
			var l ExportedLead
			if err := rows.Scan(&l.Asked, &l.Channel, &l.Car, &l.Wishes, &l.Name, &l.Phone, &l.Email, &l.Status, &l.Contacts); err != nil {
				rows.Close()
				return err
			}
			e.Leads = append(e.Leads, l)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	if len(drives) > 0 {
		rows, err := tx.Query(ctx, `
			SELECT `+carNameSQL+`, d.starts_at, d.ends_at, coalesce(d.area, ''),
			       coalesce(k.categories, '{}'), k.valid_until, coalesce(k.note, ''),
			       ARRAY(SELECT e.note FROM test_drive_events e WHERE e.test_drive_id = d.id AND e.note IS NOT NULL ORDER BY e.at, e.id)
			FROM test_drives d
			JOIN stock_units u ON u.id = d.stock_unit_id JOIN vehicles v ON v.id = u.vehicle_id
			LEFT JOIN vehicle_registrations reg ON reg.vehicle_id = v.id AND reg.valid_to IS NULL
			LEFT JOIN LATERAL (SELECT c.categories, c.valid_until, c.note FROM test_drive_licence_checks c
			                    WHERE c.test_drive_id = d.id ORDER BY c.checked_at DESC, c.id DESC LIMIT 1) k ON true
			WHERE d.id = ANY($1::uuid[]) ORDER BY d.starts_at`, drives)
		if err != nil {
			return fmt.Errorf("read test drives: %w", err)
		}
		for rows.Next() {
			var x ExportedTestDrive
			if err := rows.Scan(&x.Car, &x.From, &x.To, &x.Area, &x.Categories, &x.ValidUntil, &x.LicenceNote, &x.Notes); err != nil {
				rows.Close()
				return err
			}
			e.TestDrives = append(e.TestDrives, x)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}

// carNameSQL names a car by its plate, its make and model, or its chassis
// number, over vehicles v and their current registration reg.
const carNameSQL = `coalesce(reg.registration, nullif(btrim(concat_ws(' ', v.make, v.model)), ''), nullif(btrim(v.vin), ''), '')`

// eraseLeadsTx clears the person from leads: the name, the phone, the
// email, the party, the wishes and what was written about them. Where the
// lead came from, which car, who had it and how it ended stay. One way,
// for the retention sweep and for a person's erasure alike.
func eraseLeadsTx(ctx context.Context, tx pgx.Tx, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	for _, q := range []string{
		`UPDATE leads SET party_id = NULL, contact_name = NULL, contact_phone = NULL, contact_phone_digits = NULL,
		        contact_email = NULL, wishes = NULL, erased_at = coalesce(erased_at, clock_timestamp()) WHERE id = ANY($1::uuid[])`,
		`UPDATE lead_contacts SET note = NULL WHERE lead_id = ANY($1::uuid[]) AND note IS NOT NULL`,
		`UPDATE lead_events SET note = NULL WHERE lead_id = ANY($1::uuid[]) AND note IS NOT NULL`,
	} {
		if _, err := tx.Exec(ctx, q, ids); err != nil {
			return fmt.Errorf("erase from leads: %w", err)
		}
	}
	return nil
}

// eraseLeadsAndDrivesTx clears a person from their leads (eraseLeadsTx)
// and what was written about them on their test drives; that they drove
// and their licence was checked stay. It returns how many of each.
func eraseLeadsAndDrivesTx(ctx context.Context, tx pgx.Tx, personID string) (leadCount, driveCount int, err error) {
	leads, drives, err := theirLeadsAndDrivesTx(ctx, tx, personID)
	if err != nil {
		return 0, 0, err
	}
	if err := eraseLeadsTx(ctx, tx, leads); err != nil {
		return 0, 0, err
	}
	if len(drives) > 0 {
		for _, q := range []string{
			`UPDATE test_drive_licence_checks SET note = NULL WHERE test_drive_id = ANY($1::uuid[]) AND note IS NOT NULL`,
			`UPDATE test_drive_events SET note = NULL WHERE test_drive_id = ANY($1::uuid[]) AND note IS NOT NULL`,
		} {
			if _, err := tx.Exec(ctx, q, drives); err != nil {
				return 0, 0, fmt.Errorf("erase from test drives: %w", err)
			}
		}
	}
	return len(leads), len(drives), nil
}
