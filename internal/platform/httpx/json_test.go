package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nemo-fancy/lumi_panel/internal/platform/secret"
)

const token = "lp_magic_7f3a2e91c4b8"

func request() *http.Request {
	return httptest.NewRequest(http.MethodGet, "/api/v1/user/profile", nil)
}

// TestSecretBearingResponseFailsCleanly is the §12.1 runtime backstop, end to
// end.
//
// Marshalling into a buffer first is what makes it work. Streaming the encode
// straight to the ResponseWriter would commit a 200 before discovering that a
// Secret refused to marshal, leaving the client holding half a JSON document
// -- with everything encoded before the failure already on the wire.
func TestSecretBearingResponseFailsCleanly(t *testing.T) {
	rec := httptest.NewRecorder()

	WriteJSON(rec, request(), http.StatusOK, struct {
		Email string        `json:"email"`
		Token secret.Secret `json:"token"`
	}{Email: "user@example.com", Token: secret.New(token)})

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, token) {
		t.Fatalf("the token reached the response body: %s", body)
	}
	// Not even the fields encoded before the failure may appear.
	if strings.Contains(body, "user@example.com") {
		t.Errorf("a partially encoded body was written: %s", body)
	}
	if body != `{"error":{"code":"internal_error"}}` {
		t.Errorf("body = %s, want a clean error envelope", body)
	}
}

func TestSuccessfulResponse(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, request(), http.StatusOK, map[string]string{"hello": "world"})

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if got["hello"] != "world" {
		t.Errorf("body = %v", got)
	}
}

// TestEveryResponseIsUncacheable covers success and failure alike. An
// authenticated body caches in an intermediary exactly as happily as a public
// one.
func TestEveryResponseIsUncacheable(t *testing.T) {
	cases := map[string]func(*httptest.ResponseRecorder){
		"success": func(rec *httptest.ResponseRecorder) {
			WriteJSON(rec, request(), http.StatusOK, map[string]string{"a": "b"})
		},
		"marshal failure": func(rec *httptest.ResponseRecorder) {
			WriteJSON(rec, request(), http.StatusOK, struct {
				T secret.Secret `json:"t"`
			}{T: secret.New(token)})
		},
		"explicit error": func(rec *httptest.ResponseRecorder) {
			WriteErrorCode(rec, http.StatusForbidden, "forbidden")
		},
	}

	for name, write := range cases {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			write(rec)

			for header, want := range map[string]string{
				"Cache-Control":          "private, no-store",
				"Referrer-Policy":        "no-referrer",
				"X-Content-Type-Options": "nosniff",
				"Content-Type":           "application/json; charset=utf-8",
			} {
				if got := rec.Header().Get(header); got != want {
					t.Errorf("%s = %q, want %q", header, got, want)
				}
			}
		})
	}
}

// TestErrorCodesDoNotLeakDetail pins §12.4: production responses carry an
// enumerated code and nothing else. No stack, no SQL, no hint about whether a
// resource exists.
func TestErrorCodesDoNotLeakDetail(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteErrorCode(rec, http.StatusForbidden, "forbidden")

	if body := rec.Body.String(); body != `{"error":{"code":"forbidden"}}` {
		t.Errorf("body = %s", body)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// TestNestedSecretAlsoFails checks that a credential buried below the top
// level is caught too -- encoding/json aborts the whole encode, so depth does
// not matter.
func TestNestedSecretAlsoFails(t *testing.T) {
	rec := httptest.NewRecorder()

	WriteJSON(rec, request(), http.StatusOK, map[string]any{
		"user": map[string]any{
			"sessions": []any{
				map[string]any{"reset": secret.New(token)},
			},
		},
	})

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), token) {
		t.Errorf("a nested credential reached the body: %s", rec.Body.String())
	}
}
