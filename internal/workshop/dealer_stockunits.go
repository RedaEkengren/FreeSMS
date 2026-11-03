package workshop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// Stock units (#160): one cycle of a car through the dealer's stock, from
// the day it came in to the day it left. A car bought twice is one vehicle
// with two units. The unit's status is the deal's alone: whether the car is
// free to drive away is the availability ledger's (resources.go), who owns
// it the ownership periods', what was paid the invoices' and payments'.

// Why a cycle opened.
const (
	StockFromPurchase    = "purchase"
	StockFromTradeIn     = "trade_in"
	StockFromConsignment = "consignment"
	StockFromRepurchase  = "repurchase"
	StockFromDefleet     = "defleet"
	StockFromImport      = "import"
)

// StockReasons is every reason, in the order a page offers them.
var StockReasons = []string{StockFromPurchase, StockFromTradeIn, StockFromConsignment, StockFromRepurchase,
	StockFromDefleet, StockFromImport}

// StockStatus is where a unit's deal stands.
type StockStatus string

// The statuses. Delivered, closed and written off end the cycle: delivered
// to its buyer; closed without a sale -- a consigned car handed back to its
// principal, a purchase undone before the car came; written off -- scrapped,
// stolen.
const (
	StockInPreparation StockStatus = "in_preparation"
	StockForSale       StockStatus = "for_sale"
	StockReserved      StockStatus = "reserved"
	StockSold          StockStatus = "sold"
	StockDelivered     StockStatus = "delivered"
	StockClosed        StockStatus = "closed"
	StockWrittenOff    StockStatus = "written_off"
)

// StockStatuses is every status, in the order a cycle goes through them.
var StockStatuses = []StockStatus{StockInPreparation, StockForSale, StockReserved, StockSold, StockDelivered,
	StockClosed, StockWrittenOff}

var stockStatusLabels = map[StockStatus]string{
	StockInPreparation: "In preparation",
	StockForSale:       "For sale",
	StockReserved:      "Reserved",
	StockSold:          "Sold, not yet delivered",
	StockDelivered:     "Delivered",
	StockClosed:        "Closed without a sale",
	StockWrittenOff:    "Written off (scrapped or lost)",
}

// Label is the status in words, as a catalogue key.
func (s StockStatus) Label() string { return stockStatusLabels[s] }

// Ended reports whether the cycle is over.
func (s StockStatus) Ended() bool {
	return s == StockDelivered || s == StockClosed || s == StockWrittenOff
}

// stockMoves is every move a unit's deal can make. A reservation and a
// sale fall through back to for sale; nothing comes back from an end.
var stockMoves = map[StockStatus][]StockStatus{
	StockInPreparation: {StockForSale, StockClosed, StockWrittenOff},
	StockForSale:       {StockInPreparation, StockReserved, StockSold, StockClosed, StockWrittenOff},
	StockReserved:      {StockForSale, StockSold, StockWrittenOff},
	StockSold:          {StockForSale, StockDelivered},
}

var (
	errStockNoIdentity = fmt.Errorf("%w: a car in stock needs its chassis number or its registration, because the margin scheme is accounted car by car", ErrInvalid)
	errStockOpen       = fmt.Errorf("%w: that car is already in stock; finish its cycle before starting another", ErrInvalid)
	errStockInFleet    = fmt.Errorf("%w: that car is in the rental fleet; end its time in the fleet before it comes into stock", ErrInvalid)
	errStockReason     = fmt.Errorf("%w: that is not a way a car comes into stock", ErrInvalid)
	errStockNotTaken   = fmt.Errorf("%w: a car bought, consigned or imported comes into stock through its purchase, its agreement or its import", ErrInvalid)
	errStockMove       = fmt.Errorf("%w: a car cannot go straight from there to there", ErrInvalid)
	errStockEnded      = fmt.Errorf("%w: that cycle is over; a car that comes back starts a new one", ErrInvalid)
	errStockBooked     = fmt.Errorf("%w: something is still booked on the car; release or replan that first", ErrInvalid)
	// ErrStockNoReason is a cycle ended without a sale and without saying why.
	ErrStockNoReason = fmt.Errorf("%w: say why the cycle ends without a sale, in 300 characters or fewer", ErrInvalid)
)

