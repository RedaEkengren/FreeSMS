package workshop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
)

// The outbox (#154): every call to the outside world is a row, written in
// the transaction of the change that caused it, and sent afterwards by a
// worker with the row's own idempotency key -- retried with back-off, given
// up after a number of attempts, and in front of whoever runs the shop when
// it keeps failing. Every kind has a manual variant, a task for a person,
// so no flow waits on a provider. The workshop's customer messages keep
// their own (0040).

// OutboxItem is one row, as an adapter is handed it.
type OutboxItem struct {
	ID          string
	ShopID      string
	Kind        string
	SubjectKind string
	SubjectID   string
	// The same on every attempt: a provider that took the first and timed
	// out before answering sees the retry as the same request.
	Key      string
	Payload  map[string]any
	Attempts int
}

// OutboxAdapter sends one kind of row. An error is a failure to retry; one
// wrapping ErrPermanent is not retried; ErrManualTask makes it a task for a
// person. An error must carry no key or token; the outbox strips what it
// can recognise as well.
type OutboxAdapter interface {
	Send(ctx context.Context, item OutboxItem) error
}

// ErrPermanent is a provider's refusal that a retry will not change.
var ErrPermanent = errors.New("workshop: the provider refused, and will again")

// ErrManualTask is an effect done by a person, not by a provider.
var ErrManualTask = errors.New("workshop: this is done by hand")

// ManualAdapter is the variant every kind has: it sends nothing and turns
// the row into a task for a person, who does it and says so.
type ManualAdapter struct{}

// Send makes it a task.
func (ManualAdapter) Send(context.Context, OutboxItem) error { return ErrManualTask }

// outboxKinds are the kinds of call there are.
var outboxKinds = map[string]bool{
	"signature_request": true,
	"vehicle_lookup":    true,
}

// NewOutboxItem is a call to make. Key is what makes it once: the same
// kind with the same key is the same call.
type NewOutboxItem struct {
	Kind string
	// The module whose business it is, if any: switched off, the call is
	// cancelled if it has not been made.
	Module      access.Module
	SubjectKind string
	SubjectID   string
	Key         string
	Payload     map[string]any
	NotBefore   time.Time
	MaxAttempts int
}

// enqueueTx writes a call to make, in the caller's transaction: rolled
// back, it was never asked for. The same kind and key again is the call
// already there -- or, when that one was cancelled, the same call asked for
// again: queued anew on the same row, with its attempts starting again and
// the cancellation kept in its history.
func enqueueTx(ctx context.Context, tx pgx.Tx, scope access.Scope, n NewOutboxItem) (string, error) {
	if !outboxKinds[n.Kind] {
		return "", fmt.Errorf("outbox: %q is not a kind of call", n.Kind)
	}
	if n.Key == "" || len(n.Key) > 200 {
		return "", fmt.Errorf("outbox: a call needs its key")
	}
	if n.MaxAttempts == 0 {
		n.MaxAttempts = 8
	}
	payload, err := json.Marshal(orEmpty(n.Payload))
	if err != nil {
		return "", err
	}
	var notBefore any
	if !n.NotBefore.IsZero() {
		notBefore = n.NotBefore
	}
	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO outbox (shop_id, kind, module, subject_kind, subject_id, payload, idempotency_key, not_before, max_attempts, created_by)
		VALUES ($1, $2, nullif($3, ''), nullif($4, ''), NULLIF($5, '')::uuid, $6, $7, coalesce($8, clock_timestamp()), $9, NULLIF($10, '')::uuid)
		ON CONFLICT (shop_id, kind, idempotency_key) DO NOTHING RETURNING id`,
		scope.ShopID, n.Kind, string(n.Module), n.SubjectKind, n.SubjectID, payload, n.Key, notBefore, n.MaxAttempts, scope.UserID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.QueryRow(ctx, `SELECT id FROM outbox WHERE kind = $1 AND idempotency_key = $2`, n.Kind, n.Key).Scan(&id); err != nil {
			return "", err
		}
		state, err := outboxStateTx(ctx, tx, id)
		if err != nil || state != "cancelled" {
			return id, err
		}
		if _, err := tx.Exec(ctx, `UPDATE outbox SET claimed_until = NULL WHERE id = $1`, id); err != nil {
			return "", err
		}
		return id, outboxEventTx(ctx, tx, scope.ShopID, id, "queued", nil, "asked for again", scope.UserID)
	}
	if err != nil {
		return "", fmt.Errorf("queue the call: %w", err)
	}
	if err := outboxEventTx(ctx, tx, scope.ShopID, id, "queued", nil, "", scope.UserID); err != nil {
		return "", err
	}
	return id, nil
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func outboxEventTx(ctx context.Context, tx pgx.Tx, shopID, id, event string, retryAt *time.Time, detail, by string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO outbox_events (shop_id, outbox_id, event, retry_at, detail, by_user)
		VALUES ($1, $2, $3, $4, nullif($5, ''), NULLIF($6, '')::uuid)`, shopID, id, event, retryAt, detail, by)
	if err != nil {
		return fmt.Errorf("record the call's %s: %w", event, err)
	}
	return nil
}

