package workshop_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// The outbox (#154). The shop runs every module by its scope.

// failingSigner is a signing service that is down, and counts how often it
// was asked and with which keys.
type failingSigner struct {
	calls atomic.Int32
	err   error
}

func (s *failingSigner) Request(context.Context, workshop.DocumentRecord, workshop.DocumentParty, string) (string, error) {
	s.calls.Add(1)
	return "", s.err
}

// countingAdapter succeeds, and remembers each key it was handed.
type countingAdapter struct {
	mu   sync.Mutex
	keys []string
}

func (a *countingAdapter) Send(_ context.Context, it workshop.OutboxItem) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.keys = append(a.keys, it.Key)
	return nil
}

func callState(t *testing.T, id string, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) (state string, attempts int) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `SELECT state, attempts FROM outbox_state WHERE id = $1`, id).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	return state, attempts
}

// With the signing service down, a request is retried with the same key
// and given up after its attempts; the document stays unsigned, nothing
// else waits on it, and whoever runs the shop sees it with what went
// wrong -- but not the service's token.
func TestASigningServiceDownLeavesTheDocumentUnsignedAndTheRequestInFrontOfTheOwner(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	tpl := purchaseTemplate(t, pool)
	doc := purchaseAgreement(t, pool, tpl, "")
	d, _ := workshop.DocumentRecordByID(ctx, pool, buyerScope(), doc)
	id, err := workshop.QueueSignatureRequest(ctx, pool, buyerScope(), doc, d.Parties[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := workshop.QueueSignatureRequest(ctx, pool, buyerScope(), doc, d.Parties[0].ID); err != nil || again != id {
		t.Errorf("the same request again: %s, %v; want %s", again, err, id)
	}
	if _, err := pool.Exec(ctx, `UPDATE outbox SET max_attempts = 2 WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	signer := &failingSigner{err: errors.New("POST https://broker:hunter2@sign.example/v1/requests?token=abc123: 503 Service Unavailable")}
	adapters := map[string]workshop.OutboxAdapter{"signature_request": workshop.SignerAdapter{Pool: pool, Signer: signer}}
	if run, err := workshop.RunOutbox(ctx, pool, shopID, adapters, 10); err != nil || run.Retried != 1 {
		t.Fatalf("first pass: %+v, %v", run, err)
	}
	if state, attempts := callState(t, id, pool); state != "retry_scheduled" || attempts != 1 {
		t.Errorf("after the first attempt: %s, %d", state, attempts)
	}
	// Not due yet: nothing is sent.
	if run, _ := workshop.RunOutbox(ctx, pool, shopID, adapters, 10); run != (workshop.OutboxRun{}) {
		t.Errorf("a retry before its time: %+v", run)
	}
	// Its time comes: as the database's owner, which the application is not.
	if _, err := pool.Exec(ctx, `UPDATE outbox_events SET retry_at = now() - interval '1 second' WHERE outbox_id = $1 AND event = 'retry_scheduled'`, id); err != nil {
		t.Fatal(err)
	}
	if run, err := workshop.RunOutbox(ctx, pool, shopID, adapters, 10); err != nil || run.GaveUp != 1 {
		t.Fatalf("second pass: %+v, %v", run, err)
	}
	if signer.calls.Load() != 2 {
		t.Errorf("the service was asked %d times, want 2", signer.calls.Load())
	}
	if d, _ := workshop.DocumentRecordByID(ctx, pool, buyerScope(), doc); d.Signing() != "unsigned" {
		t.Errorf("the document is %s", d.Signing())
	}
	stuck, err := workshop.OutboxNeedingAttention(ctx, pool, owner())
	if err != nil || len(stuck) != 1 || stuck[0].State != "given_up" {
		t.Fatalf("the owner's list: %+v, %v", stuck, err)
	}
	if strings.Contains(stuck[0].Detail, "hunter2") || strings.Contains(stuck[0].Detail, "abc123") || !strings.Contains(stuck[0].Detail, "503") {
		t.Errorf("what went wrong, as kept: %q", stuck[0].Detail)
	}
	if _, err := workshop.OutboxNeedingAttention(ctx, pool, buyerScope()); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("the buyer read the outbox: %v", err)
	}
	// Retried by a person, it goes again; signed on paper meanwhile, the
	// retry asks nobody.
	if err := workshop.RetryOutbox(ctx, pool, owner(), id); err != nil {
		t.Fatal(err)
	}
	if err := workshop.RecordSignature(ctx, pool, buyerScope(), doc, d.Parties[0].ID, workshop.SignatureEvidence{
		Method: workshop.SignedOnPaper, Hash: d.Hash, WitnessedBy: "Bo"}); err != nil {
		t.Fatal(err)
	}
	if run, err := workshop.RunOutbox(ctx, pool, shopID, adapters, 10); err != nil || run.Sent != 1 || signer.calls.Load() != 2 {
		t.Errorf("after the retry: %+v, %v, asked %d", run, err, signer.calls.Load())
	}
}

// A change rolled back leaves no call; a call is made with its key, by one
// worker however many run at once.
func TestTwoWorkersNeverSendTheSameCall(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	tpl := purchaseTemplate(t, pool)
	doc := purchaseAgreement(t, pool, tpl, "")
	d, _ := workshop.DocumentRecordByID(ctx, pool, buyerScope(), doc)
	for _, p := range d.Parties {
		if _, err := workshop.QueueSignatureRequest(ctx, pool, buyerScope(), doc, p.ID); err != nil {
			t.Fatal(err)
		}
	}
	// Refused -- a superseded document is not signed -- so nothing queued.
	purchaseAgreement(t, pool, tpl, doc)
	if _, err := workshop.QueueSignatureRequest(ctx, pool, buyerScope(), doc, d.Parties[0].ID); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("a superseded document's request: %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM outbox`); n != 2 {
		t.Errorf("%d calls queued, want 2", n)
	}
	a := &countingAdapter{}
	adapters := map[string]workshop.OutboxAdapter{"signature_request": a}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := workshop.RunOutbox(ctx, pool, shopID, adapters, 10); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(a.keys) != 2 || a.keys[0] == a.keys[1] {
		t.Errorf("sent with keys %v; want each of the two calls once", a.keys)
	}
}

// With no provider, the manual variant makes a task for a person, done by
// hand or cancelled with a reason; a module switched off cancels its
// pending calls; the application cannot rewrite what happened.
func TestACallWithNoProviderIsATaskForAPerson(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	tpl := purchaseTemplate(t, pool)
	doc := purchaseAgreement(t, pool, tpl, "")
	d, _ := workshop.DocumentRecordByID(ctx, pool, buyerScope(), doc)
	first, err := workshop.QueueSignatureRequest(ctx, pool, buyerScope(), doc, d.Parties[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := workshop.QueueSignatureRequest(ctx, pool, buyerScope(), doc, d.Parties[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if run, err := workshop.RunOutbox(ctx, pool, shopID, nil, 10); err != nil || run.Manual != 2 {
		t.Fatalf("with no provider: %+v, %v", run, err)
	}
	if err := workshop.FinishOutboxByHand(ctx, pool, owner(), first, "Undertecknat på papper"); err != nil {
		t.Fatal(err)
	}
	if err := workshop.CancelOutbox(ctx, pool, owner(), second, ""); !errors.Is(err, workshop.ErrOutboxCancelNoReason) {
		t.Errorf("cancelled with no reason: %v", err)
	}
	if err := workshop.CancelOutbox(ctx, pool, owner(), second, "Säljaren ångrade sig"); err != nil {
		t.Fatal(err)
	}
	if err := workshop.CancelOutbox(ctx, pool, owner(), second, "Igen"); !errors.Is(err, workshop.ErrInvalid) {
		t.Errorf("cancelled twice: %v", err)
	}
	if stuck, _ := workshop.OutboxNeedingAttention(ctx, pool, owner()); len(stuck) != 0 {
		t.Errorf("still in front of the owner: %+v", stuck)
	}
	// Car sales switched off with a request pending: cancelled, and why.
	fresh := purchaseAgreement(t, pool, tpl, doc)
	pending, err := workshop.QueueSignatureRequest(ctx, pool, buyerScope(), fresh, partyOnNewest(t, pool, fresh))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO shop_module_changes (shop_id, module, enabled, reason) VALUES ($1, 'dealer', true, 'test')`, shopID); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.SwitchModule(ctx, pool, owner(), access.Dealer, false, "Slutar sälja bilar", true); err != nil {
		t.Fatal(err)
	}
	if state, _ := callState(t, pending, pool); state != "cancelled" {
		t.Errorf("a car sale's request after car sales were switched off: %s", state)
	}
	for _, stmt := range []string{
		`DELETE FROM outbox`, `UPDATE outbox SET payload = '{}'`, `UPDATE outbox SET idempotency_key = 'x'`,
		`DELETE FROM outbox_events`, `UPDATE outbox_events SET event = 'succeeded'`,
	} {
		err := database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, stmt)
			return err
		})
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "42501" {
			t.Errorf("%q as the application: %v, want refused for want of the privilege", stmt, err)
		}
	}
}

// partyOnNewest is the first party on a document.
func partyOnNewest(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, doc string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `SELECT id::text FROM document_record_parties WHERE document_id = $1 ORDER BY position LIMIT 1`, doc).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// A worker that died mid-send leaves its row attempted and held: nobody
// sends it while the lease runs, and once it has run out it goes again with
// the same key, so a provider that took the first sees the same request.
func TestACallWhoseWorkerDiedGoesAgainWithTheSameKey(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	tpl := purchaseTemplate(t, pool)
	doc := purchaseAgreement(t, pool, tpl, "")
	d, _ := workshop.DocumentRecordByID(ctx, pool, buyerScope(), doc)
	id, err := workshop.QueueSignatureRequest(ctx, pool, buyerScope(), doc, d.Parties[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	// The dead worker's claim, as it left it: attempted, and held.
	if _, err := pool.Exec(ctx, `UPDATE outbox SET claimed_until = now() + interval '5 minutes' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO outbox_events (shop_id, outbox_id, event) VALUES ($1, $2, 'attempted')`, shopID, id); err != nil {
		t.Fatal(err)
	}
	a := &countingAdapter{}
	adapters := map[string]workshop.OutboxAdapter{"signature_request": a}
	if run, _ := workshop.RunOutbox(ctx, pool, shopID, adapters, 10); run.Sent != 0 {
		t.Errorf("sent while another worker held it: %+v", run)
	}
	if _, err := pool.Exec(ctx, `UPDATE outbox SET claimed_until = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if run, err := workshop.RunOutbox(ctx, pool, shopID, adapters, 10); err != nil || run.Sent != 1 {
		t.Fatalf("after the lease: %+v, %v", run, err)
	}
	if len(a.keys) != 1 || a.keys[0] != "sign:"+strings.ToLower(d.Parties[0].ID) {
		t.Errorf("sent with %v", a.keys)
	}
	if state, attempts := callState(t, id, pool); state != "succeeded" || attempts != 2 {
		t.Errorf("after: %s, %d attempts", state, attempts)
	}
}

// blockingAdapter holds each send until the test releases it with what
// the provider answered.
type blockingAdapter struct {
	started chan string
	release chan error
}

func newBlockingAdapter() *blockingAdapter {
	return &blockingAdapter{started: make(chan string, 4), release: make(chan error)}
}

func (a *blockingAdapter) Send(_ context.Context, it workshop.OutboxItem) error {
	a.started <- it.ID
	return <-a.release
}

func outboxEvents(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id, event string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox_events WHERE outbox_id = $1 AND event = $2`, id, event).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A call cancelled while it is being sent stays cancelled: a failure that
// comes back after is not retried, and nothing sends it again -- whether
// whoever runs the shop cancelled it or the module was switched off.
func TestACallCancelledWhileItIsSentStaysCancelled(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	tpl := purchaseTemplate(t, pool)
	for _, how := range []string{"by the owner", "by the module"} {
		doc := purchaseAgreement(t, pool, tpl, "")
		d, _ := workshop.DocumentRecordByID(ctx, pool, buyerScope(), doc)
		id, err := workshop.QueueSignatureRequest(ctx, pool, buyerScope(), doc, d.Parties[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		a := newBlockingAdapter()
		done := make(chan workshop.OutboxRun)
		go func() {
			run, err := workshop.RunOutbox(ctx, pool, shopID, map[string]workshop.OutboxAdapter{"signature_request": a}, 1)
			if err != nil {
				t.Error(err)
			}
			done <- run
		}()
		if got := <-a.started; got != id {
			t.Fatalf("%s: sending %s, want %s", how, got, id)
		}
		if how == "by the owner" {
			if err := workshop.CancelOutbox(ctx, pool, owner(), id, "Fel avtal"); err != nil {
				t.Fatal(err)
			}
		} else {
			if _, err := pool.Exec(ctx, `INSERT INTO shop_module_changes (shop_id, module, enabled, reason) VALUES ($1, 'dealer', true, 'test')`, shopID); err != nil {
				t.Fatal(err)
			}
			if _, err := workshop.SwitchModule(ctx, pool, owner(), access.Dealer, false, "Slutar sälja bilar", true); err != nil {
				t.Fatal(err)
			}
		}
		a.release <- errors.New("signing service: timeout")
		if run := <-done; run.Retried != 0 {
			t.Errorf("%s: the cancelled call was scheduled again: %+v", how, run)
		}
		if state, _ := callState(t, id, pool); state != "cancelled" {
			t.Errorf("%s: after the send failed: %s, want cancelled", how, state)
		}
		again := &countingAdapter{}
		if run, err := workshop.RunOutbox(ctx, pool, shopID, map[string]workshop.OutboxAdapter{"signature_request": again}, 10); err != nil || run.Sent != 0 || len(again.keys) != 0 {
			t.Errorf("%s: sent after it was cancelled: %+v, %v, %v", how, run, err, again.keys)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO shop_module_changes (shop_id, module, enabled, reason) VALUES ($1, 'dealer', true, 'test')`, shopID); err != nil {
			t.Fatal(err)
		}
	}
}

// Each row has its own lease, taken as it is sent: a row waiting behind a
// slow one is not held, so another worker sends it; and a worker whose
// lease ran out mid-send, and whose row another worker then sent, writes
// nothing over that worker's outcome.
func TestEachCallHasItsOwnLease(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	tpl := purchaseTemplate(t, pool)
	doc := purchaseAgreement(t, pool, tpl, "")
	d, _ := workshop.DocumentRecordByID(ctx, pool, buyerScope(), doc)
	var ids []string
	for _, p := range d.Parties {
		id, err := workshop.QueueSignatureRequest(ctx, pool, buyerScope(), doc, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	slow := newBlockingAdapter()
	done := make(chan workshop.OutboxRun)
	go func() {
		run, err := workshop.RunOutbox(ctx, pool, shopID, map[string]workshop.OutboxAdapter{"signature_request": slow}, 10)
		if err != nil {
			t.Error(err)
		}
		done <- run
	}()
	first := <-slow.started
	other := ids[0]
	if other == first {
		other = ids[1]
	}
	// The second row is not held by the slow worker: another sends it now.
	quick := &countingAdapter{}
	if run, err := workshop.RunOutbox(ctx, pool, shopID, map[string]workshop.OutboxAdapter{"signature_request": quick}, 10); err != nil || run.Sent != 1 {
		t.Errorf("the row behind the slow one: %+v, %v; want sent by the other worker", run, err)
	}
	if state, _ := callState(t, other, pool); state != "succeeded" {
		t.Errorf("the row behind the slow one is %s", state)
	}
	// The slow worker's lease runs out; another worker takes the row and
	// sends it. The slow one's answer, when it comes, is not written.
	if _, err := pool.Exec(ctx, `UPDATE outbox SET claimed_until = now() - interval '1 second' WHERE id = $1`, first); err != nil {
		t.Fatal(err)
	}
	if run, err := workshop.RunOutbox(ctx, pool, shopID, map[string]workshop.OutboxAdapter{"signature_request": quick}, 10); err != nil || run.Sent != 1 {
		t.Errorf("the row whose lease ran out: %+v, %v", run, err)
	}
	slow.release <- errors.New("signing service: timeout")
	<-done
	if state, attempts := callState(t, first, pool); state != "succeeded" || attempts != 2 {
		t.Errorf("the row the slow worker lost: %s after %d attempts; want succeeded after 2", state, attempts)
	}
	if n := outboxEvents(t, pool, first, "failed") + outboxEvents(t, pool, first, "retry_scheduled"); n != 0 {
		t.Errorf("the slow worker wrote %d outcomes over the one that sent it", n)
	}
	if len(quick.keys) != 2 || quick.keys[0] == quick.keys[1] {
		t.Errorf("sent with %v; want each row once by the other worker", quick.keys)
	}
}

// A request cancelled -- by mistake, or with its module switched off and on
// again -- can be asked for again: the same row queued anew, sent, and its
// history still saying it was cancelled once.
func TestACancelledCallCanBeAskedForAgain(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	tpl := purchaseTemplate(t, pool)
	doc := purchaseAgreement(t, pool, tpl, "")
	d, _ := workshop.DocumentRecordByID(ctx, pool, buyerScope(), doc)
	id, err := workshop.QueueSignatureRequest(ctx, pool, buyerScope(), doc, d.Parties[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := workshop.CancelOutbox(ctx, pool, owner(), id, "Av misstag"); err != nil {
		t.Fatal(err)
	}
	again, err := workshop.QueueSignatureRequest(ctx, pool, buyerScope(), doc, d.Parties[0].ID)
	if err != nil || again != id {
		t.Fatalf("asked for again: %s, %v; want the same call, %s", again, err, id)
	}
	if state, attempts := callState(t, id, pool); state != "queued" || attempts != 0 {
		t.Errorf("asked for again: %s after %d attempts, want queued and none", state, attempts)
	}
	a := &countingAdapter{}
	if run, err := workshop.RunOutbox(ctx, pool, shopID, map[string]workshop.OutboxAdapter{"signature_request": a}, 10); err != nil || run.Sent != 1 {
		t.Errorf("sent after it was asked for again: %+v, %v", run, err)
	}
	if n := outboxEvents(t, pool, id, "cancelled"); n != 1 {
		t.Errorf("the history has %d cancellations, want the one", n)
	}
	// Done, it is not asked for again: the same request is the call made.
	if _, err := workshop.QueueSignatureRequest(ctx, pool, buyerScope(), doc, d.Parties[0].ID); err != nil {
		t.Fatal(err)
	}
	if state, _ := callState(t, id, pool); state != "succeeded" {
		t.Errorf("a call made, asked for again: %s", state)
	}
}

// A call cancelled while a worker is sending it, and asked for again
// before the send comes back, is not sent a second time by another worker:
// the lease the first worker holds stands, and what came of its send is
// written.
func TestACallAskedForAgainWhileItIsSentIsNotSentTwice(t *testing.T) {
	pool := setup(t)
	ctx := context.Background()
	tpl := purchaseTemplate(t, pool)
	doc := purchaseAgreement(t, pool, tpl, "")
	d, _ := workshop.DocumentRecordByID(ctx, pool, buyerScope(), doc)
	id, err := workshop.QueueSignatureRequest(ctx, pool, buyerScope(), doc, d.Parties[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	slow := newBlockingAdapter()
	done := make(chan workshop.OutboxRun)
	go func() {
		run, err := workshop.RunOutbox(ctx, pool, shopID, map[string]workshop.OutboxAdapter{"signature_request": slow}, 1)
		if err != nil {
			t.Error(err)
		}
		done <- run
	}()
	if got := <-slow.started; got != id {
		t.Fatalf("sending %s, want %s", got, id)
	}
	if err := workshop.CancelOutbox(ctx, pool, owner(), id, "Av misstag"); err != nil {
		t.Fatal(err)
	}
	if _, err := workshop.QueueSignatureRequest(ctx, pool, buyerScope(), doc, d.Parties[0].ID); err != nil {
		t.Fatal(err)
	}
	other := &countingAdapter{}
	if run, err := workshop.RunOutbox(ctx, pool, shopID, map[string]workshop.OutboxAdapter{"signature_request": other}, 10); err != nil || len(other.keys) != 0 {
		t.Errorf("another worker sent the call while the first was sending it: %+v, %v, %v", run, err, other.keys)
	}
	slow.release <- nil
	if run := <-done; run.Sent != 1 {
		t.Errorf("the first worker's send: %+v, want it recorded as sent", run)
	}
	if state, _ := callState(t, id, pool); state != "succeeded" {
		t.Errorf("after the first worker's send went through: %s, want succeeded", state)
	}
}
