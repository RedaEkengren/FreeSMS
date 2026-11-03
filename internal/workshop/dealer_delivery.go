package workshop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// Delivering a sold car (#179): once per sale, its moment frozen. The
// Consumer Sales Act counts from it -- two years in which a defect is
// presumed to have been there at delivery, three in which the dealer
// answers for it -- and those dates are derived from it, never typed.

// Delivery is a sale's delivery as it is read.
type Delivery struct {
	SaleID         string
	DeliveredAt    time.Time
	AtHome         bool
	OdometerKm     *int64
	OverrideReason string
	// A consumer's sale: the consumer law's clocks run from delivery.
	Consumer bool
}

// PresumptionEnds is when a defect stops being presumed to have been there
// at delivery: two years on, for a consumer; zero for a business buyer.
func (d Delivery) PresumptionEnds() time.Time {
	if !d.Consumer {
		return time.Time{}
	}
	return d.DeliveredAt.AddDate(2, 0, 0)
}

// LiabilityEnds is when the dealer stops answering for a defect: three
// years on, for a consumer; zero for a business buyer.
func (d Delivery) LiabilityEnds() time.Time {
	if !d.Consumer {
		return time.Time{}
	}
	return d.DeliveredAt.AddDate(3, 0, 0)
}

// NewDelivery is a sold car handed over.
type NewDelivery struct {
	At         time.Time
	AtHome     bool
	OdometerKm *int64
	// Why the car is handed over before every condition is met: the
	// owner's decision, an audit event. Empty for none.
	Override string
}

// DeliveryRefused is a delivery that may not be made yet, with what is
// missing, as catalogue keys.
type DeliveryRefused struct {
	Missing []string
}

func (e *DeliveryRefused) Error() string {
	return "workshop: invalid: not ready to deliver: " + strings.Join(e.Missing, "; ")
}

// Unwrap makes a refusal an ErrInvalid.
func (e *DeliveryRefused) Unwrap() error { return ErrInvalid }

var (
	errDelivered     = fmt.Errorf("%w: the car has already been delivered", ErrInvalid)
	errDeliveryWhen  = fmt.Errorf("%w: say when it was handed over, and not a moment still to come", ErrInvalid)
	errDeliveryOwner = fmt.Errorf("%w: only whoever runs the shop lets a car go before every condition is met", ErrInvalid)
)

// The conditions a delivery is held to, as catalogue keys.
const (
	MissingContract = "The contract is signed, with every known defect accepted."
	MissingInvoice  = "The sale is invoiced."
	MissingPayment  = "The invoice is paid."
)

