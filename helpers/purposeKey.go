package helpers

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"os"
)

// PurposeKey is a key for one purpose alone, derived from this server's
// secret: an HMAC of the purpose under JWT_SECRET. The secret signs sessions,
// so a key made for one purpose never works as a session's or as another
// purpose's: business/Cards seals cards under "onecamp.cards.v1", and
// AnonymousKey keys people under "onecamp.anonymous.v1". No secret, no key.
func PurposeKey(purpose string) ([]byte, error) {
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		return nil, errors.New("JWT_SECRET is not configured")
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(purpose))
	return m.Sum(nil), nil
}
