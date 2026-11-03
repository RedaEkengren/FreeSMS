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

// Complaints about a car sold (#181). A complaint belongs to the sale and
// reads its frozen delivery (#179): whether the two years in which a
// defect is presumed to have been there still run, the three in which the
// dealer answers for it, and the two months from noticing a defect within
// which the buyer should complain -- as dates to look at, never a decision
// about liability (G2). What happens to it is rows, append-only.

// The ways a complaint arrives, its statuses and the remedies.
var (
	ComplaintChannels = []string{"in_person", "phone", "email", "letter", "other"}
	ComplaintStatuses = []string{"received", "assessed", "remedy_offered", "remedy_accepted", "remedy_refused", "remedy_done",
		"closed", "referred_arn"}
	ComplaintRemedies = []string{"repair", "replacement", "price_reduction", "rescission"}
)

// ComplaintEvent is one thing that happened to a complaint.
type ComplaintEvent struct {
	Status string
	Remedy string
	Note   string
	At     time.Time
	By     string
}

// Complaint is a complaint as it is read, with what the sale says about
// it.
type Complaint struct {
	ID          string
	Number      int64
	SaleID      string
	SaleNumber  int64
	Car         string
	ReceivedAt  time.Time
	Channel     string
	Description string
	NoticedOn   *time.Time
	Events      []ComplaintEvent
	// From the sale: its delivery, whether the buyer is a consumer, and
	// the defects the buyer accepted one by one (#175).
	Delivery        Delivery
	AcceptedDefects []string
}

// Status is where the complaint stands: its newest event's status.
func (c Complaint) Status() string {
	if len(c.Events) == 0 {
		return ""
	}
	return c.Events[len(c.Events)-1].Status
}

// Open reports whether the complaint is still being dealt with.
func (c Complaint) Open() bool { return c.Status() != "closed" && c.Status() != "referred_arn" }

// WithinPresumption reports whether it was received while a defect is
// presumed to have been there at delivery: a consumer's, within two years.
func (c Complaint) WithinPresumption() bool {
	return c.Delivery.Consumer && !c.ReceivedAt.After(c.Delivery.PresumptionEnds())
}

// WithinLiability reports whether it was received while the dealer still
// answers for defects: a consumer's, within three years.
func (c Complaint) WithinLiability() bool {
	return c.Delivery.Consumer && !c.ReceivedAt.After(c.Delivery.LiabilityEnds())
}

// LateAfterNoticing reports whether the buyer complained more than two
// months after they say they noticed the defect: a reminder for whoever
// assesses it, not a refusal.
func (c Complaint) LateAfterNoticing() bool {
	return c.NoticedOn != nil && c.ReceivedAt.After(c.NoticedOn.AddDate(0, 2, 0))
}

// NewComplaint is a complaint received about a car sold.
type NewComplaint struct {
	ReceivedAt  time.Time
	Channel     string
	Description string
	NoticedOn   *time.Time
	Key         string
}

var (
	errComplaint         = fmt.Errorf("%w: a complaint says when and how it came in and what is wrong, in 2000 characters or fewer", ErrInvalid)
	errComplaintDelivery = fmt.Errorf("%w: a complaint is about a car delivered; this one has not been", ErrInvalid)
	errComplaintStatus   = fmt.Errorf("%w: that is not where a complaint can stand, or a remedy is missing", ErrInvalid)
	errComplaintClosed   = fmt.Errorf("%w: that complaint is closed", ErrInvalid)
	errComplaintKey      = fmt.Errorf("%w: that request was already used for another complaint", ErrInvalid)
)

