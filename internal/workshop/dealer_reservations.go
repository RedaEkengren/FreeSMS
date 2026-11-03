package workshop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// A car reserved for a customer until a stated time (#174). A reservation
// is a claim on the sale, not on the car's time (0128): the holder may still
// test-drive it and the workshop take it in, but nobody else buys it. One
// stands per car. Past its expiry it still stands, in front of whoever sells
// cars, until a person ends it and decides on the deposit: nothing is kept
// unasked. Extended, it ends and a new one supersedes it.

// Reservation is one reservation of a car.
type Reservation struct {
	ID            string
	Number        int64
	StockUnitID   string
	PartyID       string
	HolderName    string
	SalespersonID string
	Salesperson   string
	ExpiresAt     time.Time
	Refundable    bool
	Conditions    string
	RootID        string
	SupersedesID  string
	CreatedAt     time.Time
	EndedAt       *time.Time
	EndKind       string
	EndReason     string
	// What was decided about the deposit, as it ended without a sale.
	DepositDecision string
}

// Standing reports whether the reservation has not ended.
func (r Reservation) Standing() bool { return r.EndedAt == nil }

// Lapsed reports whether a standing reservation is past its expiry: still
// the car's, waiting for a person to end it.
func (r Reservation) Lapsed(now time.Time) bool { return r.Standing() && !r.ExpiresAt.After(now) }

// NewReservation is a car reserved for a customer.
type NewReservation struct {
	StockUnitID string
	PartyID     string
	// A holder not yet known, made a party in the reservation's own
	// transaction, so a refused reservation leaves nobody behind. Used
	// instead of PartyID.
	NewHolder  *NewParty
	ExpiresAt  time.Time
	Refundable bool
	Conditions string
	// The request's own key: replayed, it finds the reservation it made.
	Key string
}

// The ways a reservation ends without a sale, and the decisions on its
// deposit.
const (
	ReservationCancelled = "cancelled"
	ReservationLapsed    = "lapsed"
	DepositRefund        = "refund"
	DepositRetain        = "retain"
	DepositNone          = "none"
)

var (
	errReservationExpiry    = fmt.Errorf("%w: a reservation runs until a moment still to come, and for no more than ninety days", ErrInvalid)
	errReservationText      = fmt.Errorf("%w: keep the conditions to 1000 characters", ErrInvalid)
	errReservationCar       = fmt.Errorf("%w: only a car for sale can be reserved", ErrInvalid)
	errReservationTaken     = fmt.Errorf("%w: the car is already reserved; end that reservation first", ErrInvalid)
	errReservationEnded     = fmt.Errorf("%w: that reservation has already ended", ErrInvalid)
	errReservationNotLapsed = fmt.Errorf("%w: the reservation has not run out yet; cancel it instead", ErrInvalid)
	errReservationDecision  = fmt.Errorf("%w: say whether the deposit is refunded, kept, or there was none", ErrInvalid)
	errReservationKey       = fmt.Errorf("%w: that request was already used to reserve something else", ErrInvalid)
	// ErrReservationNoReason is a reservation ended without saying why.
	ErrReservationNoReason = fmt.Errorf("%w: say why the reservation ends, in 300 characters or fewer", ErrInvalid)
	// ErrReservedForAnother is a car reserved for someone else than the
	// one it is being sold to.
	ErrReservedForAnother = fmt.Errorf("%w: the car is reserved for another customer; end that reservation first", ErrInvalid)
	// errReservationStanding is a reserved car moved by hand while its
	// reservation stands: the reservation is ended, with its deposit
	// decided, instead.
	errReservationStanding = fmt.Errorf("%w: the car's reservation still stands; end the reservation instead", ErrInvalid)
)

const maxReservation = 90 * 24 * time.Hour

func checkReservationTerms(expires time.Time, conditions string) (string, error) {
	now := time.Now()
	if !expires.After(now) || expires.After(now.Add(maxReservation)) {
		return "", errReservationExpiry
	}
	conditions = strings.TrimSpace(conditions)
	if utf8.RuneCountInString(conditions) > 1000 {
		return "", errReservationText
	}
	return conditions, nil
}

