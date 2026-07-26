package secret

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"text/template"
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

// TestDiscloseIsTheOnlyWayOut confirms out-of-band senders can still do their
// job. A type nobody can read from would just be routed around.
func TestDiscloseIsTheOnlyWayOut(t *testing.T) {
	if got := Disclose(New(token)); got != token {
		t.Errorf("Disclose() = %q, want the original value", got)
	}
	if got := Disclose(Secret{}); got != "" {
		t.Errorf("Disclose(zero) = %q, want the empty string", got)
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
	if Disclose(req.Password) != "hunter2" {
		t.Errorf("inbound value = %q, want hunter2", Disclose(req.Password))
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

// unexportedHolder is the ordinary way a Go type holds a credential: an
// unexported field on a client or config struct.
type unexportedHolder struct {
	apiKey Secret
	Email  string
}

// TestUnexportedFieldDoesNotLeak covers the path that defeats Stringer.
//
// fmt cannot call String on a value it reached through an unexported field --
// reflect will not surrender it -- so fmt descends and prints the field's
// contents directly. That makes "%+v" on a struct holding an unexported
// credential, which is the most routine debug log in Go, a full disclosure
// unless the credential is stored behind a pointer.
func TestUnexportedFieldDoesNotLeak(t *testing.T) {
	h := unexportedHolder{apiKey: New(token), Email: "user@example.com"}

	for _, format := range []string{"%v", "%+v", "%#v"} {
		if got := fmt.Sprintf(format, h); strings.Contains(got, token) {
			t.Errorf("format %s leaked through an unexported field: %s", format, got)
		}
	}
}

// TestEveryVerbIsSafe covers the verbs Stringer does not reach. fmt routes
// only %v %s %q %x %X through String; everything else lands in badVerb, which
// prints the underlying value.
func TestEveryVerbIsSafe(t *testing.T) {
	s := New(token)
	verbs := []string{"%d", "%f", "%t", "%c", "%p", "%b", "%o", "%e", "%U", "%T", "%s", "%v", "%q", "%x", "%X", "%+v", "%#v"}

	for _, verb := range verbs {
		if got := fmt.Sprintf(verb, s); strings.Contains(got, token) {
			t.Errorf("verb %s leaked the value: %s", verb, got)
		}
	}
}

// TestTemplateCannotReachTheValue covers the reflective path. Templates are
// stored in the database and editable by staff (§2.2, §12.2), so an exported
// zero-argument accessor is reachable by anyone who can edit one.
func TestTemplateCannotReachTheValue(t *testing.T) {
	data := struct{ Token Secret }{Token: New(token)}

	// Every exported method on Secret, plus the field itself.
	for _, src := range []string{
		"{{.Token}}",
		"{{.Token.String}}",
		"{{.Token.GoString}}",
		"{{.Token.Disclose}}",
		"{{.Token.Reveal}}",
		"{{.Token.IsZero}}",
		"{{printf \"%v\" .Token}}",
		"{{printf \"%d\" .Token}}",
	} {
		var buf bytes.Buffer
		tpl, err := template.New("t").Parse(src)
		if err != nil {
			continue
		}
		// An error is a fine outcome -- it means the method does not exist.
		if err := tpl.Execute(&buf, data); err != nil {
			continue
		}
		if strings.Contains(buf.String(), token) {
			t.Errorf("template %q leaked the value: %s", src, buf.String())
		}
	}
}

// TestSlogRedactsUnderEveryHandler pins handler-independent redaction. Without
// LogValuer, whether a Secret is redacted depends on which handler is
// configured.
func TestSlogRedactsUnderEveryHandler(t *testing.T) {
	s := New(token)

	handlers := map[string]func(*bytes.Buffer) slog.Handler{
		"text": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
		"json": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
	}

	for name, newHandler := range handlers {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			log := slog.New(newHandler(&buf))

			log.Info("direct", slog.Any("secret", s))
			log.Info("nested", slog.Any("holder", unexportedHolder{apiKey: s, Email: "e"}))

			if strings.Contains(buf.String(), token) {
				t.Errorf("the %s handler leaked the value: %s", name, buf.String())
			}
		})
	}
}
