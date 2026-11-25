package server

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/RedaEkengren/FreeSMS/internal/workshop"
)

// A set is taken in from its owner's page, listed, and moved to another car of
// theirs; the technician reads it and the car and is never shown whose it is,
// and cannot take one in or move one.
func TestASetOfTyresIsTakenInFromTheOwnersPageAndAtechnicianDoesNotSeeTheOwner(t *testing.T) {
	ts, pool := testServer(t)
	const customer = "aaaaaaaa-0000-0000-0000-000000000004"
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO vehicle_ownership (shop_id, vehicle_id, customer_id) VALUES ($1, $2, $3)`, shopA, vehicleA, customer); err != nil {
		t.Fatal(err)
	}
	var second string
	if err := pool.QueryRow(ctx, `INSERT INTO vehicles (shop_id, make, model) VALUES ($1, 'Saab', '9-3') RETURNING id`, shopA).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO vehicle_ownership (shop_id, vehicle_id, customer_id) VALUES ($1, $2, $3)`, shopA, second, customer); err != nil {
		t.Fatal(err)
	}
	advisor, tech := signIn(t, ts, advisorEmail), signIn(t, ts, techEmail)
	base := ts.URL + "/customers/" + customer + "/tyres"
	if page := visibleText(get(t, advisor, base, http.StatusOK)); !strings.Contains(page, "Take a set in") || !strings.Contains(page, "No tyre sets yet.") {
		t.Fatalf("the owner's page:\n%s", page)
	}
	form := url.Values{"season": {"winter_friction"}, "vehicle": {vehicleA}, "brand": {"Nokian"}, "model": {"WR D4"}, "front": {"205/55R16 91H"},
		"rims": {"steel"}, "tpms": {"1"}, "dot_week": {"12"}, "dot_year": {"2023"}}
	if status := postStatus(t, tech, base, form); status != http.StatusForbidden {
		t.Errorf("a technician taking a set in answered %d", status)
	}
	if status := postStatus(t, advisor, base, url.Values{"season": {"monsoon"}, "front": {"205/55R16"}, "rims": {"steel"}}); status != http.StatusBadRequest {
		t.Errorf("a season that is not one answered %d", status)
	}
	req, _ := http.NewRequest(http.MethodPost, base, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", ts.URL)
	noFollow := *advisor
	noFollow.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noFollow.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusSeeOther || !regexp.MustCompile(`^/tyres/[0-9a-f-]{36}$`).MatchString(loc) {
		t.Fatalf("taking it in answered %d, to %q", resp.StatusCode, loc)
	}
	set := visibleText(get(t, advisor, ts.URL+loc, http.StatusOK))
	for _, want := range []string{"Nokian", "205/55R16 91H", "Pressure sensors", customerName, "Volvo V70"} {
		if !strings.Contains(set, want) {
			t.Errorf("the advisor's set lacks %q:\n%s", want, set)
		}
	}
	if status := postStatus(t, advisor, ts.URL+loc+"/move", url.Values{"vehicle": {second}}); status != http.StatusSeeOther {
		t.Errorf("moving it answered %d", status)
	}
	if status := postStatus(t, advisor, ts.URL+loc+"/move", url.Values{"vehicle": {vehicleB}}); status != http.StatusBadRequest {
		t.Errorf("moving it to another shop's car answered %d", status)
	}
	if status := postStatus(t, tech, ts.URL+loc+"/move", url.Values{"vehicle": {""}}); status != http.StatusForbidden {
		t.Errorf("a technician moving it answered %d", status)
	}
	if moved := visibleText(get(t, advisor, ts.URL+loc, http.StatusOK)); !strings.Contains(moved, "Saab 9-3") || strings.Count(moved, "Volvo V70") < 1 {
		t.Errorf("the set after the move:\n%s", moved)
	}

	for _, page := range []string{ts.URL + loc, ts.URL + "/tyres"} {
		seen := get(t, tech, page, http.StatusOK)
		if !strings.Contains(seen, "Nokian") || !strings.Contains(seen, "Saab 9-3") {
			t.Errorf("the technician's %s:\n%s", page, visibleText(seen))
		}
		for _, secret := range []string{customerName, customerAddress, customerPhoneDigits} {
			if strings.Contains(seen, secret) {
				t.Errorf("the technician is shown %q on %s", secret, page)
			}
		}
	}
	if status, _ := getBody(t, tech, base); status != http.StatusForbidden {
		t.Errorf("a technician on the owner's tyre page answered %d", status)
	}
	if list := visibleText(get(t, advisor, ts.URL+"/tyres", http.StatusOK)); !strings.Contains(list, customerName) {
		t.Errorf("the advisor's list does not name the owner:\n%s", list)
	}
}