// OpenComplaint records a complaint received about a delivered car. For
// whoever sells cars. Replayed with the same key, it returns the complaint
// it made.
func OpenComplaint(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, saleID string, n NewComplaint) (Complaint, error) {
	if !scope.Role.SellsVehicles() {
		return Complaint{}, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return Complaint{}, err
	}
	if !looksLikeUUID(saleID) {
		return Complaint{}, ErrNotFound
	}
	n.Description, n.Key = strings.TrimSpace(n.Description), strings.TrimSpace(n.Key)
	if n.ReceivedAt.IsZero() || n.ReceivedAt.After(time.Now().Add(time.Hour)) || !slices.Contains(ComplaintChannels, n.Channel) ||
		n.Description == "" || utf8.RuneCountInString(n.Description) > 2000 || (n.NoticedOn != nil && n.NoticedOn.After(n.ReceivedAt)) {
		return Complaint{}, errComplaint
	}
	var out Complaint
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if n.Key != "" {
			var id, sale string
			err := tx.QueryRow(ctx, `SELECT id, sale_id FROM vehicle_sale_complaints WHERE request_key = $1`, n.Key).Scan(&id, &sale)
			if err == nil {
				if !strings.EqualFold(sale, saleID) {
					return errComplaintKey
				}
				out, err = complaintTx(ctx, tx, scope, id)
				return err
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("read the request: %w", err)
			}
		}
		if _, err := dealTx(ctx, tx, scope, saleID); err != nil {
			return err
		}
		if _, err := deliveryTx(ctx, tx, saleID); errors.Is(err, ErrNotFound) {
			return errComplaintDelivery
		} else if err != nil {
			return err
		}
		var noticed any
		if n.NoticedOn != nil {
			noticed = n.NoticedOn.Format(time.DateOnly)
		}
		var id string
		if err := tx.QueryRow(ctx, `
			INSERT INTO vehicle_sale_complaints (shop_id, sale_id, received_at, channel, description, noticed_on, request_key, created_by)
			VALUES ($1, $2, $3, $4, $5, $6::date, nullif($7, ''), NULLIF($8, '')::uuid) RETURNING id`,
			scope.ShopID, saleID, n.ReceivedAt, n.Channel, n.Description, noticed, n.Key, scope.UserID).Scan(&id); err != nil {
			return fmt.Errorf("record the complaint: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO vehicle_sale_complaint_events (shop_id, complaint_id, status, by_user)
			VALUES ($1, $2, 'received', NULLIF($3, '')::uuid)`, scope.ShopID, id, scope.UserID); err != nil {
			return fmt.Errorf("record the complaint received: %w", err)
		}
		var err error
		out, err = complaintTx(ctx, tx, scope, id)
		return err
	})
	return out, err
}

// MoveComplaint records what has happened to a complaint: assessed, a
// remedy offered, accepted or refused, done, closed, or referred to ARN.
// The remedy is said with every step about one. For whoever sells cars.
func MoveComplaint(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id, status, remedy, note string) error {
	if !scope.Role.SellsVehicles() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return err
	}
	if !looksLikeUUID(id) {
		return ErrNotFound
	}
	note = strings.TrimSpace(note)
	needsRemedy := strings.HasPrefix(status, "remedy_")
	if !slices.Contains(ComplaintStatuses, status) || status == "received" || needsRemedy != (remedy != "") ||
		(remedy != "" && !slices.Contains(ComplaintRemedies, remedy)) || utf8.RuneCountInString(note) > 1000 {
		return errComplaintStatus
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		// Two steps at once are taken one after the other: the sale is
		// the lock, as for everything else on a deal.
		var sale string
		if err := tx.QueryRow(ctx, `SELECT sale_id FROM vehicle_sale_complaints WHERE id = $1`, id).Scan(&sale); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if err := lockDealTx(ctx, tx, sale); err != nil {
			return err
		}
		c, err := complaintTx(ctx, tx, scope, id)
		if err != nil {
			return err
		}
		if !c.Open() {
			return errComplaintClosed
		}
		_, err = tx.Exec(ctx, `INSERT INTO vehicle_sale_complaint_events (shop_id, complaint_id, status, remedy, note, by_user)
			VALUES ($1, $2, $3, nullif($4, ''), nullif($5, ''), NULLIF($6, '')::uuid)`, scope.ShopID, id, status, remedy, note, scope.UserID)
		return err
	})
}

func complaintTx(ctx context.Context, tx pgx.Tx, scope access.Scope, id string) (Complaint, error) {
	var c Complaint
	err := tx.QueryRow(ctx, `
		SELECT c.id, c.number, c.sale_id, c.received_at, c.channel, c.description, c.noticed_on
		FROM vehicle_sale_complaints c WHERE c.id = $1`, id).Scan(&c.ID, &c.Number, &c.SaleID, &c.ReceivedAt, &c.Channel,
		&c.Description, &c.NoticedOn)
	if errors.Is(err, pgx.ErrNoRows) {
		return Complaint{}, ErrNotFound
	}
	if err != nil {
		return Complaint{}, fmt.Errorf("read the complaint: %w", err)
	}
	// The sale as the caller may see it: a salesperson their own deals'.
	d, err := dealTx(ctx, tx, scope, c.SaleID)
	if err != nil {
		return Complaint{}, err
	}
	c.SaleNumber, c.Car = d.Number, d.Car
	for _, f := range d.Defects {
		c.AcceptedDefects = append(c.AcceptedDefects, f.Description)
	}
	if c.Delivery, err = deliveryTx(ctx, tx, c.SaleID); err != nil {
		return Complaint{}, err
	}
	rows, err := tx.Query(ctx, `
		SELECT e.status, coalesce(e.remedy, ''), coalesce(e.note, ''), e.at, coalesce(p.display_name, '')
		FROM vehicle_sale_complaint_events e LEFT JOIN users u ON u.id = e.by_user LEFT JOIN people p ON p.id = u.person_id
		WHERE e.complaint_id = $1 ORDER BY e.seq`, id)
	if err != nil {
		return Complaint{}, fmt.Errorf("read the complaint's events: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var e ComplaintEvent
		if err := rows.Scan(&e.Status, &e.Remedy, &e.Note, &e.At, &e.By); err != nil {
			return Complaint{}, err
		}
		c.Events = append(c.Events, e)
	}
	return c, rows.Err()
}

// ComplaintByID reads a complaint for whoever sells cars, with its sale's
// delivery and accepted defects. Reading is Core's: the dealer answers
// for three years whatever it runs.
func ComplaintByID(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string) (Complaint, error) {
	if !scope.Role.SellsVehicles() {
		return Complaint{}, access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return Complaint{}, ErrNotFound
	}
	var out Complaint
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = complaintTx(ctx, tx, scope, id)
		return err
	})
	return out, err
}

// ComplaintsOfSale is a sale's complaints' ids, newest first. For whoever
// sells cars; Core's.
func ComplaintsOfSale(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, saleID string) ([]Complaint, error) {
	if !scope.Role.SellsVehicles() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(saleID) {
		return nil, ErrNotFound
	}
	var out []Complaint
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM vehicle_sale_complaints WHERE sale_id = $1 ORDER BY received_at DESC, number DESC`, saleID)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		for _, id := range ids {
			c, err := complaintTx(ctx, tx, scope, id)
			if err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

// complaintsWaiting is the queue's rows for a complaint still open with
// nothing done for a week, in dealerWaiting's shape, for whoever sells cars.
const complaintsWaiting = `
	SELECT 'complaint_stalled', 'vehicle_sale', c.sale_id::text, ` + carNameSQL + `,
	       (SELECT max(e.at) FROM vehicle_sale_complaint_events e WHERE e.complaint_id = c.id) + interval '7 days', c.created_at
	FROM vehicle_sale_complaints c
	JOIN vehicle_sales s ON s.id = c.sale_id JOIN stock_units u ON u.id = s.stock_unit_id JOIN vehicles v ON v.id = u.vehicle_id
	LEFT JOIN vehicle_registrations reg ON reg.vehicle_id = v.id AND reg.valid_to IS NULL
	WHERE (SELECT e.status FROM vehicle_sale_complaint_events e WHERE e.complaint_id = c.id ORDER BY e.seq DESC LIMIT 1)
	      NOT IN ('closed', 'referred_arn')
	  AND (SELECT max(e.at) FROM vehicle_sale_complaint_events e WHERE e.complaint_id = c.id) < clock_timestamp() - interval '7 days'`