// Reserve reserves a car for sale for a customer until a moment, and moves
// it to reserved, in one transaction. For whoever sells cars, in a shop
// that sells them. Two attempts at once leave one (0128); replayed with the
// same key, it returns the reservation it made.
func Reserve(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, n NewReservation) (Reservation, error) {
	if !scope.Role.SellsVehicles() {
		return Reservation{}, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return Reservation{}, err
	}
	var holderNote string
	if n.NewHolder != nil {
		holder := *n.NewHolder
		holder.Role = PartyBuyer
		var err error
		if holder, holderNote, err = checkNewParty(holder); err != nil {
			return Reservation{}, err
		}
		n.NewHolder, n.PartyID = &holder, ""
	} else if !looksLikeUUID(n.PartyID) {
		return Reservation{}, ErrNotFound
	}
	if !looksLikeUUID(n.StockUnitID) {
		return Reservation{}, ErrNotFound
	}
	conditions, err := checkReservationTerms(n.ExpiresAt, n.Conditions)
	if err != nil {
		return Reservation{}, err
	}
	n.Key = strings.TrimSpace(n.Key)
	var out Reservation
	err = database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if n.Key != "" {
			var id, unit string
			err := tx.QueryRow(ctx, `SELECT id, stock_unit_id FROM vehicle_reservations WHERE request_key = $1`, n.Key).Scan(&id, &unit)
			if err == nil {
				if !strings.EqualFold(unit, n.StockUnitID) {
					return errReservationKey
				}
				out, err = reservationTx(ctx, tx, id)
				return err
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("read the request: %w", err)
			}
		}
		if n.NewHolder == nil {
			if err := partySeenTx(ctx, tx, scope, n.PartyID); err != nil {
				return err
			}
		}
		var status StockStatus
		err := tx.QueryRow(ctx, `SELECT status FROM stock_units WHERE id = $1 FOR UPDATE`, n.StockUnitID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read stock unit: %w", err)
		}
		if status != StockForSale {
			if status == StockReserved {
				return errReservationTaken
			}
			return errReservationCar
		}
		if n.NewHolder != nil {
			if n.PartyID, err = createPartyTx(ctx, tx, scope, *n.NewHolder, holderNote); err != nil {
				return err
			}
		}
		id, err := insertReservationTx(ctx, tx, scope, n.StockUnitID, n.PartyID, n.ExpiresAt, n.Refundable, conditions, "", "", n.Key)
		if err != nil {
			return err
		}
		if err := moveStockUnitTx(ctx, tx, scope, n.StockUnitID, StockReserved, ""); err != nil {
			return err
		}
		out, err = reservationTx(ctx, tx, id)
		return err
	})
	return out, err
}

// insertReservationTx writes a reservation, a first one (root and
// supersedes empty) or an extension, in the caller's transaction. Another
// standing reservation of the car is refused by the database (0128).
func insertReservationTx(ctx context.Context, tx pgx.Tx, scope access.Scope, unit, party string, expires time.Time, refundable bool,
	conditions, root, supersedes, key string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `
		WITH n AS (SELECT gen_random_uuid() AS id)
		INSERT INTO vehicle_reservations (id, shop_id, stock_unit_id, party_id, salesperson_id, expires_at, refundable, conditions,
		                                  root_id, supersedes_id, request_key, created_by)
		SELECT n.id, $1, $2, party_now($3), NULLIF($4, '')::uuid, $5, $6, nullif($7, ''),
		       coalesce(NULLIF($8, '')::uuid, n.id), NULLIF($9, '')::uuid, nullif($10, ''), NULLIF($4, '')::uuid
		FROM n RETURNING id`,
		scope.ShopID, unit, party, scope.UserID, expires, refundable, conditions, root, supersedes, key).Scan(&id)
	if isUniqueViolation(err, "vehicle_reservations_one_standing") {
		return "", errReservationTaken
	}
	if err != nil {
		return "", fmt.Errorf("record the reservation: %w", err)
	}
	return id, nil
}

