package middleware

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
)

func TestWeakInternalSecretsAreRefused(t *testing.T) {
	for _, s := range []string{"", "short", "super-secret-key", "SUPER-SECRET-KEY", "__CHANGE_ME_RANDOM_64_CHARS__", "   changeme   "} {
		if !WeakInternalSecret(s) {
			t.Errorf("%q accepted as a secret", s)
		}
	}
	if WeakInternalSecret("Zx81kQp0LmN7vR2tY5uW9aB3cD6eF4gH") {
		t.Error("a random 32-character secret was refused")
	}
}

func TestInternalRoutesRefuseTheDefaultEvenWhenItMatches(t *testing.T) {
	if helpers.MessageLogs == nil {
		d := log.New(io.Discard, "", 0)
		helpers.MessageLogs = &helpers.Message{InfoLog: d, ErrorLog: d}
	}
	reached := false
	h := VerifyInternalServiceRequest(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	call := func(configured, presented string) int {
		t.Setenv("INTERNAL_SECRET", configured)
		reached = false
		req := httptest.NewRequest(http.MethodGet, "/docColab/getDoc/x", nil)
		req.Header.Set("X-Internal-Secret", presented)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := call("super-secret-key", "super-secret-key"); code != http.StatusForbidden || reached {
		t.Fatalf("the public default opened an internal route: %d", code)
	}
	good := "Zx81kQp0LmN7vR2tY5uW9aB3cD6eF4gH"
	if code := call(good, "wrong"); code != http.StatusUnauthorized || reached {
		t.Fatalf("a wrong secret got through: %d", code)
	}
	if call(good, good); !reached {
		t.Fatal("the right secret was refused")
	}
}
