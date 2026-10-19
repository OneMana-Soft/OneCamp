package helpers

import (
	"regexp"
	"strings"
)

// An emoji is kept by its picker's id ("+1", "sandwich", "flag-in"): the id
// the web's pickers send, and draw the emoji by (emoji-mart's ids are lower
// case letters, digits, _, + and -, the longest 54 characters). One rule for
// every place that keeps one, a reaction or a person's status, so nothing else
// (a space, markup, a sentence) is ever stored and served to readers as an
// emoji: the web, email digests and MCP alike.
var emojiPickerID = regexp.MustCompile(`^[a-z0-9_+-]{1,64}$`)

// EmojiID is s as an emoji's id, and whether it is one. Space around it and a
// :shortcode:'s colons are dropped, so ":+1:" is "+1".
func EmojiID(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) > 2 && strings.HasPrefix(s, ":") && strings.HasSuffix(s, ":") {
		s = s[1 : len(s)-1]
	}
	if !emojiPickerID.MatchString(s) {
		return "", false
	}
	return s, true
}
