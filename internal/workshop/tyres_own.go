package workshop

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// The shop's own tyres (#167, #233): the sets on its stock cars and fleet cars.
// They belong to one customer of the shop's own, made the first time it is
// needed, so a set is a set whoever it belongs to and nothing else about tyres
// needs to know the difference -- except what is charged and written: the
// shop pays itself no storage and sends itself no campaign.

// ownCustomerTx is the shop's own customer, made when there is none.
func ownCustomerTx(ctx context.Context, tx pgx.Tx, scope access.Scope) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT customer_id::text FROM shop_own_customers`).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "owncustomer:"+scope.ShopID); err != nil {
		return "", err
	}
	// Another request may have made it while this waited for the lock.
	err = tx.QueryRow(ctx, `SELECT customer_id::text FROM shop_own_customers`).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if err := tx.QueryRow(ctx, `INSERT INTO customers (shop_id, kind, company_name) SELECT id, 'company', name FROM shops WHERE id = $1 RETURNING id::text`, scope.ShopID).Scan(&id); err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `INSERT INTO shop_own_customers (shop_id, customer_id) VALUES ($1, $2)`, scope.ShopID, id)
	return id, err
}

// CreateShopTyreSet takes in a set that belongs to the shop itself, on one of
// its own cars or loose. Whoever looks after the shop's stock or fleet, or runs
// it.
func CreateShopTyreSet(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, n NewTyreSet) (string, error) {
	if !scope.Role.BuysVehicles() && !scope.Role.SellsVehicles() && !scope.Role.HandsOverRentals() && !scope.Role.RunsTheShop() {
		return "", access.ErrForbidden
	}
	n.CustomerID = ""
	if err := checkNewSet(&n); err != nil {
		return "", err
	}
	// The tyre hotel is the workshop's, though the cars are the dealer's and the
	// fleet's: the module is checked here and the role above.
	if err := scope.Require(access.Workshop); err != nil {
		return "", err
	}
	var id string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		owner, err := ownCustomerTx(ctx, tx, scope)
		if err != nil {
			return err
		}
		n.CustomerID = owner
		id, err = createTyreSetTx(ctx, tx, scope, n)
		return err
	})
	return id, err
}

// ShopTyreSets lists the sets on a car the shop controls, with where each is
// kept and how worn: the card's tyres. Whoever works in the workshop, buys or
// sells cars, or hands out rentals; the owner is never named, being the shop.
func ShopTyreSets(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, vehicleID string) ([]TyreSet, error) {
	if !scope.Role.WorksInTheWorkshop() && !scope.Role.BuysVehicles() && !scope.Role.SellsVehicles() && !scope.Role.HandsOverRentals() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(vehicleID) {
		return nil, ErrNotFound
	}
	return tyreSetsRead(ctx, pool, scope, TyreFilter{VehicleID: vehicleID})
}
