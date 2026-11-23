package server

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
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
