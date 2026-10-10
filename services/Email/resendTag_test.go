package email

import (
	"strings"
	"testing"
)

// Resend refuses a tag outside [A-Za-z0-9_-] with a terminal 422, and the
// notification worker tags every email with a dotted event type. These pin
// that no tag can cost an email again.
func TestResendTagText(t *testing.T) {
	cases := map[string]string{
		"channel.mention": "channel_mention",
		"event_type":      "event_type",
		"  a-b_C9  ":      "a-b_C9",
		"héllo wörld":     "h_llo_w_rld",
		"":                "",
	}
	for in, want := range cases {
		if got := resendTagText(in); got != want {
			t.Errorf("resendTagText(%q) = %q, want %q", in, got, want)
		}
	}
	if got := resendTagText(strings.Repeat("x", 400)); len(got) != 256 {
		t.Errorf("long tag kept %d characters, want 256", len(got))
	}
}