// The owner makes a storage place, a technician puts sets in it until it is
// full and sees whose they are never; a full place and a place that is not in
// use refuse, and the pages say how much room is left.
func TestAStoragePlaceIsMadeByTheOwnerAndFilledByTheTechnician(t *testing.T) {
	ts, pool := testServer(t)
	const customer = "aaaaaaaa-0000-0000-0000-000000000004"
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO vehicle_ownership (shop_id, vehicle_id, customer_id) VALUES ($1, $2, $3)`, shopA, vehicleA, customer); err != nil {
		t.Fatal(err)
	}
	owner, advisor, tech := signIn(t, ts, ownerEmail), signIn(t, ts, advisorEmail), signIn(t, ts, techEmail)
	if status := postStatus(t, advisor, ts.URL+"/storage", url.Values{"name": {"Hall A"}, "capacity": {"1"}}); status != http.StatusForbidden {
		t.Errorf("an advisor making a place answered %d", status)
	}
	if status := postStatus(t, owner, ts.URL+"/storage", url.Values{"name": {"Hall A"}, "capacity": {"0"}}); status != http.StatusBadRequest {
		t.Errorf("a place with no room answered %d", status)
	}
	if status := postStatus(t, owner, ts.URL+"/storage", url.Values{"name": {"Hall A"}, "capacity": {"1"}}); status != http.StatusSeeOther {
		t.Fatalf("making a place answered %d", status)
	}
	var place string
	if err := pool.QueryRow(ctx, `SELECT id FROM storage_locations WHERE name = 'Hall A'`).Scan(&place); err != nil {
		t.Fatal(err)
	}
	makeSet := func() string {
		form := url.Values{"season": {"summer"}, "vehicle": {vehicleA}, "brand": {"Michelin"}, "front": {"205/55R16"}, "rims": {"alloy"}}
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/customers/"+customer+"/tyres", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", ts.URL)
		c := *advisor
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.Header.Get("Location")
	}
	first, second := makeSet(), makeSet()
	if status := postStatus(t, tech, ts.URL+first+"/place", url.Values{"place": {place}}); status != http.StatusSeeOther {
		t.Fatalf("a technician placing a set answered %d", status)
	}
	if status := postStatus(t, tech, ts.URL+second+"/place", url.Values{"place": {place}}); status != http.StatusBadRequest {
		t.Errorf("a set in a full place answered %d", status)
	}
	if status := postStatus(t, tech, ts.URL+"/storage/"+place+"/use", url.Values{"use": {"0"}}); status != http.StatusForbidden {
		t.Errorf("a technician taking a place out of use answered %d", status)
	}
	if status := postStatus(t, owner, ts.URL+"/storage/"+place+"/use", url.Values{"use": {"0"}}); status != http.StatusBadRequest {
		t.Errorf("a place with a set in it taken out of use answered %d", status)
	}
	seen := get(t, tech, ts.URL+"/storage", http.StatusOK)
	text := visibleText(seen)
	if !strings.Contains(text, "Hall A") || !strings.Contains(text, "0 of 1 places free") || !strings.Contains(text, "Michelin") || strings.Contains(seen, customerName) {
		t.Errorf("the technician's storage page:\n%s", text)
	}
	if page := visibleText(get(t, owner, ts.URL+"/storage", http.StatusOK)); !strings.Contains(page, customerName) || !strings.Contains(page, "Take out of use") {
		t.Errorf("the owner's storage page:\n%s", page)
	}
	if page := visibleText(get(t, tech, ts.URL+first, http.StatusOK)); !strings.Contains(page, "Stored in Hall A") {
		t.Errorf("the set's page:\n%s", page)
	}
	if status := postStatus(t, tech, ts.URL+first+"/place", url.Values{"place": {""}}); status != http.StatusSeeOther {
		t.Errorf("taking a set out answered %d", status)
	}
	if status := postStatus(t, tech, ts.URL+second+"/place", url.Values{"place": {place}}); status != http.StatusSeeOther {
		t.Errorf("the room it left answered %d", status)
	}
	if status := postStatus(t, tech, ts.URL+second+"/place", url.Values{"place": {"bbbbbbbb-0000-0000-0000-000000000019"}}); status != http.StatusNotFound {
		t.Errorf("another shop's place answered %d", status)
	}
}

// Scan-scan: the set's label, then the place's; the labels page prints both;
// a label nobody has refuses.
func TestAStoredSetIsPlacedByScanningItsLabelAndThenThePlaces(t *testing.T) {
	ts, pool := testServer(t)
	const customer = "aaaaaaaa-0000-0000-0000-000000000004"
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO vehicle_ownership (shop_id, vehicle_id, customer_id) VALUES ($1, $2, $3)`, shopA, vehicleA, customer); err != nil {
		t.Fatal(err)
	}
	owner, advisor, tech := signIn(t, ts, ownerEmail), signIn(t, ts, advisorEmail), signIn(t, ts, techEmail)
	postStatus(t, owner, ts.URL+"/storage", url.Values{"name": {"Rack 7"}, "capacity": {"2"}})
	var place string
	if err := pool.QueryRow(ctx, `SELECT id FROM storage_locations WHERE name = 'Rack 7'`).Scan(&place); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/customers/"+customer+"/tyres", strings.NewReader(url.Values{
		"season": {"summer"}, "brand": {"Michelin"}, "front": {"205/55R16"}, "rims": {"alloy"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", ts.URL)
	c := *advisor
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	set := strings.TrimPrefix(resp.Header.Get("Location"), "/tyres/")

	labels := get(t, tech, ts.URL+"/storage/labels", http.StatusOK)
	for _, code := range []string{workshop.TyreSetLabel(set), workshop.StorageLabel(place)} {
		if !strings.Contains(labels, code) || !strings.Contains(labels, "<svg") {
			t.Errorf("the labels page lacks %s", code)
		}
	}
	scan := func(form url.Values) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/storage/scan", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", ts.URL)
		cl := *tech
		cl.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode, resp.Header.Get("Location")
	}
	if status, _ := scan(url.Values{"code": {"TS-000000000000"}}); status != http.StatusNotFound {
		t.Errorf("a label nobody has answered %d", status)
	}
	if status, loc := scan(url.Values{"code": {workshop.TyreSetLabel(set)}}); status != http.StatusSeeOther || loc != "/storage/scan?set="+set {
		t.Fatalf("the set's label answered %d to %q", status, loc)
	}
	if page := visibleText(get(t, tech, ts.URL+"/storage/scan?set="+set, http.StatusOK)); !strings.Contains(page, "Now scan the place for Michelin") {
		t.Errorf("the second scan's page:\n%s", page)
	}
	if status, loc := scan(url.Values{"code": {workshop.StorageLabel(place)}, "set": {set}}); status != http.StatusSeeOther || !strings.HasPrefix(loc, "/storage/scan?done=") {
		t.Fatalf("the place's label answered %d to %q", status, loc)
	}
	if page := visibleText(get(t, tech, ts.URL+"/tyres/"+set, http.StatusOK)); !strings.Contains(page, "Stored in Rack 7") {
		t.Errorf("the set after the scans:\n%s", page)
	}
}

