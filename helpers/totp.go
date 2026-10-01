package helpers

// Time-based one-time passwords, RFC 6238 over RFC 4226.
//
// WHY THIS IS NOT A DEPENDENCY. github.com/pquerna/otp is the conventional choice and would have been
// fine for generating and checking a code. It was not chosen for one concrete reason: its Validate
// returns a boolean, and this package must report WHICH TIME-STEP matched.
//
// That is not a nicety. A TOTP code stays valid for its whole window plus the skew either side, so
// without recording the step that was accepted, the same code can be presented twice — which is the
// realistic attack here, someone reading a code over a shoulder or out of a screen share and using it
// seconds later. Rejecting a step already spent is what closes it, and that needs the number.
//
// Reaching for the library's ValidateCustom and looping over candidate times myself would have meant
// writing this loop regardless, leaving the dependency supplying only HMAC truncation — about fifteen
// lines of RFC 4226 — while adding a transitive barcode package pinned to a 2019 pseudo-version into
// the build graph of an authentication path. The trade did not favour it.
//
// The algorithm is fully specified and both RFCs publish test vectors, so this implementation is
// PROVEN rather than trusted: totp_test.go checks every RFC 4226 Appendix D vector and every RFC 6238
// Appendix B SHA-1 vector. A wrong implementation cannot pass those.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	// TOTPDigits is the code length. Six is what every authenticator app shows by default, and the
	// brute-force resistance comes from rate limiting, not from length.
	TOTPDigits = 6

	// TOTPPeriodSeconds is the step width. 30 is the RFC 6238 default and what apps assume; changing
	// it silently breaks every already-enrolled device.
	TOTPPeriodSeconds = 30

	// totpSkewSteps accepts one step either side, so a code is good for at most 90 seconds.
	//
	// Clock skew between a phone and a server is the reason this is not zero: a user whose phone is
	// twenty seconds fast would otherwise be unable to log in, and would have no way to discover why.
	// One step is the usual compromise; more widens the window an observed code stays usable in, which
	// is precisely what the replay guard is trying to shrink.
	totpSkewSteps = 1

	// totpSecretBytes is 160 bits, the minimum RFC 4226 section 4 requires. Authenticator apps handle
	// it as 32 base32 characters.
	totpSecretBytes = 20
)

// base32NoPad is how authenticator apps expect a secret: RFC 4648 base32, uppercase, unpadded.
var base32NoPad = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a fresh base32-encoded shared secret.
func NewTOTPSecret() (string, error) {
	raw := make([]byte, totpSecretBytes)
	if _, err := rand.Read(raw); err != nil {
		// crypto/rand failing is not a condition to paper over with a weaker source.
		return "", fmt.Errorf("totp: cannot read random bytes: %w", err)
	}
	return base32NoPad.EncodeToString(raw), nil
}

// TOTPProvisioningURI builds the otpauth:// URI an authenticator app scans.
//
// The issuer appears TWICE, as the label prefix and as a query parameter, which looks redundant and is
// not: the parameter is authoritative for modern apps, the prefix is what older ones display, and apps
// that read both will show a stray colon if they disagree. Both are escaped because an issuer is a
// display name — "OneMana Solutions" and an account that is an email address both need it.
func TOTPProvisioningURI(issuer, account, secret string) string {
	label := url.PathEscape(issuer + ":" + account)

	params := url.Values{}
	params.Set("secret", secret)
	params.Set("issuer", issuer)
	// Stated explicitly rather than relying on defaults. They ARE the defaults, but an app that
	// assumes differently produces codes that never match, and the failure looks like a wrong secret.
	params.Set("algorithm", "SHA1")
	params.Set("digits", fmt.Sprintf("%d", TOTPDigits))
	params.Set("period", fmt.Sprintf("%d", TOTPPeriodSeconds))

	return "otpauth://totp/" + label + "?" + params.Encode()
}

// TOTPStep is the RFC 6238 time-step for an instant: seconds since the epoch divided by the period.
func TOTPStep(at time.Time) int64 {
	return at.Unix() / TOTPPeriodSeconds
}

// ValidateTOTP checks a user-supplied code and reports the time-step it matched.
//
// minStep REJECTS REPLAY. Pass the last step this user already authenticated with; any candidate at or
// below it is refused even when the arithmetic is correct, so a code cannot be spent twice. Pass 0 on
// first enrolment.
//
// Returns (step, true) on success — the caller MUST persist that step. Returns (0, false) otherwise,
// and deliberately says nothing about why: "wrong code", "expired step" and "already used" are all the
// same answer to whoever is typing, and distinguishing them would let an attacker learn that a code
// was once right.
func ValidateTOTP(secret, code string, at time.Time, minStep int64) (int64, bool) {
	secret = normaliseTOTPSecret(secret)
	code = strings.TrimSpace(code)
	if secret == "" || len(code) != TOTPDigits {
		return 0, false
	}

	key, err := base32NoPad.DecodeString(secret)
	if err != nil || len(key) == 0 {
		return 0, false
	}

	current := TOTPStep(at)
	// Ordered from oldest to newest so that, in the vanishingly unlikely event two candidate steps
	// produce the same digits, the earliest is consumed and the later one stays available.
	for offset := int64(-totpSkewSteps); offset <= totpSkewSteps; offset++ {
		step := current + offset
		if step <= minStep {
			// Already spent, or older than the last successful login. Not an error to report.
			continue
		}
		if hotpEqual(key, step, code) {
			return step, true
		}
	}
	return 0, false
}

// hotpEqual computes the HOTP value for a counter and compares it to the supplied code.
//
// The comparison is constant-time. Codes are short and the window is small, so a timing oracle here is
// not a practical break — but it costs one function call to remove the question entirely, and this is
// an authentication path where "probably not exploitable" is the wrong standard.
func hotpEqual(key []byte, counter int64, code string) bool {
	want := hotp(key, uint64(counter), TOTPDigits)
	return subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1
}

// hotp implements RFC 4226: HMAC-SHA1 over the big-endian counter, dynamically truncated.
//
// SHA-1 is correct here and is not a weakness. RFC 4226 specifies HMAC-SHA-1, every authenticator app
// implements it, and the collision attacks that retired SHA-1 for signatures do not apply to HMAC.
// Choosing SHA-256 would be strictly more modern and would silently fail against the apps users
// actually have.
func hotp(key []byte, counter uint64, digits int) string {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)

	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)

	// Dynamic truncation, RFC 4226 section 5.3: the low nibble of the last byte selects the offset,
	// and the high bit of the chosen word is masked off so the result is positive on every platform.
	offset := sum[len(sum)-1] & 0x0f
	value := (uint32(sum[offset]&0x7f) << 24) |
		(uint32(sum[offset+1]) << 16) |
		(uint32(sum[offset+2]) << 8) |
		uint32(sum[offset+3])

	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}
	// Zero-padded: 001234 is a valid code, and trimming the leading zero is a classic way to lock a
	// user out roughly one time in ten.
	return fmt.Sprintf("%0*d", digits, value%mod)
}

// normaliseTOTPSecret accepts what a human might paste.
//
// Apps display secrets in space-separated groups and some in lowercase. Rejecting either would present
// as "wrong secret" during enrolment, which is unfalsifiable from the user's side.
func normaliseTOTPSecret(secret string) string {
	secret = strings.TrimSpace(secret)
	secret = strings.ReplaceAll(secret, " ", "")
	secret = strings.ReplaceAll(secret, "-", "")
	// Padding is stripped rather than rejected so a secret produced by a padding encoder still works.
	secret = strings.TrimRight(secret, "=")
	return strings.ToUpper(secret)
}
