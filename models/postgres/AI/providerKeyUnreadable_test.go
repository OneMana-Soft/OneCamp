package models

import (
	"errors"
	"strings"
	"testing"
)

// The unreadable-key error must stay recognisable AND stay readable.
//
// Two properties, and they pull in opposite directions, which is why they are pinned together.
//
// RECOGNISABLE: callers decide an HTTP status from this (409, not 502) and a log level (INFO, not
// ERROR) by asking errors.Is. Wrapping with %w is what keeps that working; wrapping with %v or
// rebuilding the sentence by hand silently turns the check off, and the symptom is not a failing
// test but a 502 in production for a condition the server could name exactly.
//
// READABLE: the message is shown to an admin verbatim. The frontend renders msg, and this text is
// deliberately put there, so its wording is a user interface. The previous composition was
// fmt.Errorf("%w: %s", ErrProviderKeyUnreadable, p.Label), which produced
//
//	...re-enter the key in admin AI settings: groq
//
// — a sentence with a provider name stapled to the end of the instruction. The suffix assertion
// below is specifically that regression, because it is the kind of thing that reads fine in a diff
// and badly in a toast.
func TestProviderKeyUnreadableError(t *testing.T) {
	const wantWhole = "provider groq: the stored API key can no longer be decrypted " +
		"(usually because AI_CONFIG_KEK changed); re-enter the key in admin AI settings"

	err := ProviderKeyUnreadableError("groq")

	if !errors.Is(err, ErrProviderKeyUnreadable) {
		t.Fatalf("errors.Is failed: callers switch on this to answer 409 instead of 502.\n got: %v", err)
	}
	if got := err.Error(); got != wantWhole {
		t.Errorf("message changed — this text is shown to an admin verbatim\n got: %q\nwant: %q", got, wantWhole)
	}
	if strings.HasSuffix(err.Error(), ": groq") {
		t.Errorf("the label is stapled to the end of the instruction again: %q", err.Error())
	}
	if !strings.HasPrefix(err.Error(), "provider groq: ") {
		t.Errorf("the label should be the subject, in front: %q", err.Error())
	}
}

// An empty label must not produce "provider : ...".
//
// Labels are NOT NULL in ai_providers, so this should not arise from real data. It is pinned anyway
// because the alternative to handling it is asserting a database invariant inside a sentence shown
// to a user, and the failure mode is silent: a stray colon, no error anywhere.
func TestProviderKeyUnreadableErrorWithoutLabel(t *testing.T) {
	err := ProviderKeyUnreadableError("")

	if !errors.Is(err, ErrProviderKeyUnreadable) {
		t.Fatalf("errors.Is failed for the unlabelled form: %v", err)
	}
	if err != ErrProviderKeyUnreadable {
		t.Errorf("want the bare sentinel itself, got a wrap: %q", err.Error())
	}
	if strings.Contains(err.Error(), "provider :") {
		t.Errorf("empty label leaked into the message: %q", err.Error())
	}
}

// The sentinel must remain composable, i.e. have no leading subject of its own.
//
// ProviderKeyUnreadableError puts "provider <label>: " in front of it, so a message beginning "this
// provider's stored API key..." (which is how this started) yields "provider groq: this provider's
// stored API key...". The sentinel's phrasing and the constructor's prefix are one design, and this
// is the assertion that keeps someone from editing one without the other.
func TestProviderKeyUnreadableSentinelIsComposable(t *testing.T) {
	msg := ErrProviderKeyUnreadable.Error()

	if !strings.HasPrefix(msg, "the stored API key") {
		t.Errorf("sentinel gained a subject of its own; it is prefixed with %q by "+
			"ProviderKeyUnreadableError, so it must continue that sentence: %q", "provider <label>: ", msg)
	}
	// The cause and the remedy are the reason this is worth showing to an admin at all. Losing
	// either turns an actionable message back into "something went wrong".
	if !strings.Contains(msg, "AI_CONFIG_KEK") {
		t.Errorf("sentinel no longer names the cause, which is the one thing an operator can check: %q", msg)
	}
	if !strings.Contains(msg, "re-enter") {
		t.Errorf("sentinel no longer states the remedy: %q", msg)
	}
}
