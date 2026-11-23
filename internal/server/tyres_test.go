package server

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

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