// DeliverCar hands a sold car over: refused unless the contract stands
// signed with every defect accepted (#175), the sale is invoiced (#177)
// and the invoice paid -- unless whoever runs the shop lets it go with a
// reason, an audit event. Its moment is frozen, the odometer recorded as
// read at delivery, and the car's cycle in stock ends as delivered, in one
// transaction. Not before it happened, and only once. For whoever sells
// cars.
func DeliverCar(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, saleID string, n NewDelivery) (Delivery, error) {
	if !scope.Role.SellsVehicles() {
		return Delivery{}, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return Delivery{}, err
	}
	if !looksLikeUUID(saleID) {
		return Delivery{}, ErrNotFound
	}
	if n.At.IsZero() || n.At.After(time.Now().Add(time.Hour)) {
		return Delivery{}, errDeliveryWhen
	}
	if n.OdometerKm != nil && (*n.OdometerKm < 0 || *n.OdometerKm >= 10_000_000) {
		return Delivery{}, errOdometer
	}
	n.Override = strings.TrimSpace(n.Override)
	if n.Override != "" && !scope.Role.RunsTheShop() {
		return Delivery{}, errDeliveryOwner
	}
	var out Delivery
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockDealTx(ctx, tx, saleID); err != nil {
			return err
		}
		d, err := dealTx(ctx, tx, scope, saleID)
		if err != nil {
			return err
		}
		var done bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM vehicle_sale_deliveries WHERE sale_id = $1)`, saleID).Scan(&done); err != nil {
			return fmt.Errorf("read the delivery: %w", err)
		}
		if done {
			return errDelivered
		}
		var missing []string
		if !d.Completed() {
			missing = append(missing, MissingContract)
		}
		var invoice string
		if err := tx.QueryRow(ctx, `SELECT coalesce(sale_invoice_standing($1)::text, '')`, saleID).Scan(&invoice); err != nil {
			return fmt.Errorf("read the invoice: %w", err)
		}
		if invoice == "" {
			missing = append(missing, MissingInvoice)
		} else {
			b, err := balanceTx(ctx, tx, invoice, false)
			if err != nil {
				return err
			}
			if b.OutstandingMinor() > 0 {
				missing = append(missing, MissingPayment)
			}
		}
		if len(missing) > 0 && n.Override == "" {
			return &DeliveryRefused{Missing: missing}
		}
		if !d.Completed() {
			// Nothing sold to deliver: no override hands a car over on a
			// contract nobody completed.
			return &DeliveryRefused{Missing: []string{MissingContract}}
		}
		var reading *string
		if n.OdometerKm != nil {
			var id string
			if err := tx.QueryRow(ctx, `
				INSERT INTO odometer_readings (shop_id, vehicle_id, km, source, read_at, decreasing)
				SELECT $1, u.vehicle_id, $3, 'delivery', $4,
				       EXISTS (SELECT 1 FROM odometer_readings o WHERE o.vehicle_id = u.vehicle_id AND o.km > $3)
				FROM stock_units u WHERE u.id = $2 RETURNING id`, scope.ShopID, d.StockUnitID, *n.OdometerKm, n.At).Scan(&id); err != nil {
				return fmt.Errorf("record the odometer: %w", err)
			}
			reading = &id
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO vehicle_sale_deliveries (shop_id, sale_id, delivered_at, at_home, odometer_reading_id, override_reason, recorded_by)
			VALUES ($1, $2, $3, $4, $5, nullif($6, ''), NULLIF($7, '')::uuid)`,
			scope.ShopID, saleID, n.At, n.AtHome, reading, n.Override, scope.UserID); err != nil {
			return fmt.Errorf("record the delivery: %w", err)
		}
		if len(missing) > 0 {
			if err := auditTx(ctx, tx, scope, saleID, "vehicle_sale.delivered_early", "", "delivered", n.Override,
				map[string]any{"missing": missing}); err != nil {
				return err
			}
		}
		if err := moveStockUnitTx(ctx, tx, scope, d.StockUnitID, StockDelivered, ""); err != nil {
			return err
		}
		out, err = deliveryTx(ctx, tx, saleID)
		return err
	})
	return out, err
}

func deliveryTx(ctx context.Context, tx pgx.Tx, saleID string) (Delivery, error) {
	var d Delivery
	err := tx.QueryRow(ctx, `
		SELECT x.sale_id, x.delivered_at, x.at_home, o.km, coalesce(x.override_reason, ''),
		       (SELECT t.buyer_kind FROM vehicle_sale_terms t WHERE t.sale_id = x.sale_id ORDER BY t.version DESC LIMIT 1) = 'consumer'
		FROM vehicle_sale_deliveries x LEFT JOIN odometer_readings o ON o.id = x.odometer_reading_id
		WHERE x.sale_id = $1`, saleID).Scan(&d.SaleID, &d.DeliveredAt, &d.AtHome, &d.OdometerKm, &d.OverrideReason, &d.Consumer)
	if errors.Is(err, pgx.ErrNoRows) {
		return Delivery{}, ErrNotFound
	}
	return d, err
}

// DeliveryOfSale reads a sale's delivery, or ErrNotFound before it. For
// whoever sells cars; reading is Core's, as the complaint view needs it
// for three years.
func DeliveryOfSale(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, saleID string) (Delivery, error) {
	if !scope.Role.SellsVehicles() {
		return Delivery{}, access.ErrForbidden
	}
	if !looksLikeUUID(saleID) {
		return Delivery{}, ErrNotFound
	}
	var out Delivery
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := dealTx(ctx, tx, scope, saleID); err != nil {
			return err
		}
		var err error
		out, err = deliveryTx(ctx, tx, saleID)
		return err
	})
	return out, err
}
