package secret

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const token = "lp_magic_7f3a2e91c4b8"

// TestSecretRefusesToSerialize is the assertion that stands in for
// CVE-2026-39912. A magic-login token written into a response body handed
// anyone an account-takeover primitive; here that attempt fails at the
// marshaller.
func TestSecretRefusesToSerialize(t *testing.T) {
	_, err := json.Marshal(New(token))
	if err == nil {
		t.Fatal("a Secret serialized successfully")
	}
	if !strings.Contains(err.Error(), ErrSerialized.Error()) {
		t.Errorf("error %q does not mention the security rule", err)
	}
}

// TestSecretTakesDownTheWholeResponse checks the property the runtime backstop
// depends on: encoding/json aborts the entire encode rather than emitting the
// other fields and skipping the secret. A partial response would be a silent
// leak of everything around it plus a 200 nobody questions.
func TestSecretTakesDownTheWholeResponse(t *testing.T) {
	resp := struct {
		Email string `json:"email"`
		Token Secret `json:"token"`
	}{Email: "user@example.com", Token: New(token)}

	out, err := json.Marshal(resp)
	if err == nil {
		t.Fatalf("the response marshalled: %s", out)
	}
	if strings.Contains(string(out), token) {
		t.Fatal("the token appeared in the partial output")
	}
}

// TestSecretNeverFormatsItsValue covers the accidental paths: a log line, a
// %v in an error, a panic message, a struct dump.
func TestSecretNeverFormatsItsValue(t *testing.T) {
	s := New(token)
	for _, format := range []string{"%s", "%v", "%+v", "%#v", "%q"} {
		got := fmt.Sprintf(format, s)
		if strings.Contains(got, token) {
			t.Errorf("format %s leaked the value: %s", format, got)
		}
	}

	wrapper := struct{ Key Secret }{Key: s}
	for _, format := range []string{"%v", "%+v"} {
		if got := fmt.Sprintf(format, wrapper); strings.Contains(got, token) {
			t.Errorf("nested format %s leaked the value: %s", format, got)
		}
	}
}

// TestRevealIsTheOnlyWayOut confirms out-of-band senders can still do their
// job. A type nobody can read from would just be routed around.
func TestRevealIsTheOnlyWayOut(t *testing.T) {
	if got := New(token).Reveal(); got != token {
		t.Errorf("Reveal() = %q, want the original value", got)
	}
}

func TestSecretRoundTripsInbound(t *testing.T) {
	// Inbound is the safe direction: a user submitting a credential must be
	// able to send one.
	var req struct {
		Password Secret `json:"password"`
	}
	if err := json.Unmarshal([]byte(`{"password":"hunter2"}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.Password.Reveal() != "hunter2" {
		t.Errorf("inbound value = %q, want hunter2", req.Password.Reveal())
	}
}

func TestZeroValue(t *testing.T) {
	var s Secret
	if !s.IsZero() {
		t.Error("the zero Secret does not report itself empty")
	}
	if s.String() != "[REDACTED]" {
		t.Errorf("String() = %q, want the placeholder even when empty", s.String())
	}
	if _, err := json.Marshal(s); !errors.Is(err, ErrSerialized) && err == nil {
		t.Error("an empty Secret serialized; emptiness is not a licence to leak")
	}
}
