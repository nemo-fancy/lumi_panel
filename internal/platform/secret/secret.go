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
// architecture simply had no place where the rule could be stated. Here it is
// stated in the type system, so that violating it fails to compile, fails CI,
// or fails loudly at runtime -- never quietly on the wire.
package secret

import (
	"encoding/json"
	"errors"
)

// ErrSerialized is returned by MarshalJSON. It is an error, not a redaction,
// on purpose: silently emitting "[REDACTED]" would let a handler ship a
// response whose shape is wrong and whose bug nobody notices.
var ErrSerialized = errors.New("security: Secret must never be serialized into a response")

// Secret wraps a credential. The zero value is an empty secret.
type Secret struct{ v string }

// New wraps a credential value.
func New(v string) Secret { return Secret{v: v} }

// Reveal returns the underlying value.
//
// This is the one legitimate way out, and it is reserved for out-of-band
// senders: the mailer, the Telegram notifier, the config writer. CI forbids
// this identifier from appearing anywhere under internal/api or
// internal/subplane, which is the primary defence -- the type system is the
// backstop, not the other way round.
func (s Secret) Reveal() string { return s.v }

// IsZero reports whether the secret is empty.
func (s Secret) IsZero() bool { return s.v == "" }

// String renders a placeholder so a Secret caught in a log line, a %v, or a
// panic message discloses nothing.
func (s Secret) String() string { return "[REDACTED]" }

// GoString covers %#v, which does not route through String.
func (s Secret) GoString() string { return "secret.Secret{[REDACTED]}" }

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
func (s *Secret) UnmarshalJSON(b []byte) error {
	var v string
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	s.v = v
	return nil
}

var (
	_ json.Marshaler   = Secret{}
	_ json.Unmarshaler = (*Secret)(nil)
)
