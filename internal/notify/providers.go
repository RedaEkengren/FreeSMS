package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"strings"
	"time"
)

var client = &http.Client{Timeout: 20 * time.Second}

// Lettermint sends email over its API: POST /v1/send with the project's
// token in x-lettermint-token, answered 202 with a message id.
type Lettermint struct {
	URL   string // https://api.lettermint.co, or a test server
	Token string
	From  string
}

func (l Lettermint) Name() string { return "lettermint" }

func (l Lettermint) Send(ctx context.Context, m Message) (string, error) {
	body, _ := json.Marshal(map[string]any{"from": l.From, "to": []string{m.To}, "subject": m.Subject, "text": m.Text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(l.URL, "/")+"/v1/send", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-lettermint-token", l.Token)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("lettermint: %w", err)
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("lettermint answered %d: %s", resp.StatusCode, strings.TrimSpace(string(answer)))
	}
	var got struct {
		MessageID string `json:"message_id"`
	}
	_ = json.Unmarshal(answer, &got)
	return got.MessageID, nil
}

// Elks sends a text over 46elks: POST /a1/sms with basic auth and the form
// fields from, to and message. A delivery report comes back to Callback, if
// set, as whendelivered.
type Elks struct {
	URL      string // https://api.46elks.com, or a test server
	Username string
	Password string
	From     string
	Callback string
}

func (e Elks) Name() string { return "46elks" }

func (e Elks) Send(ctx context.Context, m Message) (string, error) {
	form := url.Values{"from": {e.From}, "to": {m.To}, "message": {m.Text}}
	if e.Callback != "" {
		form.Set("whendelivered", e.Callback)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(e.URL, "/")+"/a1/sms", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(e.Username, e.Password)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("46elks: %w", err)
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("46elks answered %d: %s", resp.StatusCode, strings.TrimSpace(string(answer)))
	}
	var got struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(answer, &got)
	return got.ID, nil
}

// SMTP sends email through whatever mail server the workshop already has --
// its own account's, usually. The address is smtp://user:password@host:port;
// the password is only ever read out of it to log in.
type SMTP struct {
	Addr     string // host:port
	Username string
	Password string
	From     string
}

// ParseSMTP reads smtp://user:password@host:port.
func ParseSMTP(raw, from string) (SMTP, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "smtp" || u.Host == "" {
		// Not the parse error: it would repeat the URL, password and all.
		return SMTP{}, fmt.Errorf("SMTP_URL must be smtp://user:password@host:port")
	}
	s := SMTP{Addr: u.Host, From: from}
	if u.Port() == "" {
		s.Addr = net.JoinHostPort(u.Hostname(), "587")
	}
	if u.User != nil {
		s.Username = u.User.Username()
		s.Password, _ = u.User.Password()
	}
	return s, nil
}

func (s SMTP) Name() string { return "smtp" }

func (s SMTP) Send(ctx context.Context, m Message) (string, error) {
	id := fmt.Sprintf("<%d.freesms@%s>", time.Now().UnixNano(), hostOf(s.From))
	var msg bytes.Buffer
	fmt.Fprintf(&msg, "From: %s\r\nTo: %s\r\nSubject: %s\r\nMessage-ID: %s\r\nDate: %s\r\n",
		s.From, m.To, mime.QEncoding.Encode("utf-8", m.Subject), id, time.Now().Format(time.RFC1123Z))
	msg.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
	msg.WriteString(strings.ReplaceAll(m.Text, "\n", "\r\n"))
	var auth smtp.Auth
	if s.Username != "" {
		// net/smtp refuses to send a password unencrypted, except to the
		// machine itself: a server that offers no TLS gets no password.
		auth = smtp.PlainAuth("", s.Username, s.Password, hostOnly(s.Addr))
	}
	done := make(chan error, 1)
	go func() { done <- smtp.SendMail(s.Addr, auth, addressOf(s.From), []string{m.To}, msg.Bytes()) }()
	select {
	case err := <-done:
		if err != nil {
			return "", fmt.Errorf("smtp: %w", err)
		}
		return id, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func hostOnly(addr string) string {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return h
}

// addressOf is the bare address in "Name <a@b>".
func addressOf(from string) string {
	if i, j := strings.LastIndex(from, "<"), strings.LastIndex(from, ">"); i >= 0 && j > i {
		return from[i+1 : j]
	}
	return from
}

func hostOf(from string) string {
	a := addressOf(from)
	if i := strings.LastIndex(a, "@"); i >= 0 {
		return a[i+1:]
	}
	return "localhost"
}
