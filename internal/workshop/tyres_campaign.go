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

// A seasonal message to every customer with a set in storage (#230). The
// audience is worked out on the day from the sets in a place, a customer is
// reached once however many sets or cars they have, and one who has opted out
// is left out and counted.

var (
	errCampaign        = fmt.Errorf("%w: a campaign has a name of 1 to 100 characters, not used by another, a season, and a text of up to 1200 characters", ErrInvalid)
	errCampaignSeason  = fmt.Errorf("%w: a campaign is for summer, winter or all-season sets", ErrInvalid)
	errCampaignRunTime = fmt.Errorf("%w: say when the messages may go", ErrInvalid)
)

// CampaignSeasons are the kinds of set a campaign can be about, in the order a
// form offers them.
var CampaignSeasons = []string{"summer", "winter", "all_season"}

// NewCampaign is what the shop writes to start a campaign.
type NewCampaign struct {
	Name    string
	Season  string
	Subject string
	Body    string
}

// Campaign is one made, with how it stands.
type Campaign struct {
	ID        string
	Name      string
	Season    string
	Subject   string
	Body      string
	CreatedAt time.Time
	// How many customers it has been queued for.
	Reached int
}

// CampaignCustomer is one customer a campaign would reach, with what they
// would be told about.
type CampaignCustomer struct {
	CustomerID string
	Name       string
	// "sms" or "email": how the message would go.
	Channel string
	// Their sets in storage, one line each: the tyre and the car if it has one.
	Sets []string
	// The message as this customer would read it.
	Message string
}

// CampaignAudience is who a campaign would reach today and who it would not.
type CampaignAudience struct {
	Reached []CampaignCustomer
	// Left out because the customer opted out of shop messages.
	OptedOut int
	// Left out because there is nowhere to send it.
	NoContact int
	// Already queued for this campaign, so not queued again.
	AlreadyQueued int
}

