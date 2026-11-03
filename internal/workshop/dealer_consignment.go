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

// Consignment (#166): a car sold on a customer's behalf under a written
// agreement that says in whose name. The two models give different
// invoices, VAT and responsibility, so the model is the agreement's choice,
// never a flag on the car. The car is a stock unit the dealer does not own
// (StockUnit.Owned): on the stock list, out of the stock value. A version
// of the agreement is never changed; a new one supersedes it.

// The models.
const (
	ConsignOwnName       = "own_name"
	ConsignPrincipalName = "principal_name"
)

// ConsignmentTerms are what a version of the agreement says.
type ConsignmentTerms struct {
	Model string
	// What the principal is to have: one of the two.
	MinimumPriceMinor *int64
	NetPayoutMinor    *int64
	// The dealer's commission: one of the two.
	CommissionMinor *int64
	CommissionBasis *int
	PrincipalCosts  string
	InsuredBy       string // dealer or principal
	SellerType      string
	StartsOn        time.Time
	EndsOn          time.Time
	Terms           string
}

// Consignment is one version of a consignment agreement.
type Consignment struct {
	ID            string
	Number        int64
	StockUnitID   string
	PrincipalID   string
	PrincipalName string
	Version       int
	RootID        string
	SupersedesID  string
	ConsignmentTerms
	CreatedAt time.Time
	EndedAt   *time.Time
	EndKind   string
	EndReason string
}

// Standing reports whether this version has not ended.
func (c Consignment) Standing() bool { return c.EndedAt == nil }

// NewConsignment is a car taken in to sell on a principal's behalf.
type NewConsignment struct {
	VehicleID   string
	PrincipalID string
	// A principal not yet known, made a party in the agreement's own
	// transaction. Used instead of PrincipalID.
	NewPrincipal *NewParty
	ConsignmentTerms
	// The request's own key: replayed, it finds the agreement it made.
	Key string
}

var (
	errConsignTerms = fmt.Errorf("%w: a consignment agreement says in whose name the car is sold, a minimum price or a net payout, a commission as an amount or a share up to 100 per cent, who insures the car, what kind of seller the principal is, and a period that ends on or after it starts", ErrInvalid)
	errConsignText  = fmt.Errorf("%w: keep the costs to 500 characters and the terms to 2000", ErrInvalid)
	errConsignEnded = fmt.Errorf("%w: that consignment agreement has already ended", ErrInvalid)
	errConsignKey   = fmt.Errorf("%w: that request was already used to take in another car", ErrInvalid)
	errConsignEnd   = fmt.Errorf("%w: a consignment ends withdrawn by the principal or run out", ErrInvalid)
	errConsignMoved = fmt.Errorf("%w: the agreement was changed meanwhile; read it again", ErrInvalid)
	// ErrConsignNoReason is a consignment ended without saying why.
	ErrConsignNoReason = fmt.Errorf("%w: say why the consignment ends, in 300 characters or fewer", ErrInvalid)
)

func checkConsignmentTerms(t *ConsignmentTerms) error {
	t.PrincipalCosts, t.Terms = strings.TrimSpace(t.PrincipalCosts), strings.TrimSpace(t.Terms)
	ok := func(p *int64) bool { return p == nil || (*p >= 0 && *p <= 100_000_000_000) }
	switch {
	case t.Model != ConsignOwnName && t.Model != ConsignPrincipalName,
		(t.MinimumPriceMinor == nil) == (t.NetPayoutMinor == nil), !ok(t.MinimumPriceMinor), !ok(t.NetPayoutMinor),
		(t.CommissionMinor == nil) == (t.CommissionBasis == nil), !ok(t.CommissionMinor),
		t.CommissionBasis != nil && (*t.CommissionBasis < 0 || *t.CommissionBasis > 10000),
		t.InsuredBy != "dealer" && t.InsuredBy != "principal", !slices.Contains(SellerTypes, t.SellerType),
		t.StartsOn.IsZero(), t.EndsOn.Before(t.StartsOn):
		return errConsignTerms
	case utf8.RuneCountInString(t.PrincipalCosts) > 500 || utf8.RuneCountInString(t.Terms) > 2000:
		return errConsignText
	}
	return nil
}