// A technician records depths from the set's page, a wrong one is corrected
// with a reason, the page says which tyres are at the shop's limit or below the
// law, and the owner moves the limit.
func TestATechnicianRecordsTreadDepthAndTheOwnerSetsTheLimit(t *testing.T) {
	ts, pool := testServer(t)
	const customer = "aaaaaaaa-0000-0000-0000-000000000004"
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO vehicle_ownership (shop_id, vehicle_id, customer_id) VALUES ($1, $2, $3)`, shopA, vehicleA, customer); err != nil {
		t.Fatal(err)
	}
	owner, advisor, tech := signIn(t, ts, ownerEmail), signIn(t, ts, advisorEmail), signIn(t, ts, techEmail)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/customers/"+customer+"/tyres", strings.NewReader(url.Values{
		"season": {"summer"}, "brand": {"Michelin"}, "front": {"205/55R16"}, "rims": {"alloy"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", ts.URL)
	c := *advisor
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	set := resp.Header.Get("Location")
	var tyres []string
	rows, err := pool.Query(ctx, `SELECT id FROM tyres WHERE shop_id = $1 ORDER BY position`, shopA)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		tyres = append(tyres, id)
	}
	rows.Close()

	if page := visibleText(get(t, tech, ts.URL+set, http.StatusOK)); strings.Count(page, "not measured") != 4 {
		t.Errorf("a set never measured:\n%s", page)
	}
	for _, bad := range []string{"", "x", "26", "-1"} {
		if status := postStatus(t, tech, ts.URL+set+"/tread", url.Values{"tyre": {tyres[0]}, "depth": {bad}}); status != http.StatusBadRequest {
			t.Errorf("a depth of %q answered %d", bad, status)
		}
	}
	if status := postStatus(t, tech, ts.URL+set+"/tread", url.Values{"tyre": {tyres[0]}, "depth": {"3,5"}}); status != http.StatusSeeOther {
		t.Fatalf("recording 3,5 answered %d", status)
	}
	postStatus(t, tech, ts.URL+set+"/tread", url.Values{"tyre": {tyres[1]}, "depth": {"1.2"}, "note": {"kordblottad"}})
	page := visibleText(get(t, tech, ts.URL+set, http.StatusOK))
	for _, want := range []string{"Tread 3.5 mm", "Tread 1.2 mm", "Below the legal minimum of 1.6 mm.", "kordblottad", "not measured"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, "limit of 3.0") {
		t.Errorf("3.5 mm is above the default limit of 3.0:\n%s", page)
	}
	if status := postStatus(t, tech, ts.URL+"/tyres/limit", url.Values{"limit": {"4"}}); status != http.StatusForbidden {
		t.Errorf("a technician setting the limit answered %d", status)
	}
	if status := postStatus(t, owner, ts.URL+"/tyres/limit", url.Values{"limit": {"4,0"}}); status != http.StatusSeeOther {
		t.Fatalf("the owner setting the limit answered %d", status)
	}
	if status := postStatus(t, owner, ts.URL+"/tyres/limit", url.Values{"limit": {"0,5"}}); status != http.StatusBadRequest {
		t.Errorf("a limit below the law answered %d", status)
	}
	if page := visibleText(get(t, tech, ts.URL+set, http.StatusOK)); !strings.Contains(page, "limit of 4.0 mm.") {
		t.Errorf("under a limit of 4.0:\n%s", page)
	}
	var wrong string
	if err := pool.QueryRow(ctx, `SELECT id FROM tyre_measurements WHERE tenths = 12`).Scan(&wrong); err != nil {
		t.Fatal(err)
	}
	if status := postStatus(t, tech, ts.URL+set+"/tread/correct", url.Values{"measurement": {wrong}, "depth": {"6.1"}}); status != http.StatusBadRequest {
		t.Errorf("a correction with no reason answered %d", status)
	}
	if status := postStatus(t, tech, ts.URL+set+"/tread/correct", url.Values{"measurement": {wrong}, "depth": {"6.1"}, "reason": {"Fel däck"}}); status != http.StatusSeeOther {
		t.Fatalf("a correction answered %d", status)
	}
	if page := visibleText(get(t, tech, ts.URL+set, http.StatusOK)); !strings.Contains(page, "Tread 6.1 mm") || !strings.Contains(page, "Corrected: Fel däck") {
		t.Errorf("after the correction:\n%s", page)
	}
	if list := visibleText(get(t, owner, ts.URL+"/tyres", http.StatusOK)); !strings.Contains(list, "tread limit") {
		t.Errorf("the owner's list lacks the limit form:\n%s", list)
	}
	if list := visibleText(get(t, tech, ts.URL+"/tyres", http.StatusOK)); strings.Contains(list, "tread limit") {
		t.Errorf("a technician is offered the limit form")
	}
}

// A seasonal change from the job's page: the winter set out of storage, the
// summer set taken in with its tyres measured, the winter set fitted; a set a
// job holds is refused to a second, and the page shows the trail.
func TestASeasonalChangeIsDoneFromTheJobsPage(t *testing.T) {
	ts, pool := testServer(t)
	const customer = "aaaaaaaa-0000-0000-0000-000000000004"
	const job = "aaaaaaaa-0000-0000-0000-00000000000f"
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO vehicle_ownership (shop_id, vehicle_id, customer_id) VALUES ($1, $2, $3)`, shopA, vehicleA, customer); err != nil {
		t.Fatal(err)
	}
	owner, advisor, tech := signIn(t, ts, ownerEmail), signIn(t, ts, advisorEmail), signIn(t, ts, techEmail)
	postStatus(t, owner, ts.URL+"/storage", url.Values{"name": {"Rack"}, "capacity": {"3"}})
	postStatus(t, owner, ts.URL+"/storage", url.Values{"name": {"Shelf"}, "capacity": {"3"}})
	var rack, shelf string
	_ = pool.QueryRow(ctx, `SELECT id FROM storage_locations WHERE name = 'Rack'`).Scan(&rack)
	_ = pool.QueryRow(ctx, `SELECT id FROM storage_locations WHERE name = 'Shelf'`).Scan(&shelf)
	make := func(season, brand, vehicle string) string {
		form := url.Values{"season": {season}, "brand": {brand}, "front": {"205/55R16"}, "rims": {"alloy"}, "vehicle": {vehicle}}
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/customers/"+customer+"/tyres", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", ts.URL)
		c := *advisor
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return strings.TrimPrefix(resp.Header.Get("Location"), "/tyres/")
	}
	winter, summer := make("winter_friction", "Nokian", ""), make("summer", "Michelin", vehicleA)
	postStatus(t, tech, ts.URL+"/tyres/"+winter+"/place", url.Values{"place": {rack}})
	base := ts.URL + "/jobs/" + job + "/tyres"

	if page := visibleText(get(t, tech, base, http.StatusOK)); !strings.Contains(page, "Nothing done with tyres on this job yet.") {
		t.Errorf("the job's tyre page:\n%s", page)
	}
	if status := postStatus(t, tech, base+"/check-out", url.Values{"set": {winter}}); status != http.StatusSeeOther {
		t.Fatalf("taking the winter set out answered %d", status)
	}
	if status := postStatus(t, tech, base+"/check-out", url.Values{"set": {winter}}); status != http.StatusBadRequest {
		t.Errorf("taking a set a job holds answered %d", status)
	}
	if status := postStatus(t, tech, base+"/check-in", url.Values{"set": {summer}, "place": {shelf}}); status != http.StatusBadRequest {
		t.Errorf("a set with no tyre measured answered %d", status)
	}
	rows, _ := pool.Query(ctx, `SELECT t.id FROM tyres t WHERE t.tyre_set_id = $1`, summer)
	var tyres []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		tyres = append(tyres, id)
	}
	rows.Close()
	for _, id := range tyres {
		if status := postStatus(t, tech, ts.URL+"/tyres/"+summer+"/tread", url.Values{"tyre": {id}, "depth": {"4,2"}}); status != http.StatusSeeOther {
			t.Fatalf("measuring answered %d", status)
		}
	}
	if status := postStatus(t, tech, base+"/check-in", url.Values{"set": {summer}, "place": {shelf}}); status != http.StatusSeeOther {
		t.Fatalf("taking the summer set in answered %d", status)
	}
	if status := postStatus(t, tech, base+"/fit", url.Values{"set": {winter}}); status != http.StatusSeeOther {
		t.Fatalf("fitting the winter set answered %d", status)
	}
	if status := postStatus(t, tech, base+"/teleport", url.Values{"set": {winter}}); status != http.StatusNotFound {
		t.Errorf("a move that does not exist answered %d", status)
	}
	page := get(t, tech, base, http.StatusOK)
	text := visibleText(page)
	for _, want := range []string{"taken out of storage", "checked in", "fitted", "Shelf", "Nokian", "Michelin"} {
		if !strings.Contains(text, want) {
			t.Errorf("the trail lacks %q:\n%s", want, text)
		}
	}
	for _, secret := range []string{customerName, customerAddress, customerPhoneDigits} {
		if strings.Contains(page, secret) {
			t.Errorf("the technician is shown %q on the job's tyre page", secret)
		}
	}
	if set := visibleText(get(t, tech, ts.URL+"/tyres/"+winter, http.StatusOK)); !strings.Contains(set, "Volvo V70") {
		t.Errorf("the winter set is not on the car:\n%s", set)
	}
}