// CreateCampaign writes a campaign. Whoever runs the shop: it is the shop
// speaking to its customers.
func CreateCampaign(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, n NewCampaign) (string, error) {
	if !scope.Role.RunsTheShop() {
		return "", access.ErrForbidden
	}
	n.Name, n.Subject, n.Body = strings.TrimSpace(n.Name), strings.TrimSpace(n.Subject), strings.TrimSpace(n.Body)
	if !slices.Contains(CampaignSeasons, n.Season) {
		return "", errCampaignSeason
	}
	if utf8.RuneCountInString(n.Name) < 1 || utf8.RuneCountInString(n.Name) > 100 || n.Body == "" ||
		utf8.RuneCountInString(n.Body) > 1200 || utf8.RuneCountInString(n.Subject) > 200 {
		return "", errCampaign
	}
	var id string
	err := inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO tyre_campaigns (shop_id, name, season, subject, body, created_by) VALUES ($1, $2, $3, $4, $5, NULLIF($6, '')::uuid)
			ON CONFLICT (shop_id, name) DO NOTHING RETURNING id`,
			scope.ShopID, n.Name, n.Season, n.Subject, n.Body, scope.UserID).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return errCampaign
		}
		return err
	})
	return id, err
}

// Campaigns lists the campaigns, the newest first. Whoever deals with customers.
func Campaigns(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]Campaign, error) {
	if !scope.Role.SeesCustomerPersonalData() && !scope.Role.RunsTheShop() {
		return nil, access.ErrForbidden
	}
	var out []Campaign
	err := inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT c.id::text, c.name, c.season, c.subject, c.body, c.created_at,
			       (SELECT count(*) FROM tyre_campaign_recipients r WHERE r.campaign_id = c.id)::int
			FROM tyre_campaigns c ORDER BY c.created_at DESC, c.id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c Campaign
			if err := rows.Scan(&c.ID, &c.Name, &c.Season, &c.Subject, &c.Body, &c.CreatedAt, &c.Reached); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

func campaignSeasons(kind string) []string {
	if kind == "winter" {
		return []string{SeasonWinterStudded, SeasonWinterFriction}
	}
	return []string{kind}
}

// audienceTx works out who a campaign would reach. Everything is read in the
// one transaction, so what the preview shows is what a run would use.
func audienceTx(ctx context.Context, tx pgx.Tx, campaignID string) (Campaign, CampaignAudience, error) {
	var c Campaign
	var a CampaignAudience
	if err := tx.QueryRow(ctx, `SELECT id::text, name, season, subject, body FROM tyre_campaigns WHERE id = $1`, campaignID).
		Scan(&c.ID, &c.Name, &c.Season, &c.Subject, &c.Body); errors.Is(err, pgx.ErrNoRows) {
		return c, a, ErrNotFound
	} else if err != nil {
		return c, a, err
	}
	rows, err := tx.Query(ctx, `
		SELECT c.id::text,
		       coalesce(CASE WHEN c.kind = 'company' THEN c.company_name END, p.display_name, ''),
		       coalesce(nullif(btrim(p.phone), ''), ''), coalesce(p.email::text, ''),
		       ts.brand || CASE WHEN ts.model <> '' THEN ' ' || ts.model ELSE '' END,
		       coalesce(r.registration, ''),
		       coalesce((SELECT ch.kind FROM customer_message_choices ch WHERE ch.customer_id = c.id ORDER BY ch.seq DESC LIMIT 1), 'opted_in') = 'opted_out',
		       EXISTS (SELECT 1 FROM tyre_campaign_recipients cr WHERE cr.campaign_id = $1 AND cr.customer_id = c.id)
		FROM tyre_sets ts
		JOIN tyre_set_placements pl ON pl.tyre_set_id = ts.id AND pl.removed_at IS NULL
		JOIN customers c ON c.id = ts.customer_id
		LEFT JOIN people p ON p.id = c.person_id
		LEFT JOIN LATERAL (SELECT e.vehicle_id FROM tyre_set_vehicle_events e WHERE e.tyre_set_id = ts.id ORDER BY e.seq DESC LIMIT 1) m ON true
		LEFT JOIN vehicle_registrations r ON r.vehicle_id = m.vehicle_id AND r.valid_to IS NULL
		WHERE ts.season = ANY($2) AND NOT EXISTS (SELECT 1 FROM shop_own_customers o WHERE o.customer_id = c.id)
		ORDER BY 2, c.id, ts.created_at, ts.id`, campaignID, campaignSeasons(c.Season))
	if err != nil {
		return c, a, err
	}
	defer rows.Close()
	seen := map[string]*CampaignCustomer{}
	var order []string
	status := map[string][3]bool{} // opted out, already queued, has an address
	for rows.Next() {
		var id, name, phone, email, tyre, reg string
		var out, queued bool
		if err := rows.Scan(&id, &name, &phone, &email, &tyre, &reg, &out, &queued); err != nil {
			return c, a, err
		}
		cc := seen[id]
		if cc == nil {
			cc = &CampaignCustomer{CustomerID: id, Name: name}
			switch {
			case phone != "":
				cc.Channel = "sms"
			case email != "":
				cc.Channel = "email"
			}
			seen[id] = cc
			order = append(order, id)
			status[id] = [3]bool{out, queued, cc.Channel != ""}
		}
		line := tyre
		if reg != "" {
			line += " (" + reg + ")"
		}
		cc.Sets = append(cc.Sets, line)
	}
	if err := rows.Err(); err != nil {
		return c, a, err
	}
	for _, id := range order {
		cc, st := seen[id], status[id]
		switch {
		case st[1]:
			a.AlreadyQueued++
		case st[0]:
			a.OptedOut++
		case !st[2]:
			a.NoContact++
		default:
			cc.Message = strings.ReplaceAll(c.Body, "{sets}", strings.Join(cc.Sets, ", "))
			a.Reached = append(a.Reached, *cc)
		}
	}
	return c, a, nil
}

// CampaignPreview says who a campaign would reach today and the message each
// would read. Nothing is queued. Whoever deals with customers.
func CampaignPreview(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, campaignID string) (Campaign, CampaignAudience, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return Campaign{}, CampaignAudience{}, access.ErrForbidden
	}
	if !looksLikeUUID(campaignID) {
		return Campaign{}, CampaignAudience{}, ErrNotFound
	}
	var c Campaign
	var a CampaignAudience
	err := inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		c, a, err = audienceTx(ctx, tx, campaignID)
		return err
	})
	return c, a, err
}

// RunCampaign queues the message for everyone the campaign reaches today who
// has not been queued for it already. It can be run again, after a failure or
// a click made twice, and queues nothing a second time: a customer is a row
// that exists once per campaign. Whoever runs the shop; it returns the
// audience it queued for.
func RunCampaign(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, campaignID string, notBefore time.Time) (CampaignAudience, error) {
	if !scope.Role.RunsTheShop() {
		return CampaignAudience{}, access.ErrForbidden
	}
	if !looksLikeUUID(campaignID) {
		return CampaignAudience{}, ErrNotFound
	}
	if notBefore.IsZero() {
		return CampaignAudience{}, errCampaignRunTime
	}
	var a CampaignAudience
	err := inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		// One run at a time per campaign, so two clicks are told apart.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "campaign:"+campaignID); err != nil {
			return err
		}
		c, aud, err := audienceTx(ctx, tx, campaignID)
		if err != nil {
			return err
		}
		a = aud
		for _, who := range aud.Reached {
			var address string
			if who.Channel == "sms" {
				err = tx.QueryRow(ctx, `SELECT btrim(p.phone) FROM customers c JOIN people p ON p.id = c.person_id WHERE c.id = $1`, who.CustomerID).Scan(&address)
			} else {
				err = tx.QueryRow(ctx, `SELECT p.email::text FROM customers c JOIN people p ON p.id = c.person_id WHERE c.id = $1`, who.CustomerID).Scan(&address)
			}
			if err != nil {
				return fmt.Errorf("read the address: %w", err)
			}
			var msg string
			if err := tx.QueryRow(ctx, `
				INSERT INTO outbound_messages (shop_id, campaign_id, about, channel, recipient, subject, body, not_before, created_by)
				VALUES ($1, $2, 'campaign', $3, $4, $5, $6, $7, $8) RETURNING id`,
				scope.ShopID, c.ID, who.Channel, address, c.Subject, who.Message, notBefore, scope.UserID).Scan(&msg); err != nil {
				return fmt.Errorf("queue message: %w", err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO message_events (shop_id, message_id, event) VALUES ($1, $2, 'queued')`, scope.ShopID, msg); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO tyre_campaign_recipients (shop_id, campaign_id, customer_id, message_id) VALUES ($1, $2, $3, $4)`,
				scope.ShopID, c.ID, who.CustomerID, msg); err != nil {
				return fmt.Errorf("record the recipient: %w", err)
			}
		}
		return nil
	})
	return a, err
}

// CampaignMessageStillWanted is asked as a campaign message is about to go:
// whether the customer has opted out since it was queued.
func CampaignMessageStillWanted(ctx context.Context, pool *pgxpool.Pool, shopID, messageID string) (bool, string, error) {
	var out bool
	err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT coalesce((SELECT ch.kind FROM customer_message_choices ch WHERE ch.customer_id = r.customer_id ORDER BY ch.seq DESC LIMIT 1), 'opted_in') = 'opted_out'
			FROM tyre_campaign_recipients r WHERE r.message_id = $1`, messageID).Scan(&out)
	})
	if err != nil {
		return false, "", err
	}
	return !out, "The customer opted out of messages before this went; nothing was sent.", nil
}