// ConsignCar takes a car in to sell on a principal's behalf: its cycle in
// stock opens as consigned, and the agreement's first version is recorded,
// in one transaction. For whoever buys cars, in a shop that sells them.
// Replayed with the same key, it returns the agreement it made.
func ConsignCar(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, n NewConsignment) (Consignment, error) {
	if !scope.Role.BuysVehicles() {
		return Consignment{}, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return Consignment{}, err
	}
	var principalNote string
	if n.NewPrincipal != nil {
		p := *n.NewPrincipal
		p.Role = PartyConsignor
		var err error
		if p, principalNote, err = checkNewParty(p); err != nil {
			return Consignment{}, err
		}
		n.NewPrincipal, n.PrincipalID = &p, ""
	} else if !looksLikeUUID(n.PrincipalID) {
		return Consignment{}, ErrNotFound
	}
	if !looksLikeUUID(n.VehicleID) {
		return Consignment{}, ErrNotFound
	}
	if err := checkConsignmentTerms(&n.ConsignmentTerms); err != nil {
		return Consignment{}, err
	}
	n.Key = strings.TrimSpace(n.Key)
	var out Consignment
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if n.Key != "" {
			var id, vehicle string
			err := tx.QueryRow(ctx, `SELECT a.id, u.vehicle_id FROM consignment_agreements a JOIN stock_units u ON u.id = a.stock_unit_id
				WHERE a.request_key = $1`, n.Key).Scan(&id, &vehicle)
			if err == nil {
				if !strings.EqualFold(vehicle, n.VehicleID) {
					return errConsignKey
				}
				out, err = consignmentTx(ctx, tx, id)
				return err
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("read the request: %w", err)
			}
		}
		if n.NewPrincipal != nil {
			var err error
			if n.PrincipalID, err = createPartyTx(ctx, tx, scope, *n.NewPrincipal, principalNote); err != nil {
				return err
			}
		} else {
			if err := partySeenTx(ctx, tx, scope, n.PrincipalID); err != nil {
				return err
			}
			if err := startConsignorTx(ctx, tx, scope, n.PrincipalID); err != nil {
				return err
			}
		}
		unit, err := openStockUnitTx(ctx, tx, scope, n.VehicleID, StockFromConsignment, "")
		if err != nil {
			return err
		}
		id, err := insertConsignmentTx(ctx, tx, scope, unit.ID, n.PrincipalID, 1, "", "", n.ConsignmentTerms, n.Key)
		if err != nil {
			return err
		}
		out, err = consignmentTx(ctx, tx, id)
		return err
	})
	return out, err
}

// startConsignorTx makes a party a consignor, if not one already.
func startConsignorTx(ctx context.Context, tx pgx.Tx, scope access.Scope, party string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO party_roles (shop_id, party_id, role, started_by)
		SELECT $1, party_now($2), 'consignor', NULLIF($3, '')::uuid
		WHERE NOT EXISTS (SELECT 1 FROM party_roles WHERE party_id = party_now($2) AND role = 'consignor' AND valid_to IS NULL)`,
		scope.ShopID, party, scope.UserID)
	if err != nil {
		return fmt.Errorf("make the principal a consignor: %w", err)
	}
	return nil
}

func insertConsignmentTx(ctx context.Context, tx pgx.Tx, scope access.Scope, unit, principal string, version int, root, supersedes string,
	t ConsignmentTerms, key string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `
		WITH n AS (SELECT gen_random_uuid() AS id)
		INSERT INTO consignment_agreements (id, shop_id, stock_unit_id, principal_party_id, version, root_id, supersedes_id, model,
		        minimum_price_minor, net_payout_minor, commission_minor, commission_basis, principal_costs, insured_by, seller_type,
		        starts_on, ends_on, terms, request_key, created_by)
		SELECT n.id, $1, $2, party_now($3), $4, coalesce(NULLIF($5, '')::uuid, n.id), NULLIF($6, '')::uuid, $7,
		       $8, $9, $10, $11, nullif($12, ''), $13, $14, $15::date, $16::date, nullif($17, ''), nullif($18, ''), NULLIF($19, '')::uuid
		FROM n RETURNING id`,
		scope.ShopID, unit, principal, version, root, supersedes, t.Model, t.MinimumPriceMinor, t.NetPayoutMinor, t.CommissionMinor,
		t.CommissionBasis, t.PrincipalCosts, t.InsuredBy, t.SellerType, t.StartsOn.Format(time.DateOnly), t.EndsOn.Format(time.DateOnly),
		t.Terms, key, scope.UserID).Scan(&id)
	if isUniqueViolation(err, "consignment_agreements_one_standing") {
		return "", errConsignMoved
	}
	if err != nil {
		return "", fmt.Errorf("record the consignment agreement: %w", err)
	}
	return id, nil
}

// ReviseConsignment makes a new version of a standing agreement --
// a lower minimum the principal approved in writing, a longer period --
// superseding version, which must be the one standing. For whoever buys
// cars.
func ReviseConsignment(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string, t ConsignmentTerms) (Consignment, error) {
	if !scope.Role.BuysVehicles() {
		return Consignment{}, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return Consignment{}, err
	}
	if !looksLikeUUID(id) {
		return Consignment{}, ErrNotFound
	}
	if err := checkConsignmentTerms(&t); err != nil {
		return Consignment{}, err
	}
	var out Consignment
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		c, err := lockConsignmentTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if !c.Standing() {
			return errConsignMoved
		}
		if err := endConsignmentTx(ctx, tx, scope, id, "replaced", ""); err != nil {
			return err
		}
		next, err := insertConsignmentTx(ctx, tx, scope, c.StockUnitID, c.PrincipalID, c.Version+1, c.RootID, c.ID, t, "")
		if err != nil {
			return err
		}
		out, err = consignmentTx(ctx, tx, next)
		return err
	})
	return out, err
}

// EndConsignment ends a standing agreement without a sale -- withdrawn by
// the principal, or run out -- with why, and closes the car's cycle in
// stock: it goes back to its principal. Any charges agreed are settled
// apart. For whoever buys cars.
func EndConsignment(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id, kind, reason string) error {
	if !scope.Role.BuysVehicles() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return err
	}
	if !looksLikeUUID(id) {
		return ErrNotFound
	}
	if kind != "withdrawn" && kind != "expired" {
		return errConsignEnd
	}
	reason, err := checkReason(reason, ErrConsignNoReason)
	if err != nil {
		return err
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		c, err := lockConsignmentTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if !c.Standing() {
			return errConsignEnded
		}
		if err := endConsignmentTx(ctx, tx, scope, id, kind, reason); err != nil {
			return err
		}
		return moveStockUnitTx(ctx, tx, scope, c.StockUnitID, StockClosed, reason)
	})
}

func endConsignmentTx(ctx context.Context, tx pgx.Tx, scope access.Scope, id, kind, reason string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE consignment_agreements SET ended_at = clock_timestamp(), ended_by = NULLIF($2, '')::uuid, end_kind = $3,
		       end_reason = nullif($4, '')
		WHERE id = $1`, id, scope.UserID, kind, reason); err != nil {
		return fmt.Errorf("end the consignment agreement: %w", err)
	}
	return nil
}