// StockMovesFor is where roles may move a unit from where it is now, for a
// page to offer only the moves it may make. It decides nothing: MoveStockUnit
// checks again.
func StockMovesFor(roles access.Roles, from StockStatus) []StockStatus {
	var out []StockStatus
	for _, to := range stockMoves[from] {
		if mayMoveStockRoles(roles, from, to) {
			out = append(out, to)
		}
	}
	return out
}

// mayMoveStock checks roles, and the Dealer module, for moving a unit to
// status: the buyer, who prepares the car and decides it is ready, may
// offer it for sale, take it back into preparation, and end a cycle
// without a sale; the salesperson reserves it, sells it and delivers it,
// and lets a reservation or a sale fall through. Whoever runs the shop does
// both.
func mayMoveStock(roles access.Roles, scope access.Scope, from, to StockStatus) error {
	if !mayMoveStockRoles(roles, from, to) {
		return access.ErrForbidden
	}
	return scope.Require(access.Dealer)
}

// mayMoveStockRoles is mayMoveStock's question of the roles alone.
func mayMoveStockRoles(roles access.Roles, from, to StockStatus) bool {
	var may bool
	switch to {
	case StockInPreparation, StockClosed, StockWrittenOff:
		may = roles.BuysVehicles()
	case StockForSale:
		// Back from a reservation or a sale that fell through is the
		// salesperson's; first offered, the buyer's.
		may = roles.BuysVehicles() || (roles.SellsVehicles() && (from == StockReserved || from == StockSold))
	case StockReserved, StockSold, StockDelivered:
		may = roles.SellsVehicles()
	}
	return may
}

// StockUnit is one cycle, as it is read.
type StockUnit struct {
	ID             string
	VehicleID      string
	ResourceID     string
	Reason         string
	Status         StockStatus
	OpenedAt       time.Time
	FirstForSaleAt *time.Time
	ClosedAt       *time.Time
	VehicleSaleID  string
}

// Owned reports whether the dealer owns the car in this cycle. A consigned
// car is sold on its principal's behalf and never the dealer's (#166).
func (u StockUnit) Owned() bool { return u.Reason != StockFromConsignment }

// ReasonLabel is why the cycle opened, as a catalogue key.
func (u StockUnit) ReasonLabel() string { return stockReasonLabels[u.Reason] }

var stockReasonLabels = map[string]string{
	StockFromPurchase:    "Bought",
	StockFromTradeIn:     "Traded in",
	StockFromConsignment: "On consignment",
	StockFromRepurchase:  "Bought back",
	StockFromDefleet:     "From the rental fleet",
	StockFromImport:      "Imported",
}