// The owner keeps the price list, the front desk puts an extra on a job, and
// the technician's page for the same job shows neither the list nor a price.
func TestTyreExtrasArePricedByTheOwnerAndAddedByTheFrontDesk(t *testing.T) {
	ts, pool := testServer(t)
	const job = "aaaaaaaa-0000-0000-0000-00000000000f"
	owner, advisor, tech := signIn(t, ts, ownerEmail), signIn(t, ts, advisorEmail), signIn(t, ts, techEmail)
	base := ts.URL + "/tyres/extras"

	if status := postStatus(t, advisor, base, url.Values{"name": {"Balancing"}, "per": {"tyre"}, "kind": {"labour"}, "price": {"150"}, "vat": {"25"}}); status != http.StatusForbidden {
		t.Errorf("the front desk making an extra answered %d", status)
	}
	if status := postStatus(t, owner, base, url.Values{"name": {"Balancing"}, "per": {"tyre"}, "kind": {"labour"}, "price": {"abc"}, "vat": {"25"}}); status != http.StatusBadRequest {
		t.Errorf("a price that is not a number answered %d", status)
	}
	if status := postStatus(t, owner, base, url.Values{"name": {"Balancing"}, "per": {"tyre"}, "kind": {"labour"}, "price": {"150"}, "vat": {"25"}}); status != http.StatusSeeOther {
		t.Fatalf("the owner making an extra answered %d", status)
	}
	var extra string
	if err := pool.QueryRow(context.Background(), `SELECT id FROM tyre_extras WHERE name = 'Balancing'`).Scan(&extra); err != nil {
		t.Fatal(err)
	}
	if status := postStatus(t, advisor, ts.URL+"/jobs/"+job+"/tyres/extra", url.Values{"extra": {extra}, "count": {"4"}}); status != http.StatusSeeOther {
		t.Fatalf("the front desk adding it answered %d", status)
	}
	var qty, price int64
	if err := pool.QueryRow(context.Background(),
		`SELECT (quantity * 1000)::bigint, unit_price_minor FROM work_order_lines WHERE work_order_id = $1 AND description = 'Balancing'`, job).Scan(&qty, &price); err != nil || qty != 4000 || price != 15000 {
		t.Errorf("the line: %d at %d, %v", qty, price, err)
	}
	if page := visibleText(get(t, owner, base, http.StatusOK)); !strings.Contains(page, "Balancing") || !strings.Contains(page, "Offer an extra") {
		t.Errorf("the owner's price list:\n%s", page)
	}
	if page := visibleText(get(t, advisor, base, http.StatusOK)); !strings.Contains(page, "Balancing") || strings.Contains(page, "Offer an extra") {
		t.Errorf("the front desk's price list:\n%s", page)
	}
	if status := postStatus(t, tech, ts.URL+"/jobs/"+job+"/tyres/extra", url.Values{"extra": {extra}, "count": {"4"}}); status != http.StatusForbidden {
		t.Errorf("a technician adding an extra answered %d", status)
	}
	if status := getStatus(t, tech, base); status != http.StatusForbidden {
		t.Errorf("a technician reading the price list answered %d", status)
	}
	if page := visibleText(get(t, tech, ts.URL+"/jobs/"+job+"/tyres", http.StatusOK)); strings.Contains(page, "Balancing") || strings.Contains(page, "Add an extra") {
		t.Errorf("the technician's page shows the price list:\n%s", page)
	}
	if page := visibleText(get(t, advisor, ts.URL+"/jobs/"+job+"/tyres", http.StatusOK)); !strings.Contains(page, "Balancing") {
		t.Errorf("the front desk's page lacks the extra:\n%s", page)
	}
}

