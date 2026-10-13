package helpers

import (
	"strings"
	"unicode/utf8"
)

// PushBodyMax is how many bytes of a message a push carries. FCM refuses a
// data payload over 4096 bytes in all, and message bodies went in whole: one
// long paste (a log, a spec) was refused for every device it was sent to.
const PushBodyMax = 1024

// CutPushBody is text cut to at most max bytes of UTF-8, at a rune boundary,
// with an ellipsis when anything was cut; text that fits comes back as it is.
// For a notification's body: a push's (firebaseInit.MultiCastPush) or any
// other notice that has a size limit. Pure.
func CutPushBody(text string, max int) string {
	if len(text) <= max {
		return text
	}
	if max <= 0 {
		return ""
	}
	ellipsis := "…"
	room := max - len(ellipsis)
	if room < 0 {
		room, ellipsis = max, ""
	}
	// Back to the start of a rune, so the cut never splits one.
	for room > 0 && !utf8.RuneStart(text[room]) {
		room--
	}
	if ellipsis == "" {
		return text[:room]
	}
	return strings.TrimRight(text[:room], " \t\r\n") + ellipsis
}
