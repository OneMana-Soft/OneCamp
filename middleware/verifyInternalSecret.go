package middleware

import (
	"crypto/subtle"
	"net/http"
	"os"
	"strings"

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

		// A secret anyone can read is not a secret. The collaboration service once fell back to a
		// literal in its public source, and an install left on it would open every document and board
		// to anyone who read that file. A known default or a short value is refused outright, with a
		// log line that says what to do, rather than accepted quietly.
		if WeakInternalSecret(internalSecret) {
			helpers.MessageLogs.ErrorLog.Println("middleware/VerifyInternalServiceRequest INTERNAL_SECRET is a public default or too short; set a random value of at least 32 characters (make secrets) and restart")
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

// knownDefaultSecrets are values that have appeared in OneCamp's own source or
// samples. Compared case-insensitively.
var knownDefaultSecrets = []string{"super-secret-key", "changeme", "change-me", "secret", "internal-secret"}

// WeakInternalSecret reports whether a configured INTERNAL_SECRET is a known
// default or shorter than 16 characters. Exported for its test.
func WeakInternalSecret(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 16 {
		return true
	}
	for _, d := range knownDefaultSecrets {
		if strings.EqualFold(s, d) {
			return true
		}
	}
	return strings.HasPrefix(s, "__") && strings.HasSuffix(s, "__")
}