// openStockUnitTx opens a cycle for a vehicle in the caller's transaction:
// the car must be known by its chassis number or a registration it carries
// now, and becomes a resource, or comes back into use as the one it was.
// The database refuses a second open cycle (stock_units_one_cycle_at_a_time),
// so two buyers taking the same car in at once leave one unit.
func openStockUnitTx(ctx context.Context, tx pgx.Tx, scope access.Scope, vehicleID, reason, key string) (StockUnit, error) {
	if !slices.Contains(StockReasons, reason) {
		return StockUnit{}, errStockReason
	}
	if !looksLikeUUID(vehicleID) {
		return StockUnit{}, ErrNotFound
	}
	if key != "" {
		u, err := stockUnitTx(ctx, tx, `u.request_key = $1`, key)
		if err == nil {
			if u.VehicleID != vehicleID || u.Reason != reason {
				return StockUnit{}, fmt.Errorf("%w: that request was already used to take another car in", ErrInvalid)
			}
			return u, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return StockUnit{}, err
		}
	}
	var known bool
	err := tx.QueryRow(ctx, `
		SELECT nullif(btrim(v.vin), '') IS NOT NULL
		       OR EXISTS (SELECT 1 FROM vehicle_registrations r WHERE r.vehicle_id = v.id AND r.valid_to IS NULL)
		FROM vehicles v WHERE v.id = $1 AND v.merged_into IS NULL`, vehicleID).Scan(&known)
	if errors.Is(err, pgx.ErrNoRows) {
		return StockUnit{}, ErrNotFound
	}
	if err != nil {
		return StockUnit{}, fmt.Errorf("read vehicle: %w", err)
	}
	if !known {
		return StockUnit{}, errStockNoIdentity
	}
	resource, err := vehicleResourceTx(ctx, tx, scope, vehicleID)
	if err != nil {
		return StockUnit{}, err
	}
	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO stock_units (shop_id, vehicle_id, resource_id, reason, opened_by, request_key)
		VALUES ($1, $2, $3, $4, NULLIF($5, '')::uuid, nullif($6, '')) RETURNING id`,
		scope.ShopID, vehicleID, resource, reason, scope.UserID, key).Scan(&id)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.ConstraintName == "stock_units_one_cycle_at_a_time" {
		return StockUnit{}, errStockOpen
	}
	if errors.As(err, &pg) && pg.ConstraintName == "vehicle_fleet_or_stock" {
		return StockUnit{}, errStockInFleet
	}
	if err != nil {
		return StockUnit{}, fmt.Errorf("open the stock unit: %w", err)
	}
	if err := auditTx(ctx, tx, scope, id, "stock_unit.opened", "", string(StockInPreparation), "",
		map[string]any{"vehicle_id": vehicleID, "reason": reason}); err != nil {
		return StockUnit{}, err
	}
	return stockUnitTx(ctx, tx, `u.id = $1`, id)
}

const stockUnitColumns = `
	SELECT u.id, u.vehicle_id, u.resource_id, u.reason, u.status, u.opened_at, u.first_for_sale_at, u.closed_at,
	       coalesce(u.vehicle_sale_id::text, '')
	FROM stock_units u`

func scanStockUnit(row pgx.Row) (StockUnit, error) {
	var u StockUnit
	err := row.Scan(&u.ID, &u.VehicleID, &u.ResourceID, &u.Reason, &u.Status, &u.OpenedAt, &u.FirstForSaleAt,
		&u.ClosedAt, &u.VehicleSaleID)
	return u, err
}

func stockUnitTx(ctx context.Context, tx pgx.Tx, where string, arg any) (StockUnit, error) {
	u, err := scanStockUnit(tx.QueryRow(ctx, stockUnitColumns+` WHERE `+where, arg))
	if errors.Is(err, pgx.ErrNoRows) {
		return StockUnit{}, ErrNotFound
	}
	if err != nil {
		return StockUnit{}, fmt.Errorf("read stock unit: %w", err)
	}
	return u, nil
}

// TakeIntoStock opens a cycle for a car with no purchase to record: one
// moved over from the rental fleet. A car bought, traded in or bought back
// comes in through its purchase (RecordPurchase), a consigned car through
// its agreement, an imported one through its import, each opening the
// unit in its own transaction. For whoever buys cars.
func TakeIntoStock(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, vehicleID, reason, key string) (StockUnit, error) {
	if !scope.Role.BuysVehicles() {
		return StockUnit{}, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return StockUnit{}, err
	}
	if reason != StockFromDefleet {
		return StockUnit{}, errStockNotTaken
	}
	var u StockUnit
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		u, err = openStockUnitTx(ctx, tx, scope, vehicleID, reason, strings.TrimSpace(key))
		return err
	})
	return u, err
}

// MoveStockUnit moves a unit's deal from where it stands to status: the
// one way a unit's status changes, each move an audit event. Ending a cycle
// without a sale says why; ending it at all takes the car's resource out of
// use, refused while anything is booked on it from now on. Moving to where
// it already is does nothing, so a second tap is not an error.
func MoveStockUnit(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string, to StockStatus, reason string) error {
	if !slices.Contains(StockStatuses, to) {
		return errStockMove
	}
	if !looksLikeUUID(id) {
		return ErrNotFound
	}
	if !scope.Role.BuysVehicles() && !scope.Role.SellsVehicles() {
		return access.ErrForbidden
	}
	reason = strings.TrimSpace(reason)
	if to == StockClosed || to == StockWrittenOff {
		var err error
		if reason, err = checkReason(reason, ErrStockNoReason); err != nil {
			return err
		}
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		return moveStockUnitTx(ctx, tx, scope, id, to, reason)
	})
}

// moveStockUnitTx is MoveStockUnit in the caller's transaction, for a deal
// that sells its car as its contract is completed. The role is checked
// here, against the move.
func moveStockUnitTx(ctx context.Context, tx pgx.Tx, scope access.Scope, id string, to StockStatus, reason string) error {
	{
		var from StockStatus
		var resource string
		err := tx.QueryRow(ctx, `SELECT status, resource_id FROM stock_units WHERE id = $1 FOR UPDATE`, id).Scan(&from, &resource)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read stock unit: %w", err)
		}
		if err := mayMoveStock(scope.Role, scope, from, to); err != nil {
			return err
		}
		if from == to {
			return nil
		}
		if from.Ended() {
			return errStockEnded
		}
		if !slices.Contains(stockMoves[from], to) {
			return errStockMove
		}
		if to.Ended() {
			if err := retireResourceTx(ctx, tx, scope, resource); err != nil {
				if errors.Is(err, ErrInvalid) {
					return errStockBooked
				}
				return err
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE stock_units SET status = $2,
			       first_for_sale_at = CASE WHEN $2 = 'for_sale' THEN coalesce(first_for_sale_at, clock_timestamp()) ELSE first_for_sale_at END,
			       closed_at = CASE WHEN $3 THEN clock_timestamp() END,
			       closed_by = CASE WHEN $3 THEN NULLIF($4, '')::uuid END
			WHERE id = $1`, id, string(to), to.Ended(), scope.UserID); err != nil {
			return fmt.Errorf("move the stock unit: %w", err)
		}
		// A sale is a row (stock_unit_sales): made as the car is sold, and
		// marked as fallen through, never removed, when it goes back to for
		// sale. Delivered, the sale stands as it was.
		switch {
		case to == StockSold:
			if _, err := tx.Exec(ctx, `INSERT INTO stock_unit_sales (shop_id, stock_unit_id, sold_by) VALUES ($1, $2, NULLIF($3, '')::uuid)`,
				scope.ShopID, id, scope.UserID); err != nil {
				return fmt.Errorf("record the sale: %w", err)
			}
		case from == StockSold && to == StockForSale:
			if _, err := tx.Exec(ctx, `UPDATE stock_unit_sales SET fell_through_at = clock_timestamp(), fell_through_by = NULLIF($2, '')::uuid
				WHERE stock_unit_id = $1 AND fell_through_at IS NULL`, id, scope.UserID); err != nil {
				return fmt.Errorf("record the sale falling through: %w", err)
			}
		}
		return auditTx(ctx, tx, scope, id, "stock_unit.moved", string(from), string(to), reason, nil)
	}
}

