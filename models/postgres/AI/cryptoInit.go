package models

// Startup probe for AI_CONFIG_KEK, mirroring import token KEK validation
// (models/postgres/Import/tokenInit.go).
//
// In production we refuse to start if the dev fallback KEK is still in
// place, so provider API keys are never encrypted with a publicly-known
// key. In dev we log a single warning and continue.

import (
	"errors"
	"log"
	"os"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
)

// ValidateAIKeyEncryptionAtStartup runs an encrypt/decrypt round trip
// using the configured AI_CONFIG_KEK. Returns an error iff:
//  1. The round-trip is wrong (indicates a runtime crypto issue), or
//  2. APP_ENV is production/prod and the dev fallback KEK is in use.
//
// Uses the standard log package because this may run before the
// project's logger is initialized.
func ValidateAIKeyEncryptionAtStartup() error {
	const probe = "ai-kek-startup-probe"

	enc, err := EncryptAPIKey(probe)
	if err != nil {
		return err
	}
	dec, err := DecryptAPIKey(enc)
	if err != nil {
		return err
	}
	if dec != probe {
		return errors.New("AI_CONFIG_KEK round-trip mismatch (corrupted ciphertext)")
	}

	// A PLACEHOLDER IS FATAL EVERYWHERE. The round trip above passes for any KEK, including
	// __SET_FROM_BETA_SECRET_STORE__ copied out of vars/.env.beta and never substituted — which is
	// what beta actually ran on. That is neither unset nor the dev fallback, so the checks below
	// accepted it, and every provider key and MCP secret was encrypted with a string committed to
	// this repository. See helpers.InspectKEK.
	status := helpers.InspectKEK("AI_CONFIG_KEK", os.Getenv("AI_CONFIG_KEK"), devFallbackKEK)
	if status.Unusable() {
		return errors.New(status.Reason + " (refusing to start)")
	}

	if status.Fallback {
		env := strings.ToLower(os.Getenv("APP_ENV"))
		if env == "production" || env == "prod" {
			return errors.New(status.Reason + "; refusing to start (provider API keys would be encrypted with a public dev key)")
		}
		log.Println("WARNING: " + status.Reason)
	} else if status.Weak {
		log.Println("WARNING: " + status.Reason)
	}

	return nil
}
