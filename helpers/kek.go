package helpers

// Validation shared by the key-encryption-key startup probes.
//
// WHY THIS EXISTS. Two KEKs protect stored secrets: AI_CONFIG_KEK (AI provider API keys, MCP server
// auth secrets) and IMPORT_TOKEN_KEK (OAuth import tokens). Each had a startup probe that refused to
// boot in production when its KEK was UNSET or still the package's dev fallback.
//
// Neither caught the case that actually happened. Beta ran for months with
//
//	AI_CONFIG_KEK=__SET_FROM_BETA_SECRET_STORE__
//
// copied verbatim out of vars/.env.beta and never substituted. That is not unset and not the dev
// fallback, so both probes accepted it — and every provider key and MCP secret in that database was
// encrypted with SHA-256 of a string committed to the repository. Anyone holding a database dump and
// the repo could decrypt all of them.
//
// It surfaced only by accident, as "this provider's key cannot be decrypted" after the value changed
// at some point. A weak key does not announce itself; it works perfectly.
//
// A PLACEHOLDER IS FATAL IN EVERY ENVIRONMENT, which is a deliberate difference from the dev
// fallback. The fallback is a documented convenience so a local checkout starts without ceremony, so
// tolerating it outside production is a real trade. Nobody has ever meant to type
// __SET_FROM_BETA_SECRET_STORE__, in dev or anywhere else — and leaving a placeholder set is strictly
// worse than leaving the variable unset, because the unset path is the one the probes check.
//
// Refusing to start is the point. The alternative is what already happened: the process comes up,
// reports itself healthy, and quietly encrypts secrets with a public string.

import (
	"fmt"
	"strings"
)

// kekMinLength is the length below which a KEK is reported as weak (never fatal).
//
// 32 because that is what the existing error text already asked operators for, and because
// `make secrets` in the shipped installer mints `openssl rand -base64 48`, which is 64
// characters. Advisory
// rather than fatal on purpose: an operator may have a shorter but genuinely random value in a real
// deployment, and refusing to boot over length would be a self-inflicted outage over a judgement
// call. A placeholder is not a judgement call.
const kekMinLength = 32

// kekPlaceholderMarkers are substrings that only ever appear in an unsubstituted template value.
//
// CHOSEN TO BE UNAMBIGUOUS, because a false positive here is a refusal to boot. Each is long enough
// that a random base64 or hex secret will not contain it by chance — the odds of a specific
// seven-character run inside a 64-character value are around one in ten billion. Short, tempting
// tokens like "todo" and "xxxx" are deliberately absent: four characters is frequent enough in random
// output to eventually take a deployment down for no reason.
//
// Matched case-insensitively, since templates are written in both cases.
var kekPlaceholderMarkers = []string{
	"set_from",     // __SET_FROM_BETA_SECRET_STORE__ — the one that actually shipped
	"set-from",     //
	"secret_store", // ...SECRET_STORE__
	"secret-store", //
	"changeme",     //
	"change_me",    //
	"change-me",    //
	"replaceme",    //
	"replace_me",   //
	"replace-me",   //
	"placeholder",  //
	"fill_me",      //
	"fill-me",      //
	"yourkey",      //
	"your_key",     //
	"your-key",     //
	"example_kek",  //
	"example-kek",  //
}

// KEKStatus is the verdict on one key-encryption key.
type KEKStatus struct {
	// Reason is a sentence naming what is wrong and what to do, empty when the value looks like a
	// real secret. It names the variable, because a message that does not is useless in a log line
	// beside three other secrets.
	Reason string

	// Placeholder is true when the value is an unsubstituted template value. Never legitimate, in
	// any environment, so callers must refuse to start regardless of APP_ENV.
	Placeholder bool

	// Fallback is true when the value is absent or equals the caller's dev fallback constant.
	// Callers refuse in production and warn elsewhere, which is the pre-existing policy.
	Fallback bool

	// Weak is true when the value looks real but is shorter than kekMinLength. Advisory only.
	Weak bool
}

// Unusable reports whether the KEK must prevent startup in ANY environment.
func (s KEKStatus) Unusable() bool { return s.Placeholder }

// InspectKEK classifies a configured key-encryption key.
//
// name is the environment variable ("AI_CONFIG_KEK"), value its configured content, and devFallback
// the constant the crypto helper substitutes when the variable is empty. Pass devFallback as "" if
// the caller has none.
//
// Order matters: placeholder is checked BEFORE emptiness so a value of "__SET_FROM...__" is reported
// as the placeholder it is rather than as a present-and-therefore-fine key.
func InspectKEK(name, value, devFallback string) KEKStatus {
	trimmed := strings.TrimSpace(value)

	if isKEKPlaceholder(trimmed) {
		return KEKStatus{
			Placeholder: true,
			Reason: fmt.Sprintf(
				"%s is still a template placeholder (%q). Secrets encrypted with it are readable by "+
					"anyone who has this repository, because the value is committed in vars/. Set it "+
					"to a high-entropy value from your secret store — "+
					"`openssl rand -base64 48` — and restart. Note that secrets already stored under "+
					"the placeholder cannot be decrypted afterwards and must be re-entered",
				name, trimmed),
		}
	}

	if trimmed == "" {
		return KEKStatus{
			Fallback: true,
			Reason: fmt.Sprintf("%s is unset, so an insecure built-in development key is in use. "+
				"Production MUST set it (32+ characters, high entropy)", name),
		}
	}

	if devFallback != "" && trimmed == devFallback {
		return KEKStatus{
			Fallback: true,
			Reason: fmt.Sprintf("%s still equals the insecure built-in development key. "+
				"Production MUST replace it (32+ characters, high entropy)", name),
		}
	}

	if len(trimmed) < kekMinLength {
		return KEKStatus{
			Weak: true,
			Reason: fmt.Sprintf("%s is only %d characters; %d or more is expected. This is not "+
				"blocking startup, but if it is a passphrase rather than generated output, replace it "+
				"with `openssl rand -base64 48`", name, len(trimmed), kekMinLength),
		}
	}

	return KEKStatus{}
}

// isKEKPlaceholder reports whether a value is obviously an unsubstituted template.
func isKEKPlaceholder(trimmed string) bool {
	if trimmed == "" {
		return false
	}

	// The __WRAPPED__ convention used throughout vars/. Length guard so a value that merely starts
	// and ends with underscores, like "__", is not treated as a placeholder.
	if len(trimmed) > 4 && strings.HasPrefix(trimmed, "__") && strings.HasSuffix(trimmed, "__") {
		return true
	}

	// Angle brackets appear in <your-value-here> templates and in no base64, hex or passphrase a
	// real deployment would use.
	if strings.ContainsAny(trimmed, "<>") {
		return true
	}

	lower := strings.ToLower(trimmed)
	for _, marker := range kekPlaceholderMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
