package server

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The owner takes the whole shop: started from the privacy page, written
// after the request, readable on disk by the service alone, and handed to
// the owner and nobody else.
func TestTheOwnerTakesTheWholeShopAsAFileOnlyTheyAreHanded(t *testing.T) {
	ts, _ := testServer(t)
	owner := signIn(t, ts, ownerEmail)
	desk := signIn(t, ts, advisorEmail)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/privacy/export", nil)
	req.Header.Set("Origin", ts.URL)
	resp, err := owner.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("starting the export answered %d", resp.StatusCode)
	}

	link := regexp.MustCompile(`href="(/exports/[0-9a-f-]{36})"`)
	var path string
	for i := 0; i < 50 && path == ""; i++ {
		resp, err := owner.Get(ts.URL + "/privacy")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if m := link.FindSubmatch(body); m != nil {
			path = string(m[1])
		} else {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if path == "" {
		t.Fatal("the export never finished")
	}

	if resp, _ := desk.Get(ts.URL + path); resp.StatusCode != http.StatusForbidden {
		t.Errorf("the front desk downloading it answered %d, want 403", resp.StatusCode)
	}
	resp, err = owner.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	z, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("the download is not a zip: %v", err)
	}
	names := []string{}
	for _, f := range z.File {
		names = append(names, f.Name)
	}
	if !strings.Contains(strings.Join(names, " "), "work_orders.csv") {
		t.Errorf("the export holds %v", names)
	}

	// Readable by the service's user alone.
	srvDir := exportDirOf(t, ts)
	files, _ := filepath.Glob(filepath.Join(srvDir, "*.zip"))
	if len(files) != 1 {
		t.Fatalf("%d export files on disk", len(files))
	}
	if st, _ := os.Stat(files[0]); st.Mode().Perm() != 0o600 {
		t.Errorf("the export is written %v, want rw------- ", st.Mode().Perm())
	}
}
