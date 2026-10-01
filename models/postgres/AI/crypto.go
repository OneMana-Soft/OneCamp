package models

// AES-256-GCM encryption for AI provider API keys, mirroring the
// import_oauth_tokens scheme (models/postgres/Import/tokenModel.go).
//
// Keys are encrypted at the application layer before they touch the
// database. The KEK is derived from AI_CONFIG_KEK (SHA-256 → 32 bytes)
// so operators can use any sufficiently long passphrase rather than a
// raw 32-byte literal. A dedicated KEK (separate from IMPORT_TOKEN_KEK)
// keeps blast radius small: a compromise of one secret store entry does
// not expose the other class of secrets.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"os"
)

// devFallbackKEK is used only when AI_CONFIG_KEK is unset. It exists so
// `make dev` starts without ceremony; production MUST set AI_CONFIG_KEK
// (validated at startup in service init). It has zero real entropy.
const devFallbackKEK = "onecamp-dev-ai-config-kek-please-override-in-production"

// aiKEK returns the 32-byte key derived from AI_CONFIG_KEK.
func aiKEK() []byte {
	v := os.Getenv("AI_CONFIG_KEK")
	if v == "" {
		v = devFallbackKEK
	}
	sum := sha256.Sum256([]byte(v))
	return sum[:]
}

// The dev-fallback check used to live here as IsUsingDevFallbackKEK, testing only
// os.Getenv("AI_CONFIG_KEK") == "". helpers.InspectKEK answers the same question as its Fallback
// field and several more besides — most importantly whether the value is an unsubstituted template
// placeholder, which "is it empty?" cannot see and which is how beta came to encrypt every provider
// key with a string committed to this repository. Removed rather than left beside its replacement,
// so there is one answer to "is this KEK usable" instead of two that agree until they do not.

// EncryptAPIKey returns nonce(12) || ciphertext || tag(16).
func EncryptAPIKey(plain string) ([]byte, error) {
	if plain == "" {
		return nil, errors.New("empty plaintext")
	}
	block, err := aes.NewCipher(aiKEK())
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, []byte(plain), nil), nil
}

// DecryptAPIKey splits the nonce off the front and verifies the tag.
func DecryptAPIKey(blob []byte) (string, error) {
	if len(blob) < 12+16 {
		return "", errors.New("ciphertext too short")
	}
	block, err := aes.NewCipher(aiKEK())
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonceSize := gcm.NonceSize()
	nonce, ct := blob[:nonceSize], blob[nonceSize:]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// DeriveSecret returns 32 bytes derived from the AI key-encryption key for one
// purpose, named by label (HMAC-SHA256, so labels cannot collide with each
// other or be reversed to the KEK). Used for keys that must never be stored,
// such as an agent's signing key: whoever holds only the database cannot
// recreate them.
func DeriveSecret(label string) [32]byte {
	mac := hmac.New(sha256.New, aiKEK())
	mac.Write([]byte("onecamp/derive/v1\x00" + label))
	var out [32]byte
	copy(out[:], mac.Sum(nil))
	return out
}