// The owner starts a seasonal campaign, anyone at the counter sees who it would
// reach, a customer is taken out from their own card, and running it twice
// queues one message.
func TestASeasonalCampaignIsPreviewedAndRunOnce(t *testing.T) {
	ts, pool := testServer(t)
	const customer = "aaaaaaaa-0000-0000-0000-000000000004"
	ctx := context.Background()
	owner, advisor, tech := signIn(t, ts, ownerEmail), signIn(t, ts, advisorEmail), signIn(t, ts, techEmail)
	if _, err := pool.Exec(ctx, `UPDATE people SET phone = '+46701234567' WHERE id = (SELECT person_id FROM customers WHERE id = $1)`, customer); err != nil {
		t.Fatal(err)
	}
	postStatus(t, owner, ts.URL+"/storage", url.Values{"name": {"Rack"}, "capacity": {"3"}})
	var rack, set string
	_ = pool.QueryRow(ctx, `SELECT id FROM storage_locations WHERE name = 'Rack'`).Scan(&rack)
	if err := pool.QueryRow(ctx, `INSERT INTO tyre_sets (shop_id, customer_id, season, brand, rims) VALUES ($1, $2, 'summer', 'Michelin', 'alloy') RETURNING id::text`, shopA, customer).Scan(&set); err != nil {
		t.Fatal(err)
	}
	postStatus(t, tech, ts.URL+"/tyres/"+set+"/place", url.Values{"place": {rack}})

	form := url.Values{"name": {"Spring"}, "season": {"summer"}, "body": {"Your tyres: {sets}"}}
	if status := postStatus(t, advisor, ts.URL+"/tyres/campaigns", form); status != http.StatusForbidden {
		t.Errorf("the front desk making a campaign answered %d", status)
	}
	if status := postStatus(t, owner, ts.URL+"/tyres/campaigns", form); status != http.StatusSeeOther {
		t.Fatalf("the owner making a campaign answered %d", status)
	}
	var id string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM tyre_campaigns WHERE name = 'Spring'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	page := visibleText(get(t, advisor, ts.URL+"/tyres/campaigns/"+id, http.StatusOK))
	if !strings.Contains(page, "would reach 1 customers") || !strings.Contains(page, "Your tyres: Michelin") {
		t.Errorf("the preview:\n%s", page)
	}
	if status := getStatus(t, tech, ts.URL+"/tyres/campaigns/"+id); status != http.StatusForbidden {
		t.Errorf("a technician previewing a campaign answered %d", status)
	}
	if status := postStatus(t, advisor, ts.URL+"/tyres/campaigns/"+id+"/run", url.Values{}); status != http.StatusForbidden {
		t.Errorf("the front desk running a campaign answered %d", status)
	}
	for i := 0; i < 2; i++ {
		if status := postStatus(t, owner, ts.URL+"/tyres/campaigns/"+id+"/run", url.Values{}); status != http.StatusSeeOther {
			t.Fatalf("run %d answered %d", i+1, status)
		}
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM outbound_messages WHERE campaign_id = $1`, id).Scan(&n)
	if n != 1 {
		t.Errorf("two runs queued %d messages", n)
	}
	if page := visibleText(get(t, advisor, ts.URL+"/tyres/campaigns/"+id, http.StatusOK)); !strings.Contains(page, "1 have been queued already") {
		t.Errorf("after running:\n%s", page)
	}

	if status := postStatus(t, advisor, ts.URL+"/customers/"+customer+"/message-choice", url.Values{"choice": {"out"}}); status != http.StatusSeeOther {
		t.Fatalf("opting a customer out answered %d", status)
	}
	if page := visibleText(get(t, advisor, ts.URL+"/customers/"+customer, http.StatusOK)); !strings.Contains(page, "This customer has opted out of the shop") {
		t.Errorf("the customer card:\n%s", page)
	}
	if status := postStatus(t, tech, ts.URL+"/customers/"+customer+"/message-choice", url.Values{"choice": {"out"}}); status != http.StatusForbidden {
		t.Errorf("a technician opting a customer out answered %d", status)
	}
}

// A change is booked from the set's page, shows on it and on the days page,
// and the technician sees neither the booking form nor the days.
func TestAChangeIsBookedFromTheSetsPageAndShowsOnTheDays(t *testing.T) {
	ts, pool := testServer(t)
	const customer = "aaaaaaaa-0000-0000-0000-000000000004"
	ctx := context.Background()
	owner, advisor, tech := signIn(t, ts, ownerEmail), signIn(t, ts, advisorEmail), signIn(t, ts, techEmail)
	postStatus(t, owner, ts.URL+"/storage", url.Values{"name": {"Rack"}, "capacity": {"3"}})
	var rack, set string
	_ = pool.QueryRow(ctx, `SELECT id FROM storage_locations WHERE name = 'Rack'`).Scan(&rack)
	if err := pool.QueryRow(ctx, `INSERT INTO tyre_sets (shop_id, customer_id, season, brand, rims) VALUES ($1, $2, 'summer', 'Michelin', 'alloy') RETURNING id::text`, shopA, customer).Scan(&set); err != nil {
		t.Fatal(err)
	}
	postStatus(t, tech, ts.URL+"/tyres/"+set+"/place", url.Values{"place": {rack}})

	page := visibleText(get(t, advisor, ts.URL+"/tyres/"+set, http.StatusOK))
	if !strings.Contains(page, "Book the change") {
		t.Errorf("the set's page offers no booking:\n%s", page)
	}
	if page := visibleText(get(t, tech, ts.URL+"/tyres/"+set, http.StatusOK)); strings.Contains(page, "Book the change") {
		t.Errorf("a technician is offered a booking:\n%s", page)
	}
	if status := postStatus(t, tech, ts.URL+"/tyres/"+set+"/book", url.Values{"when": {time.Now().AddDate(0, 0, 5).Format("2006-01-02") + "T10:00"}}); status != http.StatusForbidden {
		t.Errorf("a technician booking answered %d", status)
	}
	if status := postStatus(t, advisor, ts.URL+"/tyres/"+set+"/book", url.Values{"when": {"someday"}}); status != http.StatusBadRequest {
		t.Errorf("a day that is not one answered %d", status)
	}
	day := time.Now().AddDate(0, 0, 5)
	if status := postStatus(t, advisor, ts.URL+"/tyres/"+set+"/book", url.Values{"when": {day.Format("2006-01-02") + "T10:00"}}); status != http.StatusSeeOther {
		t.Fatalf("booking answered %d", status)
	}
	if status := postStatus(t, advisor, ts.URL+"/tyres/"+set+"/book", url.Values{"when": {day.Format("2006-01-02") + "T11:00"}}); status != http.StatusBadRequest {
		t.Errorf("booking a set twice answered %d", status)
	}
	if page := visibleText(get(t, advisor, ts.URL+"/tyres/"+set, http.StatusOK)); !strings.Contains(page, "Booked for a change on") || strings.Contains(page, "Book the change") {
		t.Errorf("the set's page after booking:\n%s", page)
	}
	if page := visibleText(get(t, advisor, ts.URL+"/tyres/changes", http.StatusOK)); !strings.Contains(page, "1 booked, 11 free") {
		t.Errorf("the days page:\n%s", page)
	}
	if status := getStatus(t, tech, ts.URL+"/tyres/changes"); status != http.StatusForbidden {
		t.Errorf("a technician reading the days answered %d", status)
	}
	if status := postStatus(t, advisor, ts.URL+"/tyres/changes/settings", url.Values{"minutes": {"20"}, "per_day": {"5"}, "follow_up": {"7"}}); status != http.StatusForbidden {
		t.Errorf("the front desk changing the settings answered %d", status)
	}
	if status := postStatus(t, owner, ts.URL+"/tyres/changes/settings", url.Values{"minutes": {"20"}, "per_day": {"5"}, "follow_up": {"7"}}); status != http.StatusSeeOther {
		t.Fatalf("the owner changing the settings answered %d", status)
	}
	if page := visibleText(get(t, owner, ts.URL+"/tyres/changes", http.StatusOK)); !strings.Contains(page, "1 booked, 4 free") {
		t.Errorf("the days page after changing the settings:\n%s", page)
	}
}

// A storage fee is made by the owner, charged on a job or on an invoice from the
// fees page, once for each period, and no technician reaches any of it.
func TestAStorageFeeIsChargedOnAJobOrInvoicedOnceAPeriod(t *testing.T) {
	ts, pool := testServer(t)
	const customer = "aaaaaaaa-0000-0000-0000-000000000004"
	const job = "aaaaaaaa-0000-0000-0000-00000000000f"
	ctx := context.Background()
	owner, advisor, tech := signIn(t, ts, ownerEmail), signIn(t, ts, advisorEmail), signIn(t, ts, techEmail)
	postStatus(t, owner, ts.URL+"/storage", url.Values{"name": {"Rack"}, "capacity": {"3"}})
	var rack, summer, winter, fee string
	_ = pool.QueryRow(ctx, `SELECT id FROM storage_locations WHERE name = 'Rack'`).Scan(&rack)
	for season, dst := range map[string]*string{"summer": &summer, "winter_friction": &winter} {
		if err := pool.QueryRow(ctx, `INSERT INTO tyre_sets (shop_id, customer_id, season, brand, rims) VALUES ($1, $2, $3, 'Brand', 'alloy') RETURNING id::text`, shopA, customer, season).Scan(dst); err != nil {
			t.Fatal(err)
		}
		postStatus(t, tech, ts.URL+"/tyres/"+*dst+"/place", url.Values{"place": {rack}})
	}

	form := url.Values{"name": {"Storage"}, "per": {"set"}, "kind": {"fee"}, "price": {"400"}, "vat": {"25"}, "storage": {"year"}}
	if status := postStatus(t, advisor, ts.URL+"/tyres/extras", form); status != http.StatusForbidden {
		t.Errorf("the front desk making a fee answered %d", status)
	}
	if status := postStatus(t, owner, ts.URL+"/tyres/extras", form); status != http.StatusSeeOther {
		t.Fatalf("the owner making a fee answered %d", status)
	}
	_ = pool.QueryRow(ctx, `SELECT id::text FROM tyre_extras WHERE name = 'Storage'`).Scan(&fee)

	if status := postStatus(t, tech, ts.URL+"/jobs/"+job+"/tyres/storage-fee", url.Values{"set": {summer}, "fee": {fee}, "year": {"2027"}}); status != http.StatusForbidden {
		t.Errorf("a technician charging storage answered %d", status)
	}
	if status := postStatus(t, advisor, ts.URL+"/jobs/"+job+"/tyres/storage-fee", url.Values{"set": {summer}, "fee": {fee}, "year": {"2027"}}); status != http.StatusSeeOther {
		t.Fatalf("charging the summer set answered %d", status)
	}
	if status := postStatus(t, advisor, ts.URL+"/jobs/"+job+"/tyres/storage-fee", url.Values{"set": {summer}, "fee": {fee}, "year": {"2027"}}); status != http.StatusBadRequest {
		t.Errorf("charging the same period twice answered %d", status)
	}
	page := visibleText(get(t, advisor, ts.URL+"/tyres/storage-fees?fee="+fee+"&year=2027", http.StatusOK))
	if !strings.Contains(page, "Invoice the storage") {
		t.Errorf("the winter set is still due:\n%s", page)
	}
	if status := getStatus(t, tech, ts.URL+"/tyres/storage-fees"); status != http.StatusForbidden {
		t.Errorf("a technician reading the fees page answered %d", status)
	}
	if status := postStatus(t, tech, ts.URL+"/tyres/storage-fees/invoice", url.Values{"customer": {customer}, "fee": {fee}, "year": {"2027"}}); status != http.StatusForbidden {
		t.Errorf("a technician invoicing storage answered %d", status)
	}
	if status := postStatus(t, advisor, ts.URL+"/tyres/storage-fees/invoice", url.Values{"customer": {customer}, "fee": {fee}, "year": {"2027"}}); status != http.StatusSeeOther {
		t.Fatalf("invoicing the storage answered %d", status)
	}
	var lines int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM invoice_lines l JOIN invoices i ON i.id = l.invoice_id WHERE i.customer_id = $1 AND l.description LIKE 'Storage%'`, customer).Scan(&lines)
	if lines != 1 {
		t.Errorf("the invoice has %d lines; the job already charged the other set", lines)
	}
	if status := postStatus(t, advisor, ts.URL+"/tyres/storage-fees/invoice", url.Values{"customer": {customer}, "fee": {fee}, "year": {"2027"}}); status != http.StatusBadRequest {
		t.Errorf("invoicing with nothing left answered %d", status)
	}
	if status := postStatus(t, owner, ts.URL+"/tyres/storage-fees/billing", url.Values{"billing": {"invoice"}}); status != http.StatusSeeOther {
		t.Errorf("choosing the billing answered %d", status)
	}
	_ = winter
}

