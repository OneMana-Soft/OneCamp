// PII helpers for the import pipeline.
//
// Imports touch external user lists with real emails. Logging an
// unmasked email is a privacy risk if logs are forwarded to any
// third-party log aggregator (Datadog / CloudWatch / etc.).
//
// We redact emails by default in any string we hand to the structured
// logger. Tokens are NEVER passed through the logger because they live
// only in import_oauth_tokens.access_token_enc and are decrypted only
// inside the provider HTTP path.
package business

import (
	"regexp"
)

// emailRedactRe matches any address-shaped substring. The mask form
// keeps the first character and domain so an operator can still
// reasonably correlate "the import that failed for j***@acme.com"
// without exposing the full address.
var emailRedactRe = regexp.MustCompile(`([A-Za-z0-9])[A-Za-z0-9._%+\-]*(@[A-Za-z0-9.\-]+\.[A-Za-z]{2,})`)

// redactEmail masks every email in the given string. Returns the input
// unchanged when no email-shaped substring is present, so the function
// is safe to apply to every log message without measurable overhead.
func redactEmail(s string) string {
	if s == "" {
		return s
	}
	return emailRedactRe.ReplaceAllString(s, `$1***$2`)
}