// SetMessageOptOut records that a customer does not want the shop's seasonal
// messages, or that they do again. It does not touch what the shop must tell
// them about a job they have. Whoever deals with customers.
func SetMessageOptOut(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, customerID string, optedOut bool) error {
	if !scope.Role.SeesCustomerPersonalData() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(customerID) {
		return ErrNotFound
	}
	kind := "opted_in"
	if optedOut {
		kind = "opted_out"
	}
	return inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var one int
		if err := tx.QueryRow(ctx, `SELECT 1 FROM customers WHERE id = $1`, customerID).Scan(&one); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO customer_message_choices (shop_id, customer_id, kind, by_user) VALUES ($1, $2, $3, NULLIF($4, '')::uuid)`,
			scope.ShopID, customerID, kind, scope.UserID)
		return err
	})
}

// MessageOptedOut says whether a customer has opted out of the shop's seasonal
// messages: the newest of their choices. Whoever deals with customers.
func MessageOptedOut(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, customerID string) (bool, error) {
	if !scope.Role.SeesCustomerPersonalData() {
		return false, access.ErrForbidden
	}
	if !looksLikeUUID(customerID) {
		return false, ErrNotFound
	}
	var out bool
	err := inWorkshop(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT coalesce((SELECT kind FROM customer_message_choices WHERE customer_id = $1 ORDER BY seq DESC LIMIT 1), 'opted_in') = 'opted_out'`,
			customerID).Scan(&out)
	})
	return out, err
}