// The owner is asked on a link shown once, answers without signing in, and the
// set is then disposed of with its fee on the job; the technician hands a set
// out at the counter but cannot ask or dispose.
func TestASetIsDisposedOfAfterTheOwnerSaysYesOnTheirLink(t *testing.T) {
	ts, pool := testServer(t)
	const customer = "aaaaaaaa-0000-0000-0000-000000000004"
	const job = "aaaaaaaa-0000-0000-0000-00000000000f"
	ctx := context.Background()
	owner, advisor, tech := signIn(t, ts, ownerEmail), signIn(t, ts, advisorEmail), signIn(t, ts, techEmail)
	postStatus(t, owner, ts.URL+"/storage", url.Values{"name": {"Rack"}, "capacity": {"3"}})
	var rack, set, fee string
	_ = pool.QueryRow(ctx, `SELECT id FROM storage_locations WHERE name = 'Rack'`).Scan(&rack)
	if err := pool.QueryRow(ctx, `INSERT INTO tyre_sets (shop_id, customer_id, season, brand, rims) VALUES ($1, $2, 'summer', 'Michelin', 'alloy') RETURNING id::text`, shopA, customer).Scan(&set); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tyres (shop_id, tyre_set_id, position, dimension) VALUES ($1, $2, 'front_left', '205/55R16')`, shopA, set); err != nil {
		t.Fatal(err)
	}
	postStatus(t, tech, ts.URL+"/tyres/"+set+"/place", url.Values{"place": {rack}})
	postStatus(t, owner, ts.URL+"/tyres/extras", url.Values{"name": {"Disposal"}, "per": {"tyre"}, "kind": {"fee"}, "price": {"50"}, "vat": {"25"}})
	_ = pool.QueryRow(ctx, `SELECT id::text FROM tyre_extras WHERE name = 'Disposal'`).Scan(&fee)

	if status := postStatus(t, tech, ts.URL+"/tyres/"+set+"/agreement", url.Values{"kind": {"disposal"}}); status != http.StatusForbidden {
		t.Errorf("a technician asking for an agreement answered %d", status)
	}
	page := get(t, advisor, ts.URL+"/tyres/"+set, http.StatusOK)
	if strings.Contains(visibleText(page), "Give the owner this address") {
		t.Error("an address is shown before one was made")
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/tyres/"+set+"/agreement", strings.NewReader(url.Values{"kind": {"disposal"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", ts.URL)
	resp, err := advisor.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	m := regexp.MustCompile(`/a/([A-Za-z0-9_-]{20,})`).FindSubmatch(body)
	if resp.StatusCode != http.StatusOK || m == nil {
		t.Fatalf("asking answered %d and showed no address:\n%s", resp.StatusCode, visibleText(string(body)))
	}
	token := string(m[1])
	if again := get(t, advisor, ts.URL+"/tyres/"+set, http.StatusOK); strings.Contains(again, token) {
		t.Error("the owner's address is shown again after the first time")
	}

	anon := &http.Client{}
	if q := visibleText(get(t, anon, ts.URL+"/a/"+token, http.StatusOK)); !strings.Contains(q, "disposed of") || strings.Contains(q, customerName) {
		t.Errorf("the owner's page:\n%s", q)
	}
	get(t, anon, ts.URL+"/a/not-a-token", http.StatusNotFound)
	var agreement string
	_ = pool.QueryRow(ctx, `SELECT id::text FROM tyre_set_agreements WHERE tyre_set_id = $1`, set).Scan(&agreement)
	dispose := url.Values{"agreement": {agreement}, "job": {job}, "fee": {fee}}
	if status := postStatus(t, advisor, ts.URL+"/tyres/"+set+"/dispose", dispose); status != http.StatusBadRequest {
		t.Errorf("disposing before the owner answered gave %d", status)
	}
	if status := postStatus(t, anon, ts.URL+"/a/"+token+"/answer", url.Values{"answer": {"yes"}}); status != http.StatusSeeOther {
		t.Fatalf("the owner's yes answered %d", status)
	}
	if status := postStatus(t, tech, ts.URL+"/tyres/"+set+"/dispose", dispose); status != http.StatusForbidden {
		t.Errorf("a technician disposing answered %d", status)
	}
	if status := postStatus(t, advisor, ts.URL+"/tyres/"+set+"/dispose", dispose); status != http.StatusSeeOther {
		t.Fatalf("disposing answered %d", status)
	}
	var qty, price int64
	if err := pool.QueryRow(ctx, `SELECT (quantity * 1000)::bigint, unit_price_minor FROM work_order_lines WHERE work_order_id = $1 AND description = 'Disposal'`, job).Scan(&qty, &price); err != nil || qty != 1000 || price != 5000 {
		t.Errorf("the fee line: %d at %d, %v", qty, price, err)
	}
	if p := visibleText(get(t, tech, ts.URL+"/tyres/"+set, http.StatusOK)); !strings.Contains(p, "Disposed of on") || strings.Contains(p, "Record the collection") {
		t.Errorf("the set after disposal:\n%s", p)
	}
	if status := postStatus(t, tech, ts.URL+"/tyres/"+set+"/collect", url.Values{"by": {"Somebody"}}); status != http.StatusBadRequest {
		t.Errorf("collecting a disposed set answered %d", status)
	}
}