// secretish is what a provider's error can carry that is not the shop's to
// keep: a URL's password, its query (where tokens travel), a bearer token, a
// key=value of something that sounds like one.
var secretish = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(https?://)[^/\s:@]+:[^/\s@]+@`),
	regexp.MustCompile(`(?i)(https?://[^\s?]+)\?[^\s]*`),
	regexp.MustCompile(`(?i)(bearer|basic)\s+[A-Za-z0-9._~+/=-]+`),
	regexp.MustCompile(`(?i)((?:api[_-]?key|token|secret|password|passwd|signature)["']?\s*[:=]\s*["']?)[^\s"'&,;]+`),
}

// redactError is a provider's error as it is stored: without what looks
// like a secret, and short.
func redactError(err error) string {
	s := err.Error()
	s = secretish[0].ReplaceAllString(s, "${1}…@")
	s = secretish[1].ReplaceAllString(s, "${1}?…")
	s = secretish[2].ReplaceAllString(s, "$1 …")
	s = secretish[3].ReplaceAllString(s, "${1}…")
	if utf8.RuneCountInString(s) > 500 {
		s = string([]rune(s)[:499]) + "…"
	}
	return s
}

// outboxLease is how long a worker holds a row it is sending: a send that
// takes longer is taken for dead, and the row goes again with the same key.
const outboxLease = 5 * time.Minute

// outboxBackoff is how long after the nth failed attempt the next is: a
// minute, doubling, at most six hours.
func outboxBackoff(attempt int) time.Duration {
	d := time.Minute
	for i := 1; i < attempt && d < 6*time.Hour; i++ {
		d *= 2
	}
	return min(d, 6*time.Hour)
}

// OutboxRun says what one pass of the worker did.
type OutboxRun struct {
	Sent, Retried, GaveUp, Manual int
}

// RunOutbox sends what is due in a shop, at most limit rows, each through
// its kind's adapter -- the manual one where none is given. Run by the
// worker, as the system: no person is behind it.
//
// One row at a time: claimed, sent, and its outcome written, before the
// next is claimed. The lease is that one row's, so a slow provider never
// lets the lease on rows still waiting in this worker's memory run out
// while another worker takes them. The lease is the claim's own time: the
// worker checks it still holds it, and that the call was not cancelled,
// before it sends and again when it writes what came of it. Two workers
// never send the same row while a lease runs, and a worker whose lease ran
// out writes nothing over the one that took the row after it.
func RunOutbox(ctx context.Context, pool *pgxpool.Pool, shopID string, adapters map[string]OutboxAdapter, limit int) (OutboxRun, error) {
	var run OutboxRun
	for range limit {
		it, lease, ok, err := claimOutboxTx(ctx, pool, shopID)
		if err != nil || !ok {
			return run, err
		}
		if err := sendOutbox(ctx, pool, shopID, adapters, it, lease, &run); err != nil {
			return run, err
		}
	}
	return run, nil
}

