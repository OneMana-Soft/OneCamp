package contentcodec

import (
	"regexp"
	"strings"
)

// The doc colours by name (N15, docs programme B2): text "sky", a block's
// background "sky_background". The one list on the server, and the rules for
// reading a stored colour; the web's is lib/docColours.ts, the collaboration
// service's schema/colours.js, both held to the same names
// (doc_format_test.go). Other packages (B3's cell colours, B4's paste) read
// these.

// DocColours are the doc colours, in the colour grid's order.
var DocColours = []string{"gray", "sun", "moss", "lake", "sky", "dusk", "berry"}

// IsDocColour reports whether s names one of the doc colours.
func IsDocColour(s string) bool {
	for _, c := range DocColours {
		if c == s {
			return true
		}
	}
	return false
}

var storedColour = regexp.MustCompile(`^(?:(gray|sun|moss|lake|sky|dusk|berry)|var\(--camp-(gray|sun|moss|lake|sky|dusk|berry)-ink\))$`)

// colourName is a text colour as stored: its name, or "" when it isn't one of
// ours. Case is ASCII's alone, as the TypeScript codec's.
func colourName(v any) string {
	s, _ := v.(string)
	m := storedColour.FindStringSubmatch(asciiLower(trimSpace(s)))
	if m == nil {
		return ""
	}
	if m[1] != "" {
		return m[1]
	}
	return m[2]
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// blockColourValue is a block's stored colour ("sky", "sky_background"), or
// "" when it isn't one of ours.
func blockColourValue(v any) string {
	s, _ := v.(string)
	colour, background, found := strings.Cut(s, "_")
	if !IsDocColour(colour) || found && background != "background" {
		return ""
	}
	return s
}
