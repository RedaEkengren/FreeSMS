package server

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The whole way, through the pages: the owner adds a technician, who signs in
// with the password handed over, is kept out of the staff page, and changes
// that password on their own.
func TestAnOwnerAddsATechnicianThroughThePage(t *testing.T) {
	ts, _ := testServer(t)
	owner := signIn(t, ts, ownerEmail)

	post := func(c *http.Client, path string, form url.Values) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", ts.URL)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	status, _ := post(owner, "/staff", url.Values{"name": {"Erik Mekaniker"}, "email": {"erik@shop-a.test"},
		"role": {"technician"}, "password": {techPass}})
	if status != http.StatusSeeOther {
		t.Fatalf("adding a technician answered %d", status)
	}
	// The same email again comes back with what was typed -- not the password.
	status, body := post(owner, "/staff", url.Values{"name": {"Another Erik"}, "email": {"erik@shop-a.test"},
		"role": {"parts"}, "password": {"secret typed once"}})
	if status != http.StatusBadRequest || !strings.Contains(body, `value="Another Erik"`) {
		t.Errorf("a duplicate email answered %d without the form as typed", status)
	}
	if strings.Contains(body, "secret typed once") {
		t.Error("the refused page wrote the password back into itself")
	}

	erik := signIn(t, ts, "erik@shop-a.test")
	resp, err := erik.Get(ts.URL + "/staff")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a technician opening the staff page got %d, want 403", resp.StatusCode)
	}

	if status, body := post(erik, "/account/password", url.Values{"current": {techPass}, "password": {"Eriks eget lösenord"}}); status != http.StatusOK ||
		!strings.Contains(body, "Changed.") {
		t.Errorf("changing one's own password answered %d", status)
	}
	// Still signed in where it was changed.
	resp, _ = erik.Get(ts.URL + "/account")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("signed out by changing the password here: %d", resp.StatusCode)
	}
}

// A role whose module is switched off stays with the person (#145), and the
// roles form must say so: it posts the whole set back and SetStaffRoles
// writes what arrives. Left off the form, a salesperson's main role would
// fall back to the first option and their buyer's role would go the next
// time anybody ticked anything.
func TestTheRolesFormKeepsARoleWhoseModuleIsOff(t *testing.T) {
	ts, pool := testServer(t)
	seedShop(t, pool, shopA, []string{
		`INSERT INTO people (id, shop_id, display_name, email) VALUES
		 ('aaaaaaaa-0000-0000-0000-0000000000c1','` + shopA + `','A Salesperson','sales@shop-a.test')`,
		`INSERT INTO users (id, shop_id, person_id, role, password_hash) VALUES
		 ('aaaaaaaa-0000-0000-0000-0000000000c2','` + shopA + `','aaaaaaaa-0000-0000-0000-0000000000c1','sales','x')`,
		`INSERT INTO user_roles (shop_id, user_id, role) VALUES
		 ('` + shopA + `','aaaaaaaa-0000-0000-0000-0000000000c2','buyer')`,
	})
	owner := signIn(t, ts, ownerEmail)
	resp, err := owner.Get(ts.URL + "/staff")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	body := string(b)
	start := strings.Index(body, `action="/staff/aaaaaaaa-0000-0000-0000-0000000000c2/roles"`)
	if start < 0 {
		t.Fatalf("no roles form for the salesperson (%d)", resp.StatusCode)
	}
	form := body[start:]
	form = form[:strings.Index(form, "</form>")]
	if !strings.Contains(form, `<option value="sales" selected>`) {
		t.Error("the roles form does not select the salesperson's own main role")
	}
	if !strings.Contains(form, `name="also" value="buyer" checked`) {
		t.Error("the roles form leaves off the buyer's role the person holds")
	}
	if strings.Contains(form, `value="rental_agent"`) {
		t.Error("the roles form offers a role from a module the shop does not run and the person does not hold")
	}
}
