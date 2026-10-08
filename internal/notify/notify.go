// Package notify sends a message to a customer through whichever provider a
// workshop has: email over SMTP or Lettermint's API, a text over 46elks.
//
// A provider is one small type that turns a Message into that provider's
// request, and adding another is adding one more. Nothing configured is the
// default and is not a degraded mode: the front desk sends from its own phone,
// as it always could, and records that it did.
//
// Keys come from the environment and nowhere else. They are never stored, so
// a database dump or a shop's export cannot carry them, and nothing here puts
// one in an error: an error from a provider says what went wrong, not with
// what.
package notify

import (
	"context"
	"errors"
	"strings"
	"unicode"
)

// Message is one thing to one customer.
type Message struct {
	To      string // an address, or a number in E.164
	Subject string // email only
	Text    string
}

// Sender is a provider.
type Sender interface {
	// Send hands the message over and returns the provider's own id for it.
	// Handed over is not delivered.
	Send(ctx context.Context, m Message) (string, error)
	// Name is the provider, for the log of what was sent how.
	Name() string
}

// ErrNumber is a number nothing can be sent to.
var ErrNumber = errors.New("notify: not a phone number")

// Phone turns a number as people write it in Sweden -- "070-111 22 33",
// "+46 70 111 22 33", "0046701112233" -- into E.164, "+46701112233", which is
// what a provider takes. A number already in another country's international
// form is kept. Anything else is refused here, at the form, rather than by the
// provider later, when nobody is looking.
func Phone(s string) (string, error) {
	var b strings.Builder
	for i, r := range strings.TrimSpace(s) {
		switch {
		case unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '+' && i == 0:
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '(' || r == ')' || r == '.':
		default:
			return "", ErrNumber
		}
	}
	n := b.String()
	switch {
	case strings.HasPrefix(n, "+"):
	case strings.HasPrefix(n, "00"):
		n = "+" + n[2:]
	case strings.HasPrefix(n, "0"):
		n = "+46" + n[1:]
	default:
		return "", ErrNumber
	}
	// E.164 is at most fifteen digits; a Swedish mobile is eleven with the 46.
	if digits := len(n) - 1; digits < 8 || digits > 15 {
		return "", ErrNumber
	}
	return n, nil
}

// Email checks an address enough to send to it: something, an at sign, a
// domain with a dot. Whether it is the customer's is what confirming it is for.
func Email(s string) (string, error) {
	s = strings.TrimSpace(s)
	at := strings.LastIndex(s, "@")
	if at < 1 || !strings.Contains(s[at:], ".") || strings.ContainsAny(s, " \r\n<>,") {
		return "", errors.New("notify: not an email address")
	}
	return s, nil
}
