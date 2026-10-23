package helpers

import (
	"bytes"
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

// NewSealedLinkSecret is a fresh link secret for a link its holder may be
// sent again: the hash it is found by, and the secret sealed with the
// server's key (EncryptSecret), which only the server opens.
func NewSealedLinkSecret() (raw string, hash []byte, sealed string, err error) {
	if raw, hash, err = NewLinkSecret(); err != nil {
		return "", nil, "", err
	}
	if sealed, err = EncryptSecret(raw); err != nil {
		return "", nil, "", err
	}
	return raw, hash, sealed, nil
}

// OpenLinkSecret is a sealed link secret, opened: "" when the seal doesn't
// open (the server's key changed) or holds another secret than the one the
// hash finds.
func OpenLinkSecret(sealed string, hash []byte) string {
	raw, err := DecryptSecret(sealed)
	if err != nil || raw == "" || !bytes.Equal(LinkSecretHash(raw), hash) {
		return ""
	}
	return raw
}
