package notify

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestASwedishNumberAsWrittenBecomesOneAProviderTakes(t *testing.T) {
	for in, want := range map[string]string{
		"070-111 22 33":    "+46701112233",
		"+46 70 111 22 33": "+46701112233",
		"0046701112233":    "+46701112233",
		"(070) 111.22.33":  "+46701112233",
		"+47 912 34 567":   "+4791234567",
		"08-123 456 78":    "+46812345678",
	} {
		if got, err := Phone(in); err != nil || got != want {
			t.Errorf("Phone(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "070-ABC", "12345", "70 111 22 33", "+46 7", "070+1112233"} {
		if got, err := Phone(bad); err == nil {
			t.Errorf("Phone(%q) = %q, want refused", bad, got)
		}
	}
}

func TestLettermintIsSentWhatItsAPIAsksFor(t *testing.T) {
	var got map[string]any
	var token string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/send" || r.Method != http.MethodPost {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		token = r.Header.Get("x-lettermint-token")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, `{"message_id":"lm-1","status":"pending"}`)
	}))
	defer srv.Close()
	id, err := Lettermint{URL: srv.URL, Token: "secret-token", From: "Verkstaden <hej@verkstad.se>"}.Send(context.Background(),
		Message{To: "kund@example.se", Subject: "Din bil är klar", Text: "Hämta den när du vill."})
	if err != nil || id != "lm-1" || token != "secret-token" {
		t.Fatalf("id %q, token %q, %v", id, token, err)
	}
	if got["subject"] != "Din bil är klar" || got["from"] != "Verkstaden <hej@verkstad.se>" || got["to"].([]any)[0] != "kund@example.se" {
		t.Errorf("sent %v", got)
	}
}

func TestElksIsSentWhatItsAPIAsksFor(t *testing.T) {
	var form url.Values
	var user, pass string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/a1/sms" {
			t.Errorf("path %s", r.URL.Path)
		}
		user, pass, _ = r.BasicAuth()
		_ = r.ParseForm()
		form = r.PostForm
		io.WriteString(w, `{"id":"s1234","status":"created"}`)
	}))
	defer srv.Close()
	id, err := Elks{URL: srv.URL, Username: "u", Password: "p", From: "Verkstaden", Callback: "https://v.example/hooks/46elks/x"}.Send(
		context.Background(), Message{To: "+46701112233", Text: "Din bil är klar."})
	if err != nil || id != "s1234" || user != "u" || pass != "p" {
		t.Fatalf("id %q, auth %q:%q, %v", id, user, pass, err)
	}
	if form.Get("to") != "+46701112233" || form.Get("from") != "Verkstaden" || form.Get("message") != "Din bil är klar." ||
		form.Get("whendelivered") != "https://v.example/hooks/46elks/x" {
		t.Errorf("sent %v", form)
	}
}

// What went wrong, never with what.
func TestAProviderErrorCarriesNoSecret(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	defer srv.Close()
	m := Message{To: "+46701112233", Text: "x"}
	_, err1 := Lettermint{URL: srv.URL, Token: "secret-token"}.Send(context.Background(), m)
	_, err2 := Elks{URL: srv.URL, Username: "u", Password: "secret-password"}.Send(context.Background(), m)
	_, err3 := ParseSMTP("smtp://u:secret-password@ host", "a@b.se")
	for _, err := range []error{err1, err2, err3} {
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Errorf("error %v", err)
		}
	}
}

// A mail server as small as one can be, to see what SMTP sends.
func TestSMTPSendsAPlainUTF8Message(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		say := func(s string) { io.WriteString(c, s+"\r\n") }
		say("220 test")
		var data strings.Builder
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				say("250-test")
				say("250 AUTH PLAIN")
			case strings.HasPrefix(cmd, "AUTH"):
				say("235 ok")
			case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
				say("250 ok")
			case cmd == "DATA":
				say("354 go")
				for {
					l, _ := r.ReadString('\n')
					if l == ".\r\n" {
						break
					}
					data.WriteString(l)
				}
				say("250 queued")
			case cmd == "QUIT":
				say("221 bye")
				got <- data.String()
				return
			default:
				say("250 ok")
			}
		}
	}()
	s, err := ParseSMTP("smtp://user:pw@"+ln.Addr().String(), "Verkstaden <hej@verkstad.se>")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(context.Background(), Message{To: "kund@example.se", Subject: "Din bil är klar", Text: "Hämta den.\nVälkommen."}); err != nil {
		t.Fatal(err)
	}
	msg := <-got
	for _, want := range []string{"To: kund@example.se", "Subject: =?utf-8?q?Din_bil_=C3=A4r_klar?=", "charset=utf-8", "Hämta den.\r\nVälkommen."} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message lacks %q:\n%s", want, msg)
		}
	}
}
