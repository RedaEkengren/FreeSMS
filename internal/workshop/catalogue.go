package workshop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Units a part can be counted in. The database refuses anything else; this
// is the same list, for the form and for a message that says why.
var PartUnits = []string{"each", "litre", "metre", "kilogram", "hour"}

const maxCodesPerPart = 20

// CatalogueEntry is a part as the parts desk edits it: what it is, not what
// the ledger says about it.
type CatalogueEntry struct {
	ID       string
	Number   string
	Name     string
	Unit     string
	Location string

	// What the shop pays and what it charges. A blank price is priced from
	// the markup bands; a blank cost is a part nobody has priced yet.
	CostMinor  *int64
	PriceMinor *int64

	MinimumMilli int64

	// Barcodes on the packaging, and any other number the part answers to.
	// The scanner finds a part by its number or by any of these.
	Codes []string

	Active bool
}

// CodesText is the codes as the form shows them, one per line.
func (e CatalogueEntry) CodesText() string { return strings.Join(e.Codes, "\n") }

// Minimum is the minimum as a person writes it.
func (e CatalogueEntry) Minimum() string { return formatMilli(e.MinimumMilli) }

func formatMilli(v int64) string {
	s := fmt.Sprintf("%d.%03d", v/1000, abs64(v%1000))
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// ParseCodes reads one code per line, dropping blanks and repeats.
func ParseCodes(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" && !seen[line] {
			seen[line] = true
			out = append(out, line)
		}
	}
	return out
}

