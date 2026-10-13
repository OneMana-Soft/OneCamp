package helpers

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A push carries at most PushBodyMax bytes of a message, cut where a rune
// starts, with an ellipsis: FCM refuses a data payload over 4096 bytes, and a
// long paste went whole into every member's push (TN-BUG-14).
func TestCutPushBody(t *testing.T) {
	if got := CutPushBody("Ship it Friday", PushBodyMax); got != "Ship it Friday" {
		t.Fatalf("a short body changed: %q", got)
	}
	exact := strings.Repeat("a", PushBodyMax)
	if got := CutPushBody(exact, PushBodyMax); got != exact {
		t.Fatalf("a body of exactly the limit was cut to %d bytes", len(got))
	}

	paste := strings.Repeat("log line 42: connection reset by peer\n", 120) // ~4.5 KB
	got := CutPushBody(paste, PushBodyMax)
	if len(got) > PushBodyMax || !strings.HasSuffix(got, "…") || !strings.HasPrefix(paste, strings.TrimSuffix(got, "…")) {
		t.Fatalf("a 4.5 KB paste cut to %d bytes: %q…", len(got), got[:40])
	}
	if strings.HasSuffix(strings.TrimSuffix(got, "…"), "\n") {
		t.Fatalf("the cut keeps trailing whitespace before the ellipsis")
	}

	// Every limit, over text whose runes are 1, 2, 3 and 4 bytes long: never
	// more than the limit, never half a rune.
	text := strings.Repeat("aé€🙂", 40)
	for max := 0; max <= len(text)+1; max++ {
		got := CutPushBody(text, max)
		if len(got) > max || !utf8.ValidString(got) {
			t.Fatalf("limit %d: %d bytes, valid UTF-8 %v", max, len(got), utf8.ValidString(got))
		}
		if max >= len(text) && got != text {
			t.Fatalf("limit %d: a body that fits was cut", max)
		}
	}
}
