package helpers

import "testing"

// A reaction or a status is an emoji's picker id and nothing else: what is
// stored is served to every reader, so text, markup and spaces are refused.
func TestEmojiIDIsAPickersIDAndNothingElse(t *testing.T) {
	for in, want := range map[string]string{
		"+1": "+1", "thumbs_up": "thumbs_up", "flag-in": "flag-in", " heart ": "heart",
		":+1:": "+1", ":smiling_face_with_3_hearts:": "smiling_face_with_3_hearts",
		"100": "100",
	} {
		if got, ok := EmojiID(in); !ok || got != want {
			t.Errorf("%q: %q, %v; want %q", in, got, ok, want)
		}
	}
	long := ""
	for len(long) < 65 {
		long += "a"
	}
	for _, in := range []string{
		"", " ", "::", ":", "<b>hi</b>", "thumbs up", "Thumbs_Up", "a;b", "a\nb", "a:b",
		`"><img src=x onerror=alert(1)>`, "👍", long, ":" + long + ":",
	} {
		if got, ok := EmojiID(in); ok {
			t.Errorf("%q was taken as %q", in, got)
		}
	}
}
