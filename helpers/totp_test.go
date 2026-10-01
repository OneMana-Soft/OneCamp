package helpers

import (
	"strings"
	"testing"
	"time"
)

// rfcSecret is the shared secret both RFCs use for their published vectors: the ASCII string
// "12345678901234567890".
var rfcSecret = base32NoPad.EncodeToString([]byte("12345678901234567890"))

// RFC 4226 Appendix D — the HOTP vectors, counters 0 through 9, six digits.
//
// THIS IS WHY THE IMPLEMENTATION IS NOT A DEPENDENCY. The algorithm is fully specified and the
// specification ships answers, so correctness here is demonstrable rather than assumed. Every part that
// could plausibly be wrong — big-endian counter packing, the dynamic-truncation offset, masking the
// high bit, the modulus, zero-padding — produces different digits and is caught by these ten values.
func TestHOTPMatchesRFC4226Vectors(t *testing.T) {
	key := []byte("12345678901234567890")

	want := []string{
		"755224", "287082", "359152", "969429", "338314",
		"254676", "287922", "162583", "399871", "520489",
	}

	for counter, expected := range want {
		if got := hotp(key, uint64(counter), 6); got != expected {
			t.Errorf("counter %d: got %s, RFC 4226 says %s", counter, got, expected)
		}
	}
}

// RFC 6238 Appendix B — the SHA-1 TOTP vectors, eight digits.
//
// Eight rather than six because that is what the RFC tabulates. Exercising hotp at a non-default digit
// count also proves the modulus and padding are derived from the parameter rather than hardcoded to the
// six-digit case, which is the kind of shortcut that works until someone changes a constant.
//
// The final vector, 20000000000, is past 2038 and is included as published: it would catch a 32-bit
// truncation of the step, which is a real portability trap in this arithmetic.
func TestTOTPMatchesRFC6238Vectors(t *testing.T) {
	key := []byte("12345678901234567890")

	cases := []struct {
		unix int64
		want string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	}

	for _, c := range cases {
		step := TOTPStep(time.Unix(c.unix, 0))
		if got := hotp(key, uint64(step), 8); got != c.want {
			t.Errorf("t=%d (step %d): got %s, RFC 6238 says %s", c.unix, step, got, c.want)
		}
	}
}

// A code from the current step validates, and reports that step.
func TestValidateTOTPAcceptsCurrentStep(t *testing.T) {
	now := time.Unix(1111111111, 0)
	step := TOTPStep(now)
	code := hotp([]byte("12345678901234567890"), uint64(step), TOTPDigits)

	got, ok := ValidateTOTP(rfcSecret, code, now, 0)
	if !ok {
		t.Fatalf("a code for the current step must validate (code %s, step %d)", code, step)
	}
	if got != step {
		t.Errorf("reported step %d, want %d — the caller persists this to block replay", got, step)
	}
}

// REPLAY IS REFUSED. The property the whole design exists for.
//
// A code is valid for its window plus the skew either side, so without this a code read over a shoulder
// or out of a screen share can be used again seconds later. Passing the last accepted step as minStep is
// what spends it.
func TestValidateTOTPRefusesAReplayedCode(t *testing.T) {
	now := time.Unix(1111111111, 0)
	step := TOTPStep(now)
	code := hotp([]byte("12345678901234567890"), uint64(step), TOTPDigits)

	accepted, ok := ValidateTOTP(rfcSecret, code, now, 0)
	if !ok {
		t.Fatal("first use must succeed")
	}

	// Same code, same instant, but the step is now spent.
	if _, ok := ValidateTOTP(rfcSecret, code, now, accepted); ok {
		t.Error("the same code was accepted twice; minStep is not being enforced")
	}

	// And a step older than the last login is refused even though its arithmetic is correct.
	if _, ok := ValidateTOTP(rfcSecret, code, now, accepted+5); ok {
		t.Error("a step below minStep must be refused")
	}
}

// Clock skew of one step either way is tolerated; two steps is not.
//
// The tolerance exists because a phone twenty seconds fast would otherwise fail to log in with no way to
// discover why. The ceiling exists because every extra step widens the window an observed code stays
// usable in.
func TestValidateTOTPToleratesOneStepOfSkew(t *testing.T) {
	key := []byte("12345678901234567890")
	now := time.Unix(1111111111, 0)
	current := TOTPStep(now)

	for _, offset := range []int64{-1, 0, 1} {
		code := hotp(key, uint64(current+offset), TOTPDigits)
		if _, ok := ValidateTOTP(rfcSecret, code, now, 0); !ok {
			t.Errorf("offset %+d should be within tolerance", offset)
		}
	}

	for _, offset := range []int64{-2, 2, 10} {
		code := hotp(key, uint64(current+offset), TOTPDigits)
		if _, ok := ValidateTOTP(rfcSecret, code, now, 0); ok {
			t.Errorf("offset %+d is outside tolerance and must be refused", offset)
		}
	}
}