// claimOutboxTx claims the next row due, skipping what another worker
// holds, and returns the lease it took.
func claimOutboxTx(ctx context.Context, pool *pgxpool.Pool, shopID string) (OutboxItem, time.Time, bool, error) {
	it := OutboxItem{ShopID: shopID}
	var lease time.Time
	found := false
	err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			WITH due AS (
			  SELECT id, attempts FROM outbox_state
			  WHERE state IN ('queued', 'retry_scheduled', 'attempted') AND due_at <= clock_timestamp()
			  ORDER BY due_at)
			SELECT o.id::text, o.kind, coalesce(o.subject_kind, ''), coalesce(o.subject_id::text, ''), o.idempotency_key, o.payload, due.attempts
			FROM outbox o JOIN due ON due.id = o.id
			WHERE o.claimed_until IS NULL OR o.claimed_until <= clock_timestamp()
			LIMIT 1
			FOR UPDATE OF o SKIP LOCKED`).Scan(&it.ID, &it.Kind, &it.SubjectKind, &it.SubjectID, &it.Key, &it.Payload, &it.Attempts)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("claim a call: %w", err)
		}
		it.Attempts++
		if err := tx.QueryRow(ctx, `UPDATE outbox SET claimed_until = clock_timestamp() + $2::interval WHERE id = $1 RETURNING claimed_until`,
			it.ID, fmt.Sprintf("%d seconds", int(outboxLease.Seconds()))).Scan(&lease); err != nil {
			return fmt.Errorf("claim a call: %w", err)
		}
		found = true
		return outboxEventTx(ctx, tx, shopID, it.ID, "attempted", nil, "", "")
	})
	return it, lease, found, err
}

// outboxHeldTx locks a claimed row and reports whether lease is still this
// worker's -- no other worker has claimed it since -- whether it has not
// yet run out, and the row's state, read after the lock so a cancel
// committed while waiting for it is seen.
func outboxHeldTx(ctx context.Context, tx pgx.Tx, id string, lease time.Time) (ours, live bool, state string, max int, err error) {
	if err := tx.QueryRow(ctx, `
		SELECT coalesce(claimed_until = $2, false), coalesce(claimed_until = $2 AND claimed_until > clock_timestamp(), false), max_attempts
		FROM outbox WHERE id = $1 FOR UPDATE`, id, lease).Scan(&ours, &live, &max); err != nil {
		return false, false, "", 0, fmt.Errorf("read the call: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT state FROM outbox_state WHERE id = $1`, id).Scan(&state); err != nil {
		return false, false, "", 0, fmt.Errorf("read the call: %w", err)
	}
	return ours, live, state, max, nil
}