// StockUnitByID reads a unit for whoever buys or sells cars.
func StockUnitByID(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string) (StockUnit, error) {
	// Whoever buys or sells cars. The workshop's roles follow a stock car
	// through its jobs, not its deal. Reading is Core's: a shop that stops
	// selling cars keeps its history.
	if !scope.Role.BuysVehicles() && !scope.Role.SellsVehicles() {
		return StockUnit{}, access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return StockUnit{}, ErrNotFound
	}
	var u StockUnit
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		u, err = stockUnitTx(ctx, tx, `u.id = $1`, id)
		return err
	})
	return u, err
}

// StockUnitsOfVehicle is every cycle a vehicle has had through the
// dealer's stock, oldest first: the car bought, sold and bought back is
// two. For whoever buys or sells cars.
func StockUnitsOfVehicle(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, vehicleID string) ([]StockUnit, error) {
	if !scope.Role.BuysVehicles() && !scope.Role.SellsVehicles() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(vehicleID) {
		return nil, ErrNotFound
	}
	var out []StockUnit
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, stockUnitColumns+` WHERE u.vehicle_id = $1 ORDER BY u.opened_at`, vehicleID)
		if err != nil {
			return fmt.Errorf("read stock units: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			u, err := scanStockUnit(rows)
			if err != nil {
				return fmt.Errorf("scan stock unit: %w", err)
			}
			out = append(out, u)
		}
		return rows.Err()
	})
	return out, err
}
