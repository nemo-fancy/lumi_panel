// Package httpx holds the response primitives every handler is required to
// use.
package httpx

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
)

// WriteJSON marshals v into a buffer and only then writes the status line,
// headers and body.
//
// This is the runtime backstop for the out-of-band credential rule (§12.1).
// json.Encoder streaming straight to the ResponseWriter would commit a 200
// before discovering that a secret.Secret refused to marshal, leaving the
// client holding half a JSON document -- and, worse, leaving whatever was
// encoded before the failure already on the wire.
//
// A marshal failure is a programming error, so it is logged at CRITICAL and
// answered with a clean 500 that discloses nothing.
func WriteJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		slog.LogAttrs(r.Context(), slog.LevelError, "CRITICAL: response marshal failed",
			slog.String("path", r.URL.Path),
			slog.String("error", err.Error()),
		)
		writeError(w, http.StatusInternalServerError, "internal_error")
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// writeError emits a minimal error envelope without going back through
// WriteJSON, so a failure here cannot recurse.
func writeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":{"code":"` + code + `"}}`))
}

// WriteErrorCode emits an enumerated error code.
//
// Production responses never carry a stack trace or SQL text (§12.4), and an
// authorization failure never reveals whether the resource exists (§12.3).
func WriteErrorCode(w http.ResponseWriter, status int, code string) {
	writeError(w, status, code)
}
