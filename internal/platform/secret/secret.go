// Package secret carries credentials that must never reach an API response.
//
// The rule this package enforces (§12.1): any value usable for authentication
// -- a magic link, a verification code, a password-reset token, an API key in
// the clear -- travels out of band only, and never appears in a response body.
//
// CVE-2026-39912 is what happens without that rule. A magic-login token was
// written straight into an HTTP response body, which handed anyone an
// account-takeover primitive against V2Board 1.6.1 through 1.7.4 and Xboard up
// to 0.1.9, administrators included. That was not an implementation slip; the
// architecture simply had no place where the rule could be stated.
//
// # How the value is protected
//
// A Secret is a pointer to an unexported box rather than a string field. That
// detail is load-bearing. fmt cannot call String on a value it reaches through
// an unexported field -- reflect refuses to hand it over -- so it descends and
// prints the field directly. With a string inside, "%+v" on any struct holding
// an unexported Secret prints the credential in full, and a struct with an
// unexported credential field logged at %+v is the single most ordinary thing
// in Go. With a pointer inside, the same path prints an address.
//
// Format covers the rest of fmt: without it only %v %s %q %x %X route through
// String, and %d on a Secret prints the underlying value through badVerb.
//
// There is no exported method that returns the credential. Disclose is a
// package-level function precisely because text/template and expr-lang reach
// exported methods by reflection: {{.Token.Reveal}} would have re-created the
// CVE through the template engine, and since §2.2 stores templates in the
// database where staff can edit them, that is a live path rather than a
// theoretical one. A package-level function is not reachable from a template.
package secret

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
)

// ErrSerialized is returned by MarshalJSON. It is an error, not a redaction,
// on purpose: silently emitting "[REDACTED]" would let a handler ship a
// response whose shape is wrong and whose bug nobody notices.
var ErrSerialized = errors.New("security: Secret must never be serialized into a response")

// redacted is what a Secret renders as, everywhere.
const redacted = "[REDACTED]"

// box holds the credential. Unexported, and reached only through a pointer, so
// no reflective printer can render its contents.
type box struct{ v string }

// Secret wraps a credential. The zero value is an empty secret.
type Secret struct{ b *box }

// New wraps a credential value.
func New(v string) Secret { return Secret{b: &box{v: v}} }

// Disclose returns the underlying credential.
//
// This is the one way out, and it is reserved for out-of-band senders: the
// mailer, the Telegram notifier, the configuration writer. It is a function
// rather than a method so that no template engine can reach it, and so that
// every call site names the package -- which is what makes the CI guard in
// guard_test.go able to find them.
func Disclose(s Secret) string {
	if s.b == nil {
		return ""
	}
	return s.b.v
}

// IsZero reports whether the secret is empty.
func (s Secret) IsZero() bool { return s.b == nil || s.b.v == "" }

// String renders a placeholder so a Secret caught in a log line, a %v, or a
// panic message discloses nothing.
func (s Secret) String() string { return redacted }

// GoString covers %#v, which does not route through String.
func (s Secret) GoString() string { return "secret.Secret{" + redacted + "}" }

// Format makes every fmt verb safe, not just the string-shaped ones.
//
// fmt sends only a handful of verbs through Stringer; anything else -- %d on a
// Secret, most often from a format string that go vet never sees because it
// came from configuration -- falls through to badVerb, which prints the
// underlying value. Implementing Formatter takes precedence over both Stringer
// and GoStringer, so there is one answer for every verb.
func (s Secret) Format(f fmt.State, verb rune) {
	switch verb {
	case 'v':
		if f.Flag('#') {
			_, _ = f.Write([]byte(s.GoString()))
			return
		}
		fallthrough
	default:
		_, _ = f.Write([]byte(redacted))
	}
}

// LogValue redacts a Secret for log/slog regardless of which handler is in
// use.
//
// Without it, redaction depends on the handler: the JSON handler happens to
// surface the MarshalJSON error and the text handler happens to call String.
// LogValuer is the interface every handler honours.
func (s Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON always fails.
//
// Because encoding/json aborts the whole encode when any field errors, a
// Secret reachable from a response struct takes the entire response down
// rather than leaking one field. That is only safe if the response writer
// marshals into a buffer before touching the ResponseWriter -- streaming the
// encode would emit a half-written body with a 200 already on the wire. See
// httpx.WriteJSON.
func (s Secret) MarshalJSON() ([]byte, error) { return nil, ErrSerialized }

// UnmarshalJSON accepts a JSON string, so inbound request structs can carry a
// credential the user is submitting.
//
// Inbound is not the dangerous direction: the risk is credentials leaving, not
// arriving.
func (s *Secret) UnmarshalJSON(data []byte) error {
	var v string
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	s.b = &box{v: v}
	return nil
}

var (
	_ json.Marshaler   = Secret{}
	_ json.Unmarshaler = (*Secret)(nil)
	_ fmt.Formatter    = Secret{}
	_ fmt.Stringer     = Secret{}
	_ slog.LogValuer   = Secret{}
)
