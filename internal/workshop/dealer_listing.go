package workshop

import (
	"context"
	"crypto/sha256"
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

// A stock car offered for sale (#167, #183): what it is offered at and how
// it is described, a row per change; its own photographs, ordered, the
// first the cover; and the feed a dealer's website reads, built from an
// allow-list of fields and nothing else.

// Listing is how a stock car is offered, as the newest row says.
type Listing struct {
	AskingPriceMinor *int64
	Variant          string
	Fuel             string
	Gearbox          string
	Colour           string
	VATDeductible    bool
	Equipment        []string
	Description      string
	ChangedAt        time.Time
}

var errListing = fmt.Errorf("%w: a price up to a billion kronor, short words for the variant, fuel, gearbox and colour, at most 100 items of equipment and a description of 4000 characters", ErrInvalid)

// SetListing records how a stock car is offered, as a new row: the old
// price stays behind it. For whoever buys or sells cars.
func SetListing(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, unitID string, l Listing) error {
	if !scope.Role.BuysVehicles() && !scope.Role.SellsVehicles() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return err
	}
	if !looksLikeUUID(unitID) {
		return ErrNotFound
	}
	trim := func(s string, max int) (string, bool) {
		s = strings.TrimSpace(s)
		return s, utf8.RuneCountInString(s) <= max
	}
	var ok [5]bool
	l.Variant, ok[0] = trim(l.Variant, 100)
	l.Fuel, ok[1] = trim(l.Fuel, 40)
	l.Gearbox, ok[2] = trim(l.Gearbox, 40)
	l.Colour, ok[3] = trim(l.Colour, 60)
	l.Description, ok[4] = trim(l.Description, 4000)
	var equipment []string
	for _, e := range l.Equipment {
		if e = strings.TrimSpace(e); e != "" {
			if utf8.RuneCountInString(e) > 100 {
				return errListing
			}
			equipment = append(equipment, e)
		}
	}
	if ok != [5]bool{true, true, true, true, true} || len(equipment) > 100 ||
		(l.AskingPriceMinor != nil && (*l.AskingPriceMinor < 0 || *l.AskingPriceMinor > 100_000_000_000)) {
		return errListing
	}
	if equipment == nil {
		equipment = []string{}
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := unitOpenTx(ctx, tx, unitID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO stock_listings (shop_id, stock_unit_id, asking_price_minor, variant, fuel, gearbox, colour, vat_deductible,
			                            equipment, description, created_by)
			VALUES ($1, $2, $3, nullif($4, ''), nullif($5, ''), nullif($6, ''), nullif($7, ''), $8, $9, nullif($10, ''), NULLIF($11, '')::uuid)`,
			scope.ShopID, unitID, l.AskingPriceMinor, l.Variant, l.Fuel, l.Gearbox, l.Colour, l.VATDeductible, equipment,
			l.Description, scope.UserID)
		return err
	})
}

// ListingOf is how a stock car is offered now; the zero listing when it has
// not been described. For whoever buys or sells cars; Core's.
func ListingOf(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, unitID string) (Listing, error) {
	if !scope.Role.BuysVehicles() && !scope.Role.SellsVehicles() {
		return Listing{}, access.ErrForbidden
	}
	if !looksLikeUUID(unitID) {
		return Listing{}, ErrNotFound
	}
	var out Listing
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT asking_price_minor, coalesce(variant, ''), coalesce(fuel, ''), coalesce(gearbox, ''), coalesce(colour, ''),
			       vat_deductible, equipment, coalesce(description, ''), created_at
			FROM stock_listings WHERE stock_unit_id = $1 ORDER BY created_at DESC, id DESC LIMIT 1`, unitID).Scan(
			&out.AskingPriceMinor, &out.Variant, &out.Fuel, &out.Gearbox, &out.Colour, &out.VATDeductible, &out.Equipment,
			&out.Description, &out.ChangedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	return out, err
}

// StockPhoto is one of a stock car's photographs.
type StockPhoto struct {
	ID       string
	Key      string
	Position int
}

// AttachStockPhoto adds a photograph, already stored, to a stock car, last
// in order. For whoever buys or sells cars.
func AttachStockPhoto(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, unitID, key, contentType string, size int64, name string) (StockPhoto, error) {
	if !scope.Role.BuysVehicles() && !scope.Role.SellsVehicles() {
		return StockPhoto{}, access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return StockPhoto{}, err
	}
	if !looksLikeUUID(unitID) {
		return StockPhoto{}, ErrNotFound
	}
	var out StockPhoto
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := unitOpenTx(ctx, tx, unitID); err != nil {
			return err
		}
		out.Key = key
		return tx.QueryRow(ctx, `
			INSERT INTO attachments (shop_id, stock_unit_id, position, storage_key, content_type, byte_size, original_name, uploaded_by)
			VALUES ($1, $2, coalesce((SELECT max(position) FROM attachments WHERE stock_unit_id = $2), 0) + 1, $3, $4, $5, nullif($6, ''),
			        NULLIF($7, '')::uuid)
			RETURNING id, position`, scope.ShopID, unitID, key, contentType, size, name, scope.UserID).Scan(&out.ID, &out.Position)
	})
	return out, err
}