// sendOutbox sends one claimed row and writes what came of it.
func sendOutbox(ctx context.Context, pool *pgxpool.Pool, shopID string, adapters map[string]OutboxAdapter, it OutboxItem, lease time.Time, run *OutboxRun) error {
	// Before sending: still ours, and not cancelled since it was claimed.
	var send bool
	err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		_, live, state, _, err := outboxHeldTx(ctx, tx, it.ID, lease)
		if err != nil || !live {
			return err
		}
		if state != "attempted" {
			_, err := tx.Exec(ctx, `UPDATE outbox SET claimed_until = NULL WHERE id = $1`, it.ID)
			return err
		}
		send = true
		return nil
	})
	if err != nil || !send {
		return err
	}
	var a OutboxAdapter = ManualAdapter{}
	if given, ok := adapters[it.Kind]; ok && given != nil {
		a = given
	}
	sendCtx, cancel := context.WithTimeout(ctx, outboxLease/2)
	sendErr := a.Send(sendCtx, it)
	cancel()
	err = database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		ours, _, state, max, err := outboxHeldTx(ctx, tx, it.ID, lease)
		if err != nil {
			return err
		}
		if !ours {
			// Another worker took the row after this lease ran out: what
			// happens to it is that worker's to write.
			return nil
		}
		// The attempt is over, whatever came of it: the lease goes.
		if _, err := tx.Exec(ctx, `UPDATE outbox SET claimed_until = NULL WHERE id = $1`, it.ID); err != nil {
			return err
		}
		if state != "attempted" {
			// Cancelled while it was being sent. A failure is not retried;
			// a call that went through did, and the record says so.
			if sendErr == nil {
				run.Sent++
				return outboxEventTx(ctx, tx, shopID, it.ID, "succeeded", nil, "made before the cancellation reached it", "")
			}
			return nil
		}
		switch {
		case sendErr == nil:
			run.Sent++
			return outboxEventTx(ctx, tx, shopID, it.ID, "succeeded", nil, "", "")
		case errors.Is(sendErr, ErrManualTask):
			run.Manual++
			return outboxEventTx(ctx, tx, shopID, it.ID, "manual_task", nil, "", "")
		}
		detail := redactError(sendErr)
		if err := outboxEventTx(ctx, tx, shopID, it.ID, "failed", nil, detail, ""); err != nil {
			return err
		}
		if errors.Is(sendErr, ErrPermanent) || it.Attempts >= max {
			run.GaveUp++
			return outboxEventTx(ctx, tx, shopID, it.ID, "given_up", nil, detail, "")
		}
		run.Retried++
		at := time.Now().Add(outboxBackoff(it.Attempts))
		return outboxEventTx(ctx, tx, shopID, it.ID, "retry_scheduled", &at, "", "")
	})
	if err != nil {
		return fmt.Errorf("record the call's outcome: %w", err)
	}
	return nil
}

// OutboxEntry is a call as whoever runs the shop sees it.
type OutboxEntry struct {
	ID          string
	Kind        string
	SubjectKind string
	SubjectID   string
	State       string
	At          time.Time
	Detail      string
	Attempts    int
	CreatedAt   time.Time
}