// ExtendReservation ends a standing reservation as extended and makes a new
// one, superseding it, until a new moment: a new version with its own
// expiry, the same holder and the deposit held against the first. For
// whoever sells cars.
func ExtendReservation(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string, expires time.Time, conditions string) (Reservation, error) {
	if !scope.Role.SellsVehicles() {
		return Reservation{}, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return Reservation{}, err
	}
	if !looksLikeUUID(id) {
		return Reservation{}, ErrNotFound
	}
	conditions, err := checkReservationTerms(expires, conditions)
	if err != nil {
		return Reservation{}, err
	}
	var out Reservation
	err = database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		r, err := lockReservationTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if !r.Standing() {
			return errReservationEnded
		}
		if conditions == "" {
			conditions = r.Conditions
		}
		if err := endReservationTx(ctx, tx, scope, id, "extended", "", ""); err != nil {
			return err
		}
		next, err := insertReservationTx(ctx, tx, scope, r.StockUnitID, r.PartyID, expires, r.Refundable, conditions, r.RootID, r.ID, "")
		if err != nil {
			return err
		}
		out, err = reservationTx(ctx, tx, next)
		return err
	})
	return out, err
}

// EndReservation ends a standing reservation without a sale -- cancelled,
// or lapsed once past its expiry -- with why, and what a person decided
// about the deposit: refunded, kept, or there was none. The car goes back
// to for sale. Refunding is recorded in the payment journal as money given
// back; the decision here is what was decided, an audit event. For whoever
// sells cars.
func EndReservation(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id, kind, decision, reason string) error {
	if !scope.Role.SellsVehicles() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return err
	}
	if !looksLikeUUID(id) {
		return ErrNotFound
	}
	if kind != ReservationCancelled && kind != ReservationLapsed {
		return fmt.Errorf("%w: a reservation ends cancelled or lapsed", ErrInvalid)
	}
	if decision != DepositRefund && decision != DepositRetain && decision != DepositNone {
		return errReservationDecision
	}
	reason, err := checkReason(reason, ErrReservationNoReason)
	if err != nil {
		return err
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		r, err := lockReservationTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if !r.Standing() {
			return errReservationEnded
		}
		if kind == ReservationLapsed && r.ExpiresAt.After(time.Now()) {
			return errReservationNotLapsed
		}
		if err := endReservationTx(ctx, tx, scope, id, kind, reason, decision); err != nil {
			return err
		}
		var status StockStatus
		if err := tx.QueryRow(ctx, `SELECT status FROM stock_units WHERE id = $1`, r.StockUnitID).Scan(&status); err != nil {
			return fmt.Errorf("read stock unit: %w", err)
		}
		if status == StockReserved {
			if err := moveStockUnitTx(ctx, tx, scope, r.StockUnitID, StockForSale, ""); err != nil {
				return err
			}
		}
		return auditTx(ctx, tx, scope, r.StockUnitID, "reservation.ended", "reserved", kind, reason,
			map[string]any{"reservation": r.ID, "deposit": decision})
	})
}

// endReservationTx writes how a reservation ended, once (0128).
func endReservationTx(ctx context.Context, tx pgx.Tx, scope access.Scope, id, kind, reason, decision string) error {
	_, err := tx.Exec(ctx, `
		UPDATE vehicle_reservations SET ended_at = clock_timestamp(), ended_by = NULLIF($2, '')::uuid, end_kind = $3,
		       end_reason = nullif($4, ''), deposit_decision = nullif($5, '')
		WHERE id = $1`, id, scope.UserID, kind, reason, decision)
	if err != nil {
		return fmt.Errorf("end the reservation: %w", err)
	}
	return nil
}

// endReservationOnSaleTx ends the car's standing reservation as sold, when
// the car is sold to its holder; a sale to anyone else is refused while it
// stands (ErrReservedForAnother).
func endReservationOnSaleTx(ctx context.Context, tx pgx.Tx, scope access.Scope, unit string, buyers []string) error {
	var id, holder string
	err := tx.QueryRow(ctx, `SELECT id, party_now(party_id) FROM vehicle_reservations WHERE stock_unit_id = $1 AND ended_at IS NULL FOR UPDATE`,
		unit).Scan(&id, &holder)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the reservation: %w", err)
	}
	for _, b := range buyers {
		var now string
		if err := tx.QueryRow(ctx, `SELECT party_now($1)`, b).Scan(&now); err != nil {
			return fmt.Errorf("read the buyer: %w", err)
		}
		if now == holder {
			return endReservationTx(ctx, tx, scope, id, "sold", "", "")
		}
	}
	return ErrReservedForAnother
}

// reservedForTx is the party a car is reserved for while its reservation
// stands, or "" when none stands: the holder drives their own reserved car
// whatever the shop's block (#173).
func reservedForTx(ctx context.Context, tx pgx.Tx, unitID string) (string, error) {
	var holder string
	err := tx.QueryRow(ctx, `SELECT party_now(party_id) FROM vehicle_reservations WHERE stock_unit_id = $1 AND ended_at IS NULL`,
		unitID).Scan(&holder)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return holder, err
}

