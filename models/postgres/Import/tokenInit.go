// Package models is the provider-agnostic postgres layer for the import
package models

// Startup probe for IMPORT_TOKEN_KEK.
//
// Called from main.go after env loading. Encrypts a known plaintext
// with the configured KEK, decrypts it back, and confirms round-trip.
// In production we also fail loudly when the dev-fallback KEK is in
// use — the constant string in tokenModel.go.kek() has zero entropy
// and any deployment using it has a security bug.

import (
	"errors"
	"log"
	"os"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
)

// devFallbackKEK is the literal string in kek() when IMPORT_TOKEN_KEK
// is unset. We mirror it here (rather than exporting the inner kek()
// helper) so the startup probe can refuse to boot when production has
// fallen through to it.
const devFallbackKEK = "onecamp-dev-import-kek-please-override-in-production"

// ValidateTokenEncryptionAtStartup runs an encrypt/decrypt round trip
// using the configured KEK. Call from main.go right after env loading.
//
// Returns an error iff:
//  1. The encryption itself round-trips wrong (extremely unlikely;
//     indicates a runtime crypto issue).
//  2. APP_ENV is "production" / "prod" and the KEK is unset OR is the
//     dev-fallback string. Production deployments must override the
//     KEK from the platform secret store.
//
// Non-production environments tolerate the dev fallback so `make dev`
// stays one command. They get a single-line warning at startup.
//
// Uses the standard log package (not the project's slog wrapper)
// because this runs before loggerInit.InitLogger() in main.go.
func ValidateTokenEncryptionAtStartup() error {
	const probe = "kek-startup-probe-token"

	enc, err := encryptToken(probe)
	if err != nil {
		return err
	}
	dec, err := decryptToken(enc)
	if err != nil {
		return err
	}
	if dec != probe {
		return errors.New("token KEK round-trip mismatch (corrupted ciphertext)")
	}

	// A PLACEHOLDER IS FATAL EVERYWHERE, unlike the dev fallback. The round trip above passes for
	// any KEK, so an unsubstituted __SET_FROM_BETA_SECRET_STORE__ from vars/.env.beta was accepted
	// here as a real key — it is neither empty nor the fallback. Beta shipped that way, encrypting
	// OAuth import tokens with a string committed to this repository. See helpers.InspectKEK.
	status := helpers.InspectKEK("IMPORT_TOKEN_KEK", os.Getenv("IMPORT_TOKEN_KEK"), devFallbackKEK)
	if status.Unusable() {
		return errors.New(status.Reason + " (refusing to start)")
	}

	prodEnv := isProdEnv(os.Getenv("APP_ENV"))

	switch {
	case status.Fallback && prodEnv:
		return errors.New(status.Reason + "; refusing to start. " +
			"Set it from your platform secret store")
	case status.Fallback:
		log.Printf("[WARN] %s This is OK for local dev only; production MUST override.", status.Reason)
	case status.Weak:
		log.Printf("[WARN] %s", status.Reason)
	}
	return nil
}

func isProdEnv(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "production", "prod":
		return true
	}
	return false
}
