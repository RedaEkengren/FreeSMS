package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/database"
	"github.com/jackc/pgx/v5"
)

// Finishing a final check with a checkpoint unanswered is refused, the
// refusal says which checkpoint, and the job page does not call it done.
func TestAnUnansweredFinalCheckSaysWhatIsLeft(t *testing.T) {
	ts, pool := testServer(t)
	template := seedTemplate(t, pool)
	ctx := context.Background()
	if err := database.InShop(ctx, pool, shopA, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE shops SET final_check_template_id = $1 WHERE id = $2`, template, shopA); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE work_orders SET state = 'in_progress' WHERE id = $1`, jobA)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	tech := signIn(t, ts, techEmail)
	post(t, tech, ts.URL+"/jobs/"+jobA+"/inspect", url.Values{"template_id": {template}})
	var inspection string
	if err := database.InShop(ctx, pool, shopA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM inspections WHERE work_order_id = $1`, jobA).Scan(&inspection)
	}); err != nil {
		t.Fatalf("the final check was not started: %v", err)
	}

	if status := postStatus(t, tech, ts.URL+"/inspections/"+inspection+"/complete", url.Values{}); status != http.StatusBadRequest {
		t.Errorf("finishing an unanswered final check answered %d", status)
	}
	if body := post(t, tech, ts.URL+"/inspections/"+inspection+"/complete", url.Values{}); !strings.Contains(body, "Front brakes") {
		t.Errorf("the refusal does not say which checkpoint:\n%s", firstLines(body, 40))
	}
	if _, body := page(t, tech, ts.URL+"/jobs/"+jobA); !strings.Contains(body, "The final check has not been done.") {
		t.Errorf("the job page does not say the final check is not done")
	}
}
