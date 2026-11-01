package database

import (
	"context"
	"io"
	"net"
	"net/url"
	"os"
	"testing"
	"time"
)

// A free loopback port with nothing listening on it.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func testURL(t *testing.T) *url.URL {
	t.Helper()
	raw := os.Getenv("FREESMS_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("FREESMS_TEST_DATABASE_URL not set")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// The database comes up after the application has started: the application
// waits for it rather than exiting. This is the race that made the browser
// stack fail to start three times in four (#245).
func TestOpenWaitsForADatabaseThatIsNotUpYet(t *testing.T) {
	u := testURL(t)
	target := u.Host
	addr := freePort(t)
	u.Host = addr

	// Nothing listens for the first moment; then a plain TCP relay to the
	// real database appears on the same address.
	ready := make(chan net.Listener, 1)
	go func() {
		time.Sleep(700 * time.Millisecond)
		l, err := net.Listen("tcp", addr)
		if err != nil {
			ready <- nil
			return
		}
		ready <- l
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				d, err := net.Dial("tcp", target)
				if err != nil {
					return
				}
				defer d.Close()
				go io.Copy(d, c)
				io.Copy(c, d)
			}()
		}
	}()
	t.Cleanup(func() {
		if l := <-ready; l != nil {
			l.Close()
		}
	})

	pool, err := openWithin(context.Background(), u.String(), 20*time.Second)
	if err != nil {
		t.Fatalf("Open = %v, want it to wait for the database and reach it", err)
	}
	pool.Close()
}

// Waiting has a bound: a database that never comes is reported, not waited
// on for ever.
func TestOpenGivesUpOnADatabaseThatNeverComes(t *testing.T) {
	start := time.Now()
	_, err := openWithin(context.Background(), "postgres://x:y@"+freePort(t)+"/z?sslmode=disable", time.Second)
	took := time.Since(start)
	if err == nil {
		t.Fatal("Open succeeded with nothing listening")
	}
	if took < 500*time.Millisecond {
		t.Errorf("gave up after %v, before trying again", took)
	}
	if took > 5*time.Second {
		t.Errorf("took %v to give up on a one-second bound", took)
	}
}

// A wrong password will not get better by waiting, so it is reported at once.
func TestOpenDoesNotWaitOnAWrongPassword(t *testing.T) {
	u := testURL(t)
	u.User = url.UserPassword(u.User.Username(), "not-the-password")
	start := time.Now()
	_, err := openWithin(context.Background(), u.String(), 30*time.Second)
	if err == nil {
		t.Fatal("Open succeeded with the wrong password")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("waited %v on a wrong password", took)
	}
}
