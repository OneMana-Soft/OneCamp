package models

// Startup probe for TOTP_KEK, alongside the ones for AI_CONFIG_KEK
// (models/postgres/AI/cryptoInit.go) and IMPORT_TOKEN_KEK (models/postgres/Import/tokenInit.go).
//
// THE POLICY HERE IS DELIBERATELY NOT THE SAME AS THOSE TWO, and the difference is the point.
//
// Those KEKs have a dev fallback, so an UNSET value still encrypts — with a key printed in the source.
// The variable being empty is therefore itself the danger, which is why they refuse to start in
// production over it.
//
// TOTP_KEK has no fallback, on purpose: totpKEK() returns ErrTOTPKEKMissing when it is empty, enrolment
// answers 409 with an actionable sentence, and nothing is sealed. Unset means "this deployment does not
// offer two-factor authentication", which is a legitimate configuration. Refusing to boot over it would
// be an outage caused by an optional feature being switched off.
//
// A PLACEHOLDER IS A DIFFERENT MATTER AND IS FATAL EVERYWHERE. __SET_FROM_BETA_SECRET_STORE__ is not
// empty, so totpKEK() accepts it and hashes it into a perfectly functional key — one committed to this
// repository in vars/. Enrolment then succeeds, the user sees a QR code, the admin sees a protected
// account, an auditor sees MFA, and the second factor is sealed with a public string. That is strictly
// worse than not offering 2FA at all, because the first is visibly absent and the second is invisibly
// hollow.
//
// It is also unrecoverable in a way the other two are not. Replacing the placeholder with a real key
// makes every enrolled secret undecryptable, and the affected user cannot fix it themselves — they need
// an administrator to reset their factor. So the placeholder has to be caught BEFORE anyone enrols,
// which means at boot.

import (
	"errors"
	"log"
	"os"

	"github.com/akashc777/OneCamp/helpers"
)

// ValidateTOTPKeyEncryptionAtStartup checks TOTP_KEK and round-trips a probe value when one is set.
//
// Returns an error only when the deployment would otherwise seal second factors under a value nobody
// meant to use, or when the configured key cannot complete a round trip.
func ValidateTOTPKeyEncryptionAtStartup() error {
	configured := os.Getenv("TOTP_KEK")

	// No dev fallback to compare against, so "" for that argument.
	status := helpers.InspectKEK("TOTP_KEK", configured, "")
	if status.Unusable() {
		return errors.New(status.Reason + " (refusing to start)")
	}

	// Unset. Reported once, at INFO volume, because it is a supported configuration rather than a
	// misconfiguration — but it is worth saying, since the alternative is an admin clicking "Turn on"
	// in settings and receiving a 409 with no idea that a server-side variable is the cause.
	if status.Fallback {
		log.Println("[INFO] TOTP_KEK is not set, so two-factor authentication is unavailable. " +
			"Generate one with `openssl rand -base64 48` to enable it. " +
			"Set it BEFORE anyone enrols: changing it later makes every enrolled secret " +
			"undecryptable and each affected user needs an administrator reset.")
		return nil
	}

	if status.Weak {
		log.Println("WARNING: " + status.Reason)
	}

	// Round trip, matching the other two probes. Cheap, and it turns a key that cannot drive AES-GCM
	// into a startup failure rather than into a user watching enrolment fail with a server error.
	const probe = "totp-kek-startup-probe"
	sealed, err := sealTOTPSecret(probe)
	if err != nil {
		return err
	}
	opened, err := openTOTPSecret(sealed)
	if err != nil {
		return err
	}
	if opened != probe {
		return errors.New("TOTP_KEK round-trip mismatch (corrupted ciphertext)")
	}

	return nil
}
