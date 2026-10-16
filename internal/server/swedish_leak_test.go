package server

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/RedaEkengren/FreeSMS/internal/i18n"
)

// A Swedish workshop reads Swedish. The catalogue says, for every English
// sentence, what its Swedish is; so an English sentence from the catalogue on
// a page read in Swedish is a place where a string went to the page without
// going through it. This walks the pages an ordinary day uses, as each kind of
// person, and fails on any such sentence.
//
// What the shop wrote itself -- a part's name, a note, a checklist label -- is
// never in the catalogue, and so is never a leak.

var leakTagExpr = regexp.MustCompile(`(?s)<script.*?</script>|<style.*?</style>|<[^>]*>`)

func pageWords(body string) string {
	text := leakTagExpr.ReplaceAllString(body, " ")
	text = html.UnescapeString(text)
	return strings.Join(strings.Fields(text), " ")
}

// englishSentences are the catalogue's keys that read as sentences and that have
// a Swedish different from them.
func englishSentences(t *testing.T) []string {
	t.Helper()
	cats, err := i18n.Load("en", false)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for key, m := range cats.Messages("sv") {
		if strings.Contains(key, "%") || len(key) < 14 || !strings.Contains(key, " ") {
			continue
		}
		if m.Other == key || m.Other == "" {
			continue
		}
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// labelSet are the short labels -- a heading, a button, a tag -- that have a
// Swedish different from them. A short label inside a longer sentence is just
// a word, so these are matched only as the whole text of an element.
func labelSet(t *testing.T) map[string]bool {
	t.Helper()
	cats, err := i18n.Load("en", false)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for key, m := range cats.Messages("sv") {
		if strings.Contains(key, "%") || len(key) < 4 || m.Other == key || m.Other == "" {
			continue
		}
		out[key] = true
	}
	return out
}

func elementTexts(body string) []string {
	text := leakTagExpr.ReplaceAllString(body, "\x00")
	text = html.UnescapeString(text)
	var out []string
	for _, seg := range strings.Split(text, "\x00") {
		if seg = strings.Join(strings.Fields(seg), " "); seg != "" {
			out = append(out, seg)
		}
	}
	return out
}

func TestNoPageReadInSwedishShowsAnEnglishSentenceFromTheCatalogue(t *testing.T) {
	ts, pool := testServer(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE shops SET locale = 'sv' WHERE id = $1`, shopA); err != nil {
		t.Fatal(err)
	}
	desk, tech, owner := signIn(t, ts, advisorEmail), signIn(t, ts, techEmail), signIn(t, ts, ownerEmail)
	base := ts.URL
	job := base + intake(t, base, desk, url.Values{"registration": {"SVE 001"}, "complaint": {"Bromsar"}})
	jobID := job[strings.LastIndex(job, "/")+1:]
	if status := postStatus(t, desk, job+"/lines", url.Values{"kind": {"labour"}, "description": {"Byt bromsklossar"}, "quantity": {"1"}, "unit_price": {"800"}}); status != http.StatusSeeOther {
		t.Fatalf("adding a line answered %d", status)
	}
	var vehicleID, customerID, invoiceID string
	if err := pool.QueryRow(ctx, `SELECT vehicle_id::text FROM work_orders WHERE id = $1`, jobID).Scan(&vehicleID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT id::text FROM customers WHERE shop_id = $1 LIMIT 1`, shopA).Scan(&customerID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO invoices (shop_id, series, number, work_order_id, customer_name, net_minor, vat_minor, gross_minor, seller_name, seller_payment_terms_days)
		VALUES ($1, 'A', 77, $2, 'Test Kund', 100000, 25000, 125000, 'Shop A', 0) RETURNING id::text`, shopA, jobID).Scan(&invoiceID); err != nil {
		t.Fatal(err)
	}

	everyone := []string{"/", "/mine", "/jobs/" + jobID, "/jobs/" + jobID + "/timeline", "/jobs/" + jobID + "/outsourced",
		"/vehicles/" + vehicleID, "/search?q=SVE", "/time", "/rota", "/account", "/help", "/help/workshop", "/help/reception"}
	deskOnly := []string{"/board", "/jobs/new", "/parts", "/jobs/" + jobID + "/summary", "/jobs/" + jobID + "/comeback",
		"/enquiries", "/calendar", "/customers", "/receivables", "/day", "/accounting", "/privacy", "/labour", "/figures",
		"/outsourced", "/supply", "/stock", "/scan", "/checklists", "/schedule"}
	deskOnly = append(deskOnly, "/customers/"+customerID, "/invoices/"+invoiceID, "/invoices/"+invoiceID+"/followup",
		"/invoices/"+invoiceID+"/credit", "/invoices/"+invoiceID+"/rebill", "/invoices/"+invoiceID+"/docview")
	ownerOnly := []string{"/shop", "/staff", "/import", "/start", "/start/invoice", "/help/owner"}

	sentences := englishSentences(t)
	if len(sentences) < 300 {
		t.Fatalf("only %d sentences to look for; the catalogue has stopped loading", len(sentences))
	}
	labels := labelSet(t)
	leaks := map[string][]string{}
	walk := func(who string, c *http.Client, paths []string) {
		for _, p := range paths {
			status, body := page(t, c, base+p)
			if status != http.StatusOK {
				continue
			}
			text := pageWords(body)
			for _, s := range sentences {
				if strings.Contains(text, s) {
					leaks[s] = append(leaks[s], who+" "+p)
				}
			}
			for _, seg := range elementTexts(body) {
				if labels[seg] {
					leaks[seg] = append(leaks[seg], who+" "+p)
				}
			}
		}
	}
	walk("technician", tech, everyone)
	walk("advisor", desk, append(append([]string{}, everyone...), deskOnly...))
	walk("owner", owner, append(append(append([]string{}, everyone...), deskOnly...), ownerOnly...))

	if len(leaks) > 0 {
		var lines []string
		for s, where := range leaks {
			lines = append(lines, "\""+s+"\"  on "+where[0])
		}
		sort.Strings(lines)
		t.Errorf("%d English sentences on pages read in Swedish; route them through the catalogue:\n%s", len(lines), strings.Join(lines, "\n"))
	}
}