// CatalogueEntryByID reads one part for editing, retired or not.
func CatalogueEntryByID(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string) (CatalogueEntry, error) {
	if !scope.Role.SeesParts() {
		return CatalogueEntry{}, access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return CatalogueEntry{}, ErrNotFound
	}
	var e CatalogueEntry
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT id, number, name, unit, coalesce(location, ''), cost_minor, price_minor,
			       (minimum_quantity * 1000)::bigint, active,
			       coalesce((SELECT array_agg(code ORDER BY code) FROM part_codes c WHERE c.part_id = p.id), '{}')
			FROM parts p WHERE id = $1`, id).Scan(&e.ID, &e.Number, &e.Name, &e.Unit, &e.Location,
			&e.CostMinor, &e.PriceMinor, &e.MinimumMilli, &e.Active, &e.Codes)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return e, err
}

// RetiredParts lists the parts taken out of use, so they can be brought back.
func RetiredParts(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]CatalogueEntry, error) {
	if !scope.Role.SeesParts() {
		return nil, access.ErrForbidden
	}
	var out []CatalogueEntry
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, number, name FROM parts WHERE NOT active ORDER BY number`)
		if err != nil {
			return fmt.Errorf("list retired parts: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var e CatalogueEntry
			if err := rows.Scan(&e.ID, &e.Number, &e.Name); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// SavePart adds a part when e.ID is empty, and otherwise changes one.
//
// There was no way to add a part at all: the stock page, the scanner,
// labels, the markup bands and a part request from a job all worked on parts
// that already existed, and the only rows were written by tests. A shop set
// up from nothing could not reach any of it.
//
// Changing a part changes what is shown and offered next. It does not touch
// the past: every movement carries the unit cost it moved at, and an issued
// invoice carries its own copy of its lines.
func SavePart(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, e CatalogueEntry) (string, error) {
	if !scope.Role.SeesParts() {
		return "", access.ErrForbidden
	}
	e.Number, e.Name, e.Location = strings.TrimSpace(e.Number), strings.TrimSpace(e.Name), strings.TrimSpace(e.Location)
	switch {
	case e.Number == "":
		return "", fmt.Errorf("%w: a part needs a number to be found by", ErrInvalid)
	case e.Name == "":
		return "", fmt.Errorf("%w: a part needs a name somebody will recognise", ErrInvalid)
	case utf8.RuneCountInString(e.Number) > 60 || utf8.RuneCountInString(e.Name) > 200:
		return "", fmt.Errorf("%w: the number or the name is too long", ErrInvalid)
	case !knownUnit(e.Unit):
		return "", fmt.Errorf("%w: %q is not a unit this system counts in", ErrInvalid, e.Unit)
	case e.CostMinor != nil && *e.CostMinor < 0, e.PriceMinor != nil && *e.PriceMinor < 0:
		return "", fmt.Errorf("%w: a price cannot be below nothing", ErrInvalid)
	case e.MinimumMilli < 0:
		return "", fmt.Errorf("%w: a minimum cannot be below nothing", ErrInvalid)
	case len(e.Codes) > maxCodesPerPart:
		return "", fmt.Errorf("%w: %d codes is more than %d", ErrInvalid, len(e.Codes), maxCodesPerPart)
	}
	if e.ID != "" && !looksLikeUUID(e.ID) {
		return "", ErrNotFound
	}
	codes := append([]string{e.Number}, e.Codes...)

	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		// A scan finds a part by its number or by any code, so neither may
		// already belong to another part: the scanner would find whichever
		// came first, which is a wrong part with no warning.
		var clash, owner string
		err := tx.QueryRow(ctx, `
			SELECT x.code, p.number || ' ' || p.name
			FROM (SELECT number AS code, id FROM parts
			      UNION ALL SELECT code, part_id FROM part_codes) x
			JOIN parts p ON p.id = x.id
			WHERE x.code = ANY($1) AND x.id IS DISTINCT FROM nullif($2, '')::uuid
			LIMIT 1`, codes, e.ID).Scan(&clash, &owner)
		if err == nil {
			return fmt.Errorf("%w: %s is already %s", ErrInvalid, clash, owner)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check codes: %w", err)
		}

		if e.ID == "" {
			err = tx.QueryRow(ctx, `
				INSERT INTO parts (shop_id, number, name, unit, location, cost_minor, price_minor, minimum_quantity)
				VALUES ($1, $2, $3, $4, nullif($5, ''), $6, $7, $8::numeric / 1000)
				RETURNING id`,
				scope.ShopID, e.Number, e.Name, e.Unit, e.Location, e.CostMinor, e.PriceMinor, e.MinimumMilli).Scan(&e.ID)
			if err != nil {
				return fmt.Errorf("add part: %w", err)
			}
		} else {
			tag, err := tx.Exec(ctx, `
				UPDATE parts SET number = $2, name = $3, unit = $4, location = nullif($5, ''),
				       cost_minor = $6, price_minor = $7, minimum_quantity = $8::numeric / 1000,
				       updated_at = now()
				WHERE id = $1`,
				e.ID, e.Number, e.Name, e.Unit, e.Location, e.CostMinor, e.PriceMinor, e.MinimumMilli)
			if err != nil {
				return fmt.Errorf("change part: %w", err)
			}
			if tag.RowsAffected() == 0 {
				return ErrNotFound
			}
			if _, err := tx.Exec(ctx, `DELETE FROM part_codes WHERE part_id = $1`, e.ID); err != nil {
				return fmt.Errorf("clear codes: %w", err)
			}
		}
		for _, c := range e.Codes {
			if c == e.Number {
				continue // the number is found already
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO part_codes (shop_id, part_id, code) VALUES ($1, $2, $3)`,
				scope.ShopID, e.ID, c); err != nil {
				return fmt.Errorf("add code: %w", err)
			}
		}
		return nil
	})
	// Two people adding the same number at once: the check above saw
	// nothing for both, and the unique index decides.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return "", fmt.Errorf("%w: that number or code was just given to another part", ErrInvalid)
	}
	if err != nil {
		return "", err
	}
	return e.ID, nil
}

// SetPartActive retires a part or brings one back.
//
// Retired, never deleted: the ledger and old jobs point at it. A retired part
// is not listed, offered on a job or found by the scanner.
func SetPartActive(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string, active bool) error {
	if !scope.Role.SeesParts() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return ErrNotFound
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE parts SET active = $2, updated_at = now() WHERE id = $1`, id, active)
		if err != nil {
			return fmt.Errorf("set part active: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

func knownUnit(u string) bool {
	for _, k := range PartUnits {
		if u == k {
			return true
		}
	}
	return false
}
