package helpers

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// A link that is someone's credential (a guest's link to a meeting or a doc,
// an invitee's link to answer an invitation) carries a secret: 32 random
// bytes, URL-safe. Only its SHA-256 is kept in the clear, and the link is
// found by it, so a copy of the database opens nothing.

// NewLinkSecret is a fresh link secret and the hash it is kept and found by.
func NewLinkSecret() (raw string, hash []byte, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	raw = base64.RawURLEncoding.EncodeToString(b)
	return raw, LinkSecretHash(raw), nil
}

// LinkSecretHash is the hash a link's secret is kept and found by. Pure.
func LinkSecretHash(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}