func lockConsignmentTx(ctx context.Context, tx pgx.Tx, id string) (Consignment, error) {
	if _, err := tx.Exec(ctx, `SELECT 1 FROM stock_units WHERE id = (SELECT stock_unit_id FROM consignment_agreements WHERE id = $1) FOR UPDATE`,
		id); err != nil {
		return Consignment{}, fmt.Errorf("lock the car: %w", err)
	}
	return consignmentTx(ctx, tx, id)
}

const consignmentColumns = `
	SELECT a.id, a.number, a.stock_unit_id, a.principal_party_id, ` + partyNameSQL + `, a.version, a.root_id,
	       coalesce(a.supersedes_id::text, ''), a.model, a.minimum_price_minor, a.net_payout_minor, a.commission_minor,
	       a.commission_basis, coalesce(a.principal_costs, ''), a.insured_by, a.seller_type, a.starts_on, a.ends_on,
	       coalesce(a.terms, ''), a.created_at, a.ended_at, coalesce(a.end_kind, ''), coalesce(a.end_reason, '')
	FROM consignment_agreements a
	JOIN parties pa ON pa.id = a.principal_party_id LEFT JOIN customers c ON c.id = pa.customer_id
	LEFT JOIN people cp ON cp.id = c.person_id LEFT JOIN people pp ON pp.id = pa.person_id`

func scanConsignment(row pgx.Row) (Consignment, error) {
	var c Consignment
	var basis *int32
	err := row.Scan(&c.ID, &c.Number, &c.StockUnitID, &c.PrincipalID, &c.PrincipalName, &c.Version, &c.RootID, &c.SupersedesID,
		&c.Model, &c.MinimumPriceMinor, &c.NetPayoutMinor, &c.CommissionMinor, &basis, &c.PrincipalCosts, &c.InsuredBy,
		&c.SellerType, &c.StartsOn, &c.EndsOn, &c.Terms, &c.CreatedAt, &c.EndedAt, &c.EndKind, &c.EndReason)
	if basis != nil {
		b := int(*basis)
		c.CommissionBasis = &b
	}
	return c, err
}

func consignmentTx(ctx context.Context, tx pgx.Tx, id string) (Consignment, error) {
	c, err := scanConsignment(tx.QueryRow(ctx, consignmentColumns+` WHERE a.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Consignment{}, ErrNotFound
	}
	return c, err
}

// ConsignmentsOfUnit is a consigned car's agreement, every version, newest
// first, for whoever buys or sells cars: the salesperson needs to know in
// whose name it is sold and the minimum. Reading is Core's.
func ConsignmentsOfUnit(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, unitID string) ([]Consignment, error) {
	if !scope.Role.BuysVehicles() && !scope.Role.SellsVehicles() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(unitID) {
		return nil, ErrNotFound
	}
	var out []Consignment
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, consignmentColumns+` WHERE a.stock_unit_id = $1 ORDER BY a.version DESC`, unitID)
		if err != nil {
			return fmt.Errorf("read consignment agreements: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanConsignment(rows)
			if err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

// endConsignmentOnSaleTx ends a sold car's standing agreement as sold.
func endConsignmentOnSaleTx(ctx context.Context, tx pgx.Tx, scope access.Scope, unit string) error {
	_, err := tx.Exec(ctx, `
		UPDATE consignment_agreements SET ended_at = clock_timestamp(), ended_by = NULLIF($2, '')::uuid, end_kind = 'sold'
		WHERE stock_unit_id = $1 AND ended_at IS NULL`, unit, scope.UserID)
	if err != nil {
		return fmt.Errorf("end the consignment as sold: %w", err)
	}
	return nil
}