// Malformed input is refused without panicking.
//
// These all arrive from a login form, so every one of them is reachable by anyone on the internet. A
// panic in this function is an unauthenticated denial of service.
func TestValidateTOTPRefusesMalformedInput(t *testing.T) {
	now := time.Unix(1111111111, 0)

	cases := []struct{ name, secret, code string }{
		{"empty code", rfcSecret, ""},
		{"short code", rfcSecret, "12345"},
		{"long code", rfcSecret, "1234567"},
		{"non-numeric", rfcSecret, "abcdef"},
		{"empty secret", "", "123456"},
		{"secret not base32", "not!base32!!", "123456"},
		{"both empty", "", ""},
	}

	for _, c := range cases {
		if _, ok := ValidateTOTP(c.secret, c.code, now, 0); ok {
			t.Errorf("%s: must not validate", c.name)
		}
	}
}

// A secret is accepted in the shapes a person can actually produce.
//
// Authenticator apps display secrets in spaced groups, some lowercase, some padded. Rejecting any of
// those presents as "wrong secret" during enrolment, which the user cannot diagnose.
func TestValidateTOTPNormalisesPastedSecrets(t *testing.T) {
	now := time.Unix(1111111111, 0)
	code := hotp([]byte("12345678901234567890"), uint64(TOTPStep(now)), TOTPDigits)

	variants := []string{
		rfcSecret,
		strings.ToLower(rfcSecret),
		rfcSecret[:8] + " " + rfcSecret[8:16] + " " + rfcSecret[16:],
		rfcSecret[:8] + "-" + rfcSecret[8:],
		rfcSecret + "======",
		"  " + rfcSecret + "  ",
	}

	for _, secret := range variants {
		if _, ok := ValidateTOTP(secret, code, now, 0); !ok {
			t.Errorf("secret variant %q should be accepted", secret)
		}
	}
}

// A generated secret is the right size, decodes, and differs each time.
func TestNewTOTPSecret(t *testing.T) {
	first, err := NewTOTPSecret()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	raw, err := base32NoPad.DecodeString(first)
	if err != nil {
		t.Fatalf("the generated secret must decode as unpadded base32: %v", err)
	}
	if len(raw) != totpSecretBytes {
		t.Errorf("got %d bytes, RFC 4226 section 4 requires at least %d", len(raw), totpSecretBytes)
	}
	if strings.ContainsAny(first, "=") {
		t.Error("the secret must be unpadded; authenticator apps reject the padding")
	}

	second, err := NewTOTPSecret()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if first == second {
		t.Error("two generated secrets are identical, which means the source is not random")
	}
}

// A generated secret actually works end to end, which the size check alone does not prove.
func TestNewTOTPSecretRoundTrips(t *testing.T) {
	secret, err := NewTOTPSecret()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	now := time.Now()
	key, err := base32NoPad.DecodeString(secret)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	code := hotp(key, uint64(TOTPStep(now)), TOTPDigits)

	if _, ok := ValidateTOTP(secret, code, now, 0); !ok {
		t.Error("a freshly generated secret must validate its own current code")
	}
}

// The provisioning URI carries everything an app needs, escaped.
func TestTOTPProvisioningURI(t *testing.T) {
	uri := TOTPProvisioningURI("OneMana Solutions", "akash@onemana.dev", "ABCDEFGH")

	for _, want := range []string{
		"otpauth://totp/",
		"secret=ABCDEFGH",
		"algorithm=SHA1",
		"digits=6",
		"period=30",
		"issuer=OneMana+Solutions",
	} {
		if !strings.Contains(uri, want) {
			t.Errorf("URI is missing %q: %s", want, uri)
		}
	}

	// The label must be escaped: an unescaped space or @ breaks parsing in some apps, and the failure
	// is a QR code that scans into nothing.
	if strings.Contains(uri, " ") {
		t.Errorf("the URI contains a raw space: %s", uri)
	}
}
