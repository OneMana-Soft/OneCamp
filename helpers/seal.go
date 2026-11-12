package helpers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

// Seal and Unseal are the server's one way to hand out a body it must find
// unchanged when it comes back (a card's token, the recording stage's
// ticket): the body's JSON, base64url, ".", and the base64url HMAC-SHA256 of
// that JSON under the key for its purpose alone (PurposeKey). Nothing in it is
// hidden, only sealed, and it is safe in an address's path. Expiry and single
// use are the caller's: each body says what it needs.
var (
	// ErrSealBroken: not a token sealed for this purpose (forged, changed,
	// cut, or another purpose's).
	ErrSealBroken = errors.New("helpers: that sealed token isn't one of ours")
	// ErrNoSealKey: the server has no secret (JWT_SECRET) to seal with.
	ErrNoSealKey = errors.New("helpers: the server has no secret to seal with")
)

var sealEncoding = base64.RawURLEncoding

// Seal seals body for purpose.
func Seal(purpose string, body any) (string, error) {
	key, err := PurposeKey(purpose)
	if err != nil {
		return "", ErrNoSealKey
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	return sealEncoding.EncodeToString(raw) + "." + sealEncoding.EncodeToString(sealOf(key, raw)), nil
}

// Unseal reads into what Seal sealed for purpose, refusing anything else with
// ErrSealBroken.
func Unseal(purpose, token string, into any) error {
	key, err := PurposeKey(purpose)
	if err != nil {
		return ErrNoSealKey
	}
	body64, sig64, ok := strings.Cut(token, ".")
	if !ok {
		return ErrSealBroken
	}
	raw, err1 := sealEncoding.DecodeString(body64)
	sig, err2 := sealEncoding.DecodeString(sig64)
	if err1 != nil || err2 != nil || !hmac.Equal(sig, sealOf(key, raw)) {
		return ErrSealBroken
	}
	if json.Unmarshal(raw, into) != nil {
		return ErrSealBroken
	}
	return nil
}

// sealOf is body's HMAC-SHA256 under key.
func sealOf(key, body []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(body)
	return m.Sum(nil)
}