// OutboxNeedingAttention lists the calls a person has to look at: given up
// after their attempts, or a task to do by hand. Whoever runs the shop.
func OutboxNeedingAttention(ctx context.Context, pool *pgxpool.Pool, scope access.Scope) ([]OutboxEntry, error) {
	if !scope.Role.RunsTheShop() {
		return nil, access.ErrForbidden
	}
	var out []OutboxEntry
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id::text, kind, coalesce(subject_kind, ''), coalesce(subject_id::text, ''), state, state_at, coalesce(detail, ''),
			       attempts, created_at
			FROM outbox_state WHERE state IN ('given_up', 'manual_task') ORDER BY state_at, id`)
		if err != nil {
			return fmt.Errorf("read the outbox: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var e OutboxEntry
			if err := rows.Scan(&e.ID, &e.Kind, &e.SubjectKind, &e.SubjectID, &e.State, &e.At, &e.Detail, &e.Attempts, &e.CreatedAt); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

var (
	errOutboxNotStuck = fmt.Errorf("%w: that call is not waiting for anybody", ErrInvalid)
	errOutboxDone     = fmt.Errorf("%w: that call is already done or cancelled", ErrInvalid)
	// ErrOutboxCancelNoReason is cancelling a call without saying why.
	ErrOutboxCancelNoReason = fmt.Errorf("%w: say why the call is cancelled, in 300 characters or fewer", ErrInvalid)
)

// outboxStateTx locks a row and reads where it stands. The state is read
// after the lock, in a statement of its own, so it includes whatever the
// worker holding the lock wrote before it let go.
func outboxStateTx(ctx context.Context, tx pgx.Tx, id string) (string, error) {
	if err := tx.QueryRow(ctx, `SELECT id FROM outbox WHERE id = $1 FOR UPDATE`, id).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	var state string
	err := tx.QueryRow(ctx, `SELECT state FROM outbox_state WHERE id = $1`, id).Scan(&state)
	return state, err
}

// RetryOutbox sends a call again that was given up or left as a task: its
// attempts start again, with the same key. Whoever runs the shop.
func RetryOutbox(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id string) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return ErrNotFound
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		state, err := outboxStateTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if state != "given_up" && state != "manual_task" {
			return errOutboxNotStuck
		}
		if _, err := tx.Exec(ctx, `UPDATE outbox SET claimed_until = NULL WHERE id = $1`, id); err != nil {
			return err
		}
		return outboxEventTx(ctx, tx, scope.ShopID, id, "queued", nil, "", scope.UserID)
	})
}

// FinishOutboxByHand says a task for a person has been done.
func FinishOutboxByHand(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id, note string) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return ErrNotFound
	}
	note, err := checkNote(note)
	if err != nil {
		return err
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		state, err := outboxStateTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if state != "manual_task" && state != "given_up" {
			return errOutboxNotStuck
		}
		return outboxEventTx(ctx, tx, scope.ShopID, id, "done_by_hand", nil, note, scope.UserID)
	})
}

// CancelOutbox cancels a call not yet made, with why. Whoever runs the shop.
func CancelOutbox(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, id, reason string) error {
	if !scope.Role.RunsTheShop() {
		return access.ErrForbidden
	}
	if !looksLikeUUID(id) {
		return ErrNotFound
	}
	reason, err := checkReason(reason, ErrOutboxCancelNoReason)
	if err != nil {
		return err
	}
	return database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		state, err := outboxStateTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if state == "succeeded" || state == "cancelled" || state == "done_by_hand" {
			return errOutboxDone
		}
		return outboxEventTx(ctx, tx, scope.ShopID, id, "cancelled", nil, reason, scope.UserID)
	})
}

// cancelOutboxTx cancels every call not yet made that where says -- a
// condition on outbox_state s, with arg as $1 -- with the reason, as the
// system on behalf of the person whose change it was.
//
// The rows are locked first, as a worker writing an outcome locks its row,
// so a cancel and an outcome are one after the other and never interleave;
// which are still to cancel is read after the lock.
func cancelOutboxTx(ctx context.Context, tx pgx.Tx, shopID, by, reason, where string, arg any) error {
	const open = `s.state IN ('queued', 'retry_scheduled', 'attempted', 'failed', 'manual_task', 'given_up')`
	rows, err := tx.Query(ctx, `
		SELECT o.id::text FROM outbox o WHERE o.id IN (SELECT s.id FROM outbox_state s WHERE `+open+` AND `+where+`)
		ORDER BY o.id FOR UPDATE OF o`, arg)
	if err != nil {
		return fmt.Errorf("cancel the calls: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("cancel the calls: %w", err)
	}
	if len(ids) == 0 {
		return nil
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO outbox_events (shop_id, outbox_id, event, detail, by_user)
		SELECT $1, s.id, 'cancelled', $2, NULLIF($3, '')::uuid FROM outbox_state s
		WHERE `+open+` AND s.id = ANY($4::uuid[])`, shopID, reason, by, ids)
	if err != nil {
		return fmt.Errorf("cancel the calls: %w", err)
	}
	return nil
}

// cancelModuleOutboxTx cancels the calls of module m not yet made, as it is
// switched off, on behalf of whoever switched it.
func cancelModuleOutboxTx(ctx context.Context, tx pgx.Tx, scope access.Scope, m access.Module) error {
	return cancelOutboxTx(ctx, tx, scope.ShopID, scope.UserID, "the module was switched off",
		`s.id IN (SELECT id FROM outbox WHERE module = $1)`, string(m))
}

// cancelPersonOutboxTx cancels the calls not yet made about a person being
// erased: their customer, their party, a contract naming them.
func cancelPersonOutboxTx(ctx context.Context, tx pgx.Tx, scope access.Scope, personID string) error {
	return cancelOutboxTx(ctx, tx, scope.ShopID, scope.UserID, "the person was erased", `s.subject_id IN (
		SELECT id FROM customers WHERE person_id = $1
		UNION ALL SELECT pa.id FROM parties pa LEFT JOIN customers c ON c.id = pa.customer_id WHERE pa.person_id = $1 OR c.person_id = $1
		UNION ALL SELECT dp.document_id FROM document_record_parties dp JOIN parties pa ON pa.id = dp.party_id
		          LEFT JOIN customers c ON c.id = pa.customer_id WHERE pa.person_id = $1 OR c.person_id = $1)`, personID)
}

