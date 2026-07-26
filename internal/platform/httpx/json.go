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

	setSafetyHeaders(w)
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// setSafetyHeaders applies the headers every API response carries.
//
// no-store is not optional. An authenticated body caches in an intermediary or
// a CDN exactly as happily as a public one, and §6.8 already requires it on
// subscription responses for the same reason. Referrer-Policy keeps a token in
// a path from travelling to whatever the page links to next.
func setSafetyHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "private, no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
}

// writeError emits a minimal error envelope without going back through
// WriteJSON, so a failure here cannot recurse.
//
// It carries the same headers as a success. This is the response that fires
// when a credential nearly leaked, so it must not be the least protected one
// the package emits.
func writeError(w http.ResponseWriter, status int, code string) {
	setSafetyHeaders(w)
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
