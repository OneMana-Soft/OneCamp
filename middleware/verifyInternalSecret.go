package middleware

import (
	"crypto/subtle"
	"net/http"
	"os"

	"github.com/akashc777/OneCamp/helpers"
)

func VerifyInternalServiceRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "OPTIONS" {
			next.ServeHTTP(w, r)
			return
		}
		internalSecret := os.Getenv("INTERNAL_SECRET")
		if internalSecret == "" {
			// If internal secret is not configured, deny all internal requests for security
			helpers.MessageLogs.ErrorLog.Println("middleware/VerifyInternalServiceRequest INTERNAL_SECRET env var is not set")
			w.WriteHeader(http.StatusForbidden)
			return
		}

		requestSecret := r.Header.Get("X-Internal-Secret")
		if requestSecret == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		// Constant-time, because `!=` on strings returns as soon as two bytes differ and so leaks the
		// length of the matching prefix through response timing. That is a practical attack against a
		// value an attacker can submit repeatedly, and this endpoint has no rate limit in front of it.
		//
		// business/AI/codePRLLMProxy.go already compared its shared secret with
		// subtle.ConstantTimeCompare, so the codebase held both the right answer and the wrong one for
		// the same kind of value. This is now the one shape.
		if subtle.ConstantTimeCompare([]byte(requestSecret), []byte(internalSecret)) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}