// QueueSignatureRequest asks for a party's signature on a document through
// the outbox: written now, sent by the worker through the signing service,
// or left as a task to sign on paper. The document stays unsigned until a
// signature is recorded, and nothing else waits on it (#150). Repeated, it
// is the same request.
func QueueSignatureRequest(ctx context.Context, pool *pgxpool.Pool, scope access.Scope, documentID, documentPartyID string) (string, error) {
	if !looksLikeUUID(documentID) || !looksLikeUUID(documentPartyID) {
		return "", ErrNotFound
	}
	var id string
	err := database.InScope(ctx, pool, scope, func(ctx context.Context, tx pgx.Tx) error {
		var kind string
		err := tx.QueryRow(ctx, `SELECT kind FROM document_records WHERE id = $1`, documentID).Scan(&kind)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read document: %w", err)
		}
		if err := mayDocument(scope.Role, scope, kind, true); err != nil {
			return err
		}
		var signed, superseded bool
		err = tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM document_signatures WHERE document_party_id = p.id),
			       EXISTS (SELECT 1 FROM document_records WHERE supersedes_id = p.document_id)
			FROM document_record_parties p WHERE p.id = $1 AND p.document_id = $2`, documentPartyID, documentID).Scan(&signed, &superseded)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		switch {
		case err != nil:
			return fmt.Errorf("read the party: %w", err)
		case superseded:
			return errDocSuperseded
		case signed:
			return errDocSigned
		}
		module := access.Dealer
		if kind == DocRentalAgreement {
			module = access.Rental
		}
		id, err = enqueueTx(ctx, tx, scope, NewOutboxItem{Kind: "signature_request", Module: module, SubjectKind: "document", SubjectID: documentID,
			Key: "sign:" + strings.ToLower(documentPartyID), Payload: map[string]any{"document_party_id": strings.ToLower(documentPartyID)}})
		return err
	})
	return id, err
}

// SignerAdapter sends signature requests through a Signer. The manual
// signer asks nobody, so its requests become tasks: print it, have it
// signed, record it.
type SignerAdapter struct {
	Pool   *pgxpool.Pool
	Signer Signer
}

// Send reads the document as it is now and asks for the signature.
func (a SignerAdapter) Send(ctx context.Context, it OutboxItem) error {
	if _, manual := a.Signer.(ManualSigner); manual || a.Signer == nil {
		return ErrManualTask
	}
	partyID, _ := it.Payload["document_party_id"].(string)
	var d DocumentRecord
	if err := database.InShop(ctx, a.Pool, it.ShopID, func(ctx context.Context, tx pgx.Tx) error {
		return documentRecordTx(ctx, tx, it.SubjectID, &d)
	}); err != nil {
		return err
	}
	for _, p := range d.Parties {
		if p.ID == partyID {
			if p.Signature != nil || d.SupersededByID != "" {
				return nil // nothing left to ask for
			}
			_, err := a.Signer.Request(ctx, d, p, it.Key)
			return err
		}
	}
	// The party is no longer on the document: nothing to ask for, ever.
	return ErrPermanent
}

// KindLabel is the kind of call as a page says it, a catalogue key.
func (e OutboxEntry) KindLabel() string {
	switch e.Kind {
	case "signature_request":
		return "A signature asked for"
	case "vehicle_lookup":
		return "A registration looked up"
	}
	return "A call to another service"
}

// StateLabel is where it stands, a catalogue key.
func (e OutboxEntry) StateLabel() string {
	if e.State == "manual_task" {
		return "To do by hand"
	}
	return "Given up"
}
