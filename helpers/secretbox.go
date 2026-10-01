package helpers

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"os"
)

// secretbox.go — application-layer AES-256-GCM encryption for secrets stored
// in Postgres (app signing secrets, third-party API keys, OAuth client
// secrets). This mirrors the proven import-token encryption (models/postgres/
// Import/tokenModel.go) but is exported and reusable across packages so every
// at-rest secret uses the same vetted primitive.
//
// Key management: the KEK is derived (SHA-256) from APP_SECRET_KEK, falling
// back to IMPORT_TOKEN_KEK (so existing deployments that already set that for
// the import pipeline get encryption for free), then to a dev-only constant.
// The KEK itself MUST live in the environment / platform secret store — you
// cannot bootstrap encryption from the database you are encrypting.

func secretKEK() []byte {
	v := os.Getenv("APP_SECRET_KEK")
	if v == "" {
		v = os.Getenv("IMPORT_TOKEN_KEK")
	}
	if v == "" {
		// Dev fallback so the service still boots locally. Production must
		// set APP_SECRET_KEK (or IMPORT_TOKEN_KEK) to a high-entropy value.
		v = "onecamp-dev-app-secret-kek-please-override-in-production"
		MessageLogs.ErrorLog.Printf("helpers/secretbox: APP_SECRET_KEK unset — using INSECURE dev key. Set APP_SECRET_KEK in production.")
	}
	sum := sha256.Sum256([]byte(v))
	return sum[:]
}

// EncryptSecret encrypts plaintext and returns a base64 string suitable for a
// text column. Returns "" for empty input (callers store NULL).
func EncryptSecret(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	block, err := aes.NewCipher(secretKEK())
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	out := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return base64.StdEncoding.EncodeToString(out), nil
}

// DecryptSecret reverses EncryptSecret. Returns "" for empty input.
func DecryptSecret(encoded string) (string, error) {
	if encoded == "" {
		return "", nil
	}
	blob, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	if len(blob) < 12+16 {
		return "", errors.New("ciphertext too short")
	}
	block, err := aes.NewCipher(secretKEK())
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