// StockPhotos is a stock car's photographs in order, the cover first. For
// whoever buys or sells cars; Core's.
func StockPhotos(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, unitID string) ([]StockPhoto, error) {
	if !scope.Role.BuysVehicles() && !scope.Role.SellsVehicles() {
		return nil, access.ErrForbidden
	}
	if !looksLikeUUID(unitID) {
		return nil, ErrNotFound
	}
	var out []StockPhoto
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = stockPhotosTx(ctx, tx, unitID)
		return err
	})
	return out, err
}

func stockPhotosTx(ctx context.Context, tx pgx.Tx, unitID string) ([]StockPhoto, error) {
	rows, err := tx.Query(ctx, `SELECT id, storage_key, position FROM attachments WHERE stock_unit_id = $1 ORDER BY position, created_at`, unitID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StockPhoto
	for rows.Next() {
		var p StockPhoto
		if err := rows.Scan(&p.ID, &p.Key, &p.Position); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// MakeCover moves a stock car's photograph to the front, the others after
// it in the order they had. For whoever buys or sells cars.
func MakeCover(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, photoID string) error {
	if !scope.Role.BuysVehicles() && !scope.Role.SellsVehicles() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return err
	}
	if !looksLikeUUID(photoID) {
		return ErrNotFound
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var unit string
		if err := tx.QueryRow(ctx, `SELECT stock_unit_id FROM attachments WHERE id = $1 AND stock_unit_id IS NOT NULL`, photoID).Scan(&unit); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if _, err := unitOpenTx(ctx, tx, unit); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE attachments a SET position = o.n
			FROM (SELECT id, row_number() OVER (ORDER BY (id = $2) DESC, position, created_at) AS n
			      FROM attachments WHERE stock_unit_id = $1) o
			WHERE a.id = o.id`, unit, photoID)
		return err
	})
}

// RemoveStockPhoto removes a stock car's photograph, row and file: the
// file after the row is gone, so a rollback never leaves a row without
// it. For whoever buys or sells cars.
func RemoveStockPhoto(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, files PhotoRemover, photoID string) error {
	if !scope.Role.BuysVehicles() && !scope.Role.SellsVehicles() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return err
	}
	if !looksLikeUUID(photoID) {
		return ErrNotFound
	}
	var key string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `DELETE FROM attachments WHERE id = $1 AND stock_unit_id IS NOT NULL RETURNING storage_key`, photoID).Scan(&key)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	if err != nil {
		return err
	}
	if files != nil {
		_ = files.Remove(key)
	}
	return nil
}

// FeedSettings are how the shop publishes its stock feed.
type FeedSettings struct {
	Published       bool
	Open            bool
	HasToken        bool
	IncludeReserved bool
}

// SetFeed sets how the stock feed is published; token, when not empty,
// replaces the one there was, which then stops working. Whoever runs the
// shop.
func SetFeed(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, published, open, includeReserved bool, token string) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return err
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var hash []byte
		if token != "" {
			sum := sha256.Sum256([]byte(token))
			hash = sum[:]
		} else {
			err := tx.QueryRow(ctx, `SELECT token_sha256 FROM stock_feed_settings ORDER BY changed_at DESC, id DESC LIMIT 1`).Scan(&hash)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		if published && !open && hash == nil {
			return fmt.Errorf("%w: a feed behind a token needs a token", ErrInvalid)
		}
		_, err := tx.Exec(ctx, `INSERT INTO stock_feed_settings (shop_id, published, open, token_sha256, include_reserved, changed_by)
			VALUES ($1, $2, $3, $4, $5, NULLIF($6, '')::uuid)`, scope.ShopID, published, open, hash, includeReserved, scope.UserID)
		return err
	})
}

// FeedSettingsOf reads how the feed is published. Whoever runs the shop.
func FeedSettingsOf(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) (FeedSettings, error) {
	if !scope.Role.RunsTheShop() {
		return FeedSettings{}, access.ErrForbidden
	}
	var out FeedSettings
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT published, open, token_sha256 IS NOT NULL, include_reserved FROM stock_feed_settings
			ORDER BY changed_at DESC, id DESC LIMIT 1`).Scan(&out.Published, &out.Open, &out.HasToken, &out.IncludeReserved)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	return out, err
}

// FeedCar is one car as the public feed shows it: the allow-list, and
// nothing else. A field added to a stock car reaches the feed only by
// being added here.
type FeedCar struct {
	ID                string    `json:"id"`
	Make              string    `json:"make"`
	Model             string    `json:"model"`
	Variant           string    `json:"variant,omitempty"`
	ModelYear         *int      `json:"model_year,omitempty"`
	OdometerKm        *int64    `json:"odometer_km,omitempty"`
	Fuel              string    `json:"fuel,omitempty"`
	Gearbox           string    `json:"gearbox,omitempty"`
	Colour            string    `json:"colour,omitempty"`
	PriceIncludingVAT *int64    `json:"price_minor_including_vat,omitempty"`
	VATDeductible     bool      `json:"vat_deductible_for_business"`
	Equipment         []string  `json:"equipment"`
	Description       string    `json:"description,omitempty"`
	Reserved          bool      `json:"reserved"`
	Photos            []string  `json:"photos"`
	ChangedAt         time.Time `json:"changed_at"`
}

// ErrFeedClosed is a feed not published, or asked for with no token or the
// wrong one: it answers as a path that does not exist.
var ErrFeedClosed = errors.New("workshop: the stock feed is not open to this request")

// StockFeed is the shop's public stock feed: the cars for sale -- and, if
// the shop chooses, reserved -- with the allow-listed fields and their
// photographs' public ids, changed since since when that is not zero. No
// scope: it is public, read in the shop's own transaction, and refuses
// unless published and, behind a token, asked with it. The newest change
// is the feed's Last-Modified.
func StockFeed(ctx context.Context, pool *pgxpool.Pool, shopID, token string, since time.Time) ([]FeedCar, time.Time, error) {
	var out []FeedCar
	var newest time.Time
	err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		s, hash, err := feedSettingsTx(ctx, tx)
		if err != nil {
			return err
		}
		if !s.Published || (!s.Open && (token == "" || sha256.Sum256([]byte(token)) != [32]byte(hash))) {
			return ErrFeedClosed
		}
		rows, err := tx.Query(ctx, `
			SELECT u.id, coalesce(v.make, ''), coalesce(v.model, ''), coalesce(l.variant, ''), v.model_year,
			       (SELECT o.km FROM odometer_readings o WHERE o.vehicle_id = u.vehicle_id ORDER BY o.read_at DESC LIMIT 1),
			       coalesce(l.fuel, ''), coalesce(l.gearbox, ''), coalesce(l.colour, ''), l.asking_price_minor,
			       coalesce(l.vat_deductible, false), coalesce(l.equipment, '{}'), coalesce(l.description, ''), u.status = 'reserved',
			       ARRAY(SELECT a.id::text FROM attachments a WHERE a.stock_unit_id = u.id ORDER BY a.position, a.created_at),
			       greatest(u.first_for_sale_at, l.created_at,
			                (SELECT max(a.created_at) FROM attachments a WHERE a.stock_unit_id = u.id), u.opened_at)
			FROM stock_units u JOIN vehicles v ON v.id = u.vehicle_id
			LEFT JOIN LATERAL (SELECT * FROM stock_listings x WHERE x.stock_unit_id = u.id ORDER BY x.created_at DESC, x.id DESC LIMIT 1) l ON true
			WHERE u.closed_at IS NULL AND (u.status = 'for_sale' OR ($1 AND u.status = 'reserved'))
			ORDER BY u.first_for_sale_at DESC NULLS LAST, u.id`, s.IncludeReserved)
		if err != nil {
			return fmt.Errorf("read the feed: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var c FeedCar
			var year *int16
			if err := rows.Scan(&c.ID, &c.Make, &c.Model, &c.Variant, &year, &c.OdometerKm, &c.Fuel, &c.Gearbox, &c.Colour,
				&c.PriceIncludingVAT, &c.VATDeductible, &c.Equipment, &c.Description, &c.Reserved, &c.Photos, &c.ChangedAt); err != nil {
				return err
			}
			if year != nil {
				y := int(*year)
				c.ModelYear = &y
			}
			if c.ChangedAt.After(newest) {
				newest = c.ChangedAt
			}
			if since.IsZero() || c.ChangedAt.After(since) {
				out = append(out, c)
			}
		}
		return rows.Err()
	})
	return out, newest, err
}

func feedSettingsTx(ctx context.Context, tx pgx.Tx) (FeedSettings, []byte, error) {
	var s FeedSettings
	var hash []byte
	err := tx.QueryRow(ctx, `SELECT published, open, include_reserved, token_sha256 FROM stock_feed_settings
		ORDER BY changed_at DESC, id DESC LIMIT 1`).Scan(&s.Published, &s.Open, &s.IncludeReserved, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return FeedSettings{}, nil, nil
	}
	return s, hash, err
}

// FeedPhoto is a photograph's storage key, by its public id, while its car
// is in the feed: one removed, or of a car sold, is not found.
func FeedPhoto(ctx context.Context, pool *pgxpool.Pool, shopID, token, photoID string) (string, error) {
	if !looksLikeUUID(photoID) {
		return "", ErrNotFound
	}
	var key string
	err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		s, hash, err := feedSettingsTx(ctx, tx)
		if err != nil {
			return err
		}
		if !s.Published || (!s.Open && (token == "" || sha256.Sum256([]byte(token)) != [32]byte(hash))) {
			return ErrFeedClosed
		}
		err = tx.QueryRow(ctx, `
			SELECT a.storage_key FROM attachments a JOIN stock_units u ON u.id = a.stock_unit_id
			WHERE a.id = $1 AND u.closed_at IS NULL AND (u.status = 'for_sale' OR ($2 AND u.status = 'reserved'))`,
			photoID, s.IncludeReserved).Scan(&key)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return key, err
}