// lockReservationTx reads a reservation to change, its car locked first.
func lockReservationTx(ctx context.Context, tx pgx.Tx, id string) (Reservation, error) {
	if _, err := tx.Exec(ctx, `SELECT 1 FROM stock_units WHERE id = (SELECT stock_unit_id FROM vehicle_reservations WHERE id = $1) FOR UPDATE`,
		id); err != nil {
		return Reservation{}, fmt.Errorf("lock the car: %w", err)
	}
	return reservationTx(ctx, tx, id)
}

const reservationColumns = `
	SELECT r.id, r.number, r.stock_unit_id, r.party_id, ` + partyNameSQL + `, coalesce(r.salesperson_id::text, ''),
	       coalesce(sp.display_name, ''), r.expires_at, r.refundable, coalesce(r.conditions, ''), r.root_id,
	       coalesce(r.supersedes_id::text, ''), r.created_at, r.ended_at, coalesce(r.end_kind, ''), coalesce(r.end_reason, ''),
	       coalesce(r.deposit_decision, '')
	FROM vehicle_reservations r
	JOIN parties pa ON pa.id = r.party_id LEFT JOIN customers c ON c.id = pa.customer_id
	LEFT JOIN people cp ON cp.id = c.person_id LEFT JOIN people pp ON pp.id = pa.person_id
	LEFT JOIN users su ON su.id = r.salesperson_id LEFT JOIN people sp ON sp.id = su.person_id`

func scanReservation(row pgx.Row) (Reservation, error) {
	var r Reservation
	err := row.Scan(&r.ID, &r.Number, &r.StockUnitID, &r.PartyID, &r.HolderName, &r.SalespersonID, &r.Salesperson, &r.ExpiresAt,
		&r.Refundable, &r.Conditions, &r.RootID, &r.SupersedesID, &r.CreatedAt, &r.EndedAt, &r.EndKind, &r.EndReason, &r.DepositDecision)
	return r, err
}

