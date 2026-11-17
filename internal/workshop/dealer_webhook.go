package workshop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// The stock feed's webhook (#183). A shop may give an address that is called
// when its stock changes. The call carries no car and no price -- only that
// something changed -- so a site that is not who it seems learns nothing, and
// one that is asks the feed (with its token, and changes-since) for what.
//
// A change is any car entering or leaving the feed, a new listing, or a
// photograph added, removed or moved to the front. Changes are coalesced into
// windows of webhookWindow: one call per window that had any, made when the
// window has closed, so everything in it is covered and a person uploading 80
// photographs makes one call, not 80.
const webhookWindow = 10 * time.Second

const kindStockWebhook = "stock_webhook"

// ErrWebhookURL is an address the webhook will not call.
var ErrWebhookURL = fmt.Errorf("%w: a webhook is an https address", ErrInvalid)

// SetFeedWebhook sets the address called when stock changes; empty stops the
// calls. Whoever runs the shop.
func SetFeedWebhook(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, raw string) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if err := scope.Require(access.Dealer); err != nil {
		return err
	}
	raw = strings.TrimSpace(raw)
	if raw != "" {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || len(raw) > 500 {
			return ErrWebhookURL
		}
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO stock_feed_settings (shop_id, published, open, token_sha256, include_reserved, webhook_url, changed_by)
			SELECT $1, published, open, token_sha256, include_reserved, nullif($2, ''), NULLIF($3, '')::uuid
			FROM stock_feed_settings ORDER BY changed_at DESC, id DESC LIMIT 1`, scope.ShopID, raw, scope.UserID)
		if err != nil {
			return err
		}
		// A shop that had no settings yet has nothing to carry: the feed
		// stays unpublished until it is set up, and the address waits.
		_, err = tx.Exec(ctx, `
			INSERT INTO stock_feed_settings (shop_id, published, open, include_reserved, webhook_url, changed_by)
			SELECT $1, false, false, false, $2, NULLIF($3, '')::uuid
			WHERE $2 <> '' AND NOT EXISTS (SELECT 1 FROM stock_feed_settings)`, scope.ShopID, raw, scope.UserID)
		return err
	})
}

// FeedWebhookOf reads the address, for whoever runs the shop.
func FeedWebhookOf(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) (string, error) {
	if !scope.Role.RunsTheShop() {
		return "", access.ErrForbidden
	}
	var out string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT coalesce(webhook_url, '') FROM stock_feed_settings ORDER BY changed_at DESC, id DESC LIMIT 1`).Scan(&out)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return out, err
}

// stockChangedTx asks for the shop's site to be told, in the transaction of
// the change: rolled back, nobody is told of what did not happen. A shop with
// no address queues nothing.
func stockChangedTx(ctx context.Context, tx pgx.Tx, scope access.Scope) error {
	var has bool
	err := tx.QueryRow(ctx, `SELECT coalesce(webhook_url, '') <> '' FROM stock_feed_settings ORDER BY changed_at DESC, id DESC LIMIT 1`).Scan(&has)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !has) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the webhook: %w", err)
	}
	return queueStockPing(ctx, tx, scope, time.Now())
}

// queueStockPing queues the call for the window now falls in. The window's
// start is the key, so every change in it is the same call, and it is made
// when the window has closed.
func queueStockPing(ctx context.Context, tx pgx.Tx, scope access.Scope, now time.Time) error {
	start := now.UTC().Truncate(webhookWindow)
	_, err := enqueueTx(ctx, tx, scope, NewOutboxItem{
		Kind: kindStockWebhook, Module: access.Dealer,
		Key:       "stock@" + start.Format(time.RFC3339),
		Payload:   map[string]any{"window_start": start.Format(time.RFC3339)},
		NotBefore: start.Add(webhookWindow),
	})
	return err
}

// StockWebhookAdapter makes the call. The address is read when it sends, so
// one changed since a call was queued is the one called, and one removed is
// not called at all.
type StockWebhookAdapter struct {
	Pool   *pgxpool.Pool
	Client *http.Client
}

// NewStockWebhookAdapter is the adapter with a client that will not follow a
// redirect and will not connect to an address inside the host's own network:
// the address is a shop's to give, and a server that calls whatever it is
// given is a way into whatever the server can reach.
func NewStockWebhookAdapter(pool *pgxpool.Pool) StockWebhookAdapter {
	dialer := &net.Dialer{
		Timeout: 5 * time.Second,
		// Checked on the address actually connected to, after the name has
		// been resolved, so a name that resolves to a private address is
		// refused as well as a private address written out.
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip == nil || !publicAddress(ip) {
				return errNotPublic
			}
			return nil
		},
	}
	return StockWebhookAdapter{Pool: pool, Client: &http.Client{
		Timeout:       10 * time.Second,
		Transport:     &http.Transport{DialContext: dialer.DialContext, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

var errNotPublic = errors.New("the address is not a public one")

func publicAddress(ip net.IP) bool {
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast())
}

// Send calls the shop's address. A refusal that a retry will not change
// (the address is not allowed, or the site says 4xx other than "slow down")
// is permanent; the site being down is retried. Nothing the error says
// carries the address, which may have a secret in its path.
func (a StockWebhookAdapter) Send(ctx context.Context, it OutboxItem) error {
	var address string
	err := database.InShop(ctx, a.Pool, it.ShopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT coalesce(webhook_url, '') FROM stock_feed_settings ORDER BY changed_at DESC, id DESC LIMIT 1`).Scan(&address)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		address = ""
	} else if err != nil {
		return err
	}
	if address == "" {
		return fmt.Errorf("%w: the shop has no webhook address now", ErrPermanent)
	}
	body, _ := json.Marshal(map[string]any{"event": "stock.changed", "since": it.Payload["window_start"]})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: the webhook address cannot be called", ErrPermanent)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", it.Key)
	req.Header.Set("User-Agent", "FreeSMS-webhook")
	resp, err := a.Client.Do(req)
	if err != nil {
		if errors.Is(err, errNotPublic) {
			return fmt.Errorf("%w: the webhook address is not a public one", ErrPermanent)
		}
		// A url.Error holds the address; say only that it failed.
		return errors.New("the call to the webhook failed")
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode/100 == 2:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode >= 500:
		return fmt.Errorf("the webhook answered %d", resp.StatusCode)
	default:
		return fmt.Errorf("%w: the webhook answered %d", ErrPermanent, resp.StatusCode)
	}
}
