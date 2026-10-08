package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/access"
	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/RedaEkengren/FreeSMS/internal/workshop"
	"github.com/jackc/pgx/v5"
)

// Nothing in the evening or the night: a message waits for eight o'clock in
// the shop's zone, and goes at once in the day.
func TestAMessageWaitsForTheMorning(t *testing.T) {
	for _, c := range []struct{ now, want string }{
		{"2026-10-15 14:10", "2026-10-15 14:10"},
		{"2026-10-15 07:59", "2026-10-15 08:00"},
		{"2026-10-15 20:00", "2026-10-16 08:00"},
		{"2026-10-24 23:30", "2026-10-25 08:00"}, // the night the clocks go back
	} {
		now, _ := time.ParseInLocation("2006-01-02 15:04", c.now, stockholm)
		if got := workshop.DefaultSendHours.NotBefore(now, stockholm).In(stockholm).Format("2006-01-02 15:04"); got != c.want {
			t.Errorf("queued at %s goes at %s, want %s", c.now, got, c.want)
		}
	}
}

func told(t *testing.T, job string) bool {
	t.Helper()
	board, err := workshop.Board(context.Background(), currentPool, advisor())
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range board {
		if b.ID == job {
			return b.ToldAt != nil
		}
	}
	t.Fatalf("job %s is not on the board", job)
	return false
}

// Queued, then sent, is the customer told -- once, and only while the
// provider has not said it never arrived.
func TestASentMessageTellsTheCustomerUntilItIsReportedFailed(t *testing.T) {
	pool := setup(t)
	currentPool = pool
	ctx := context.Background()
	job := newJob(t, pool)
	move(t, pool, job, workshop.StateEstimated, workshop.StateAwaitingApproval,
		workshop.StateApproved, workshop.StateInProgress, workshop.StateReady)

	msg := workshop.NewMessage{JobID: job, About: "ready", Channel: "sms", Recipient: "+46701112233",
		Body: "Din bil är klar.", NotBefore: time.Now().Add(time.Hour)}
	if _, err := workshop.QueueMessage(ctx, pool, technician(), msg); !errors.Is(err, access.ErrForbidden) {
		t.Errorf("a technician queued a message to a customer: %v", err)
	}
	id, err := workshop.QueueMessage(ctx, pool, advisor(), msg)
	if err != nil {
		t.Fatal(err)
	}
	if due, _ := workshop.DueMessages(ctx, pool, shopID, time.Now(), 10); len(due) != 0 {
		t.Errorf("a message before its time is due: %+v", due)
	}
	due, _ := workshop.DueMessages(ctx, pool, shopID, time.Now().Add(2*time.Hour), 10)
	if len(due) != 1 || due[0].ID != id {
		t.Fatalf("due: %+v", due)
	}
	if told(t, job) {
		t.Error("queued is not told")
	}
	if at, _ := workshop.AlreadySent(ctx, pool, advisor(), job, "ready"); at == nil {
		t.Error("a queued message is not counted as on its way")
	}

	if err := workshop.MessageSent(ctx, pool, shopID, id, "46elks", "s1"); err != nil {
		t.Fatal(err)
	}
	if !told(t, job) {
		t.Error("sent, and the board does not say told")
	}
	if due, _ := workshop.DueMessages(ctx, pool, shopID, time.Now().Add(2*time.Hour), 10); len(due) != 0 {
		t.Errorf("sent, and still due: %+v", due)
	}
	if sent, _ := workshop.SentBefore(ctx, pool, advisor(), job, "+46701112233"); !sent {
		t.Error("the number is not known as sent to")
	}

	if err := workshop.MessageDelivery(ctx, pool, shopID, "46elks", "nobody", false); !errors.Is(err, workshop.ErrNotFound) {
		t.Errorf("a report about a message nobody sent: %v", err)
	}
	if err := workshop.MessageDelivery(ctx, pool, shopID, "46elks", "s1", false); err != nil {
		t.Fatal(err)
	}
	if told(t, job) {
		t.Error("reported as never arriving, and still told")
	}
	if at, _ := workshop.AlreadySent(ctx, pool, advisor(), job, "ready"); at != nil {
		t.Error("a failed message stops a new one going")
	}
}

// A provider that is down is tried again at growing gaps, then given up.
func TestAMessageThatCannotBeHandedOverIsTriedThenGivenUp(t *testing.T) {
	pool := setup(t)
	currentPool = pool
	ctx := context.Background()
	job := newJob(t, pool)
	id, err := workshop.QueueMessage(ctx, pool, advisor(), workshop.NewMessage{JobID: job, About: "ready", Channel: "email",
		Recipient: "kund@example.se", Body: "x", NotBefore: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	// Each failure waits twice as long as the one before: a minute, two, four.
	now := time.Now()
	for try := 0; try < 6; try++ {
		due, _ := workshop.DueMessages(ctx, pool, shopID, now, 10)
		if len(due) != 1 || due[0].Tries != try {
			t.Fatalf("try %d: due %+v", try, due)
		}
		if err := workshop.MessageNotSent(ctx, pool, shopID, id, due[0].Tries, "provider down"); err != nil {
			t.Fatal(err)
		}
		wait := time.Minute << try
		if again, _ := workshop.DueMessages(ctx, pool, shopID, time.Now().Add(wait-5*time.Second), 10); len(again) != 0 {
			t.Fatalf("try %d: retried before %v", try, wait)
		}
		now = time.Now().Add(wait + 5*time.Second)
	}
	if due, _ := workshop.DueMessages(ctx, pool, shopID, now.Add(24*time.Hour), 10); len(due) != 0 {
		t.Errorf("given up, and still due: %+v", due)
	}
	msgs, _ := workshop.MessagesFor(ctx, pool, advisor(), job)
	if len(msgs) != 1 || msgs[0].State != "failed" || msgs[0].Detail != "provider down" {
		t.Errorf("the job's messages: %+v", msgs)
	}
}

// The number a message went to is the customer's; erased with them.
func TestErasingACustomerClearsWhereTheirMessagesWent(t *testing.T) {
	pool := setup(t)
	currentPool = pool
	ctx := context.Background()
	job := newJob(t, pool)
	if _, err := workshop.QueueMessage(ctx, pool, advisor(), workshop.NewMessage{JobID: job, About: "ready", Channel: "sms",
		Recipient: "+46701112233", Body: "x", NotBefore: time.Now()}); err != nil {
		t.Fatal(err)
	}
	var person string
	database.InShop(ctx, pool, shopID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT c.person_id FROM work_orders w JOIN customers c ON c.id = w.customer_id WHERE w.id = $1`, job).Scan(&person)
	})
	if person == "" {
		t.Skip("the fixture job has no person as its customer")
	}
	if err := workshop.ErasePerson(ctx, pool, advisor(), person, "asked"); err != nil {
		t.Fatal(err)
	}
	msgs, _ := workshop.MessagesFor(ctx, pool, advisor(), job)
	if len(msgs) != 1 || msgs[0].Recipient != "" {
		t.Errorf("after erasure: %+v", msgs)
	}
	if due, _ := workshop.DueMessages(ctx, pool, shopID, time.Now().Add(time.Hour), 10); len(due) != 0 {
		t.Errorf("a message to an erased customer would still go: %+v", due)
	}
}