func reservationTx(ctx context.Context, tx pgx.Tx, id string) (Reservation, error) {
	r, err := scanReservation(tx.QueryRow(ctx, reservationColumns+` WHERE r.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Reservation{}, ErrNotFound
	}
	return r, err
}

// ReservationsOfUnit is a car's reservations, newest first, for whoever
// sells cars. Reading is Core's: a shop that stops selling cars keeps the
// history.
func ReservationsOfUnit(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, unitID string) ([]Reservation, error) {
	if !scope.Role.SellsVehicles() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(unitID) {
		return nil, ErrNotFound
	}
	var out []Reservation
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, reservationColumns+` WHERE r.stock_unit_id = $1 ORDER BY r.created_at DESC, r.number DESC`, unitID)
		if err != nil {
			return fmt.Errorf("read reservations: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanReservation(rows)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// reservationsWaiting is the action queue's rows for reservations about to
// run out (within a day) or already run out and still standing, in the
// shape dealerWaiting reads, for whoever sells cars.
const reservationsWaiting = `
	SELECT CASE WHEN r.expires_at <= clock_timestamp() THEN 'reservation_lapsed' ELSE 'reservation_expiring' END,
	       'stock_unit', r.stock_unit_id::text, ` + carNameSQL + `, r.expires_at, r.created_at
	FROM vehicle_reservations r
	JOIN stock_units u ON u.id = r.stock_unit_id JOIN vehicles v ON v.id = u.vehicle_id
	LEFT JOIN vehicle_registrations reg ON reg.vehicle_id = v.id AND reg.valid_to IS NULL
	WHERE r.ended_at IS NULL AND r.expires_at <= clock_timestamp() + interval '1 day'`

// exportReservationsTx adds to a person's export the cars reserved for any
// party that is them: as a private party, or as a customer's person.
func exportReservationsTx(ctx context.Context, tx pgx.Tx, personID string, e *PersonExport) error {
	rows, err := tx.Query(ctx, `
		SELECT `+carNameSQL+`, r.created_at, r.expires_at, r.refundable, coalesce(r.conditions, ''), r.ended_at,
		       coalesce(r.end_kind, ''), coalesce(r.deposit_decision, '')
		FROM vehicle_reservations r
		JOIN parties pa ON pa.id = r.party_id LEFT JOIN customers c ON c.id = pa.customer_id
		JOIN stock_units u ON u.id = r.stock_unit_id JOIN vehicles v ON v.id = u.vehicle_id
		LEFT JOIN vehicle_registrations reg ON reg.vehicle_id = v.id AND reg.valid_to IS NULL
		WHERE pa.person_id = $1 OR c.person_id = $1
		ORDER BY r.created_at, r.number`, personID)
	if err != nil {
		return fmt.Errorf("read reservations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var x ExportedReservation
		if err := rows.Scan(&x.Car, &x.Made, &x.Until, &x.Refundable, &x.Conditions, &x.Ended, &x.How, &x.DepositDecision); err != nil {
			return err
		}
		e.Reservations = append(e.Reservations, x)
	}
	return rows.Err()
}

// RecordReservationDeposit records money received as a reservation's
// deposit: held, not income (#152), against the first reservation of its
// chain so an extension keeps it. payerPartyID is who paid when it is not
// the holder -- a parent, a company -- or "" for the holder. The deposit is
// opened with its first payment, in one transaction. For whoever sells
// cars, while the reservation stands.
func RecordReservationDeposit(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, reservationID, payerPartyID string, p NewPayment) (Payment, error) {
	if !scope.Role.SellsVehicles() {
		return Payment{}, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return Payment{}, err
	}
	if !looksLikeUUID(reservationID) || (payerPartyID != "" && !looksLikeUUID(payerPartyID)) {
		return Payment{}, ErrNotFound
	}
	if p.AmountMinor == nil || *p.AmountMinor <= 0 {
		return Payment{}, fmt.Errorf("%w: say how much was received", ErrInvalid)
	}
	if err := checkMoney(p); err != nil {
		return Payment{}, err
	}
	var out Payment
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		r, err := lockReservationTx(ctx, tx, reservationID)
		if err != nil {
			return err
		}
		if !r.Standing() {
			return errReservationEnded
		}
		payer := r.PartyID
		if payerPartyID != "" {
			if err := partySeenTx(ctx, tx, scope, payerPartyID); err != nil {
				return err
			}
			payer = payerPartyID
		}
		var deposit string
		err = tx.QueryRow(ctx, `SELECT id FROM deposits WHERE subject_kind = 'reservation' AND subject_id = $1 ORDER BY created_at LIMIT 1`,
			r.RootID).Scan(&deposit)
		if errors.Is(err, pgx.ErrNoRows) {
			err = tx.QueryRow(ctx, `
				INSERT INTO deposits (shop_id, party_id, purpose, subject_kind, subject_id, created_by)
				VALUES ($1, party_now($2), 'reservation', 'reservation', $3, $4) RETURNING id`,
				scope.ShopID, payer, r.RootID, scope.UserID).Scan(&deposit)
		}
		if err != nil {
			return fmt.Errorf("the reservation's deposit: %w", err)
		}
		out = Payment{DepositID: deposit, AmountMinor: *p.AmountMinor, Method: p.Method, PaidOn: p.PaidOn,
			Reference: strings.TrimSpace(p.Reference), Provider: p.Provider, ExternalID: p.ExternalID, Key: p.Key}
		_, err = insertPaymentOnce(ctx, tx, scope, &out)
		return err
	})
	return out, err
}

// ReservationDeposit is what is held as the deposit of a reservation's
// chain: received, applied, refunded. The zero balance when none was paid.
// For whoever sells cars; reading is Core's.
func ReservationDeposit(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, reservationID string) (DepositBalance, error) {
	if !scope.Role.SellsVehicles() {
		return DepositBalance{}, access.ErrForbidden
	}
	if !looksLikeUUID(reservationID) {
		return DepositBalance{}, ErrNotFound
	}
	var out DepositBalance
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var deposit string
		err := tx.QueryRow(ctx, `
			SELECT d.id FROM vehicle_reservations r JOIN deposits d ON d.subject_kind = 'reservation' AND d.subject_id = r.root_id
			WHERE r.id = $1 ORDER BY d.created_at LIMIT 1`, reservationID).Scan(&deposit)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read the deposit: %w", err)
		}
		out, err = depositTx(ctx, tx, deposit, false)
		return err
	})
	return out, err
}
