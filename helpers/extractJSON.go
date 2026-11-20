package helpers

import (
	"encoding/json"
	"errors"
	"strings"
)

// ExtractJSON is the first complete JSON value of the kind asked for ('{' for
// an object, '[' for an array) in a model's reply, or "" when it holds none.
//
// Models wrap the JSON they were asked for in code fences and prose, and
// sometimes write a second value after it ("for example: {...}"). Taking the
// first opening bracket to the last closing one joined those into text that
// isn't JSON, so the reply was thrown away. This scans each candidate from its
// opening bracket to the bracket that closes it, skipping brackets inside
// strings (and escaped quotes inside those), and returns the first candidate
// that is valid JSON. A candidate that closes but isn't JSON ("[see below]",
// a malformed tool call) is stepped over whole, so nothing inside it is taken
// for the answer. One that never closes is a value cut off (at the reply's max
// tokens), and the reply has no answer, unless it stops being JSON part-way
// (prose such as "{ is a brace"): the scan goes on from where it stopped.
// Each candidate is scanned to its end, so a reply of n unclosed brackets
// costs O(n^2); a model's max tokens bound n. Pure.
func ExtractJSON(s string, kind byte) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	for from := 0; from < len(s); {
		at := strings.IndexByte(s[from:], kind)
		if at < 0 {
			return ""
		}
		start := from + at
		end := closingBracket(s, start)
		if end >= 0 {
			if v := s[start : end+1]; json.Valid([]byte(v)) {
				return v
			}
			from = end + 1
			continue
		}
		var cut json.RawMessage
		var syntax *json.SyntaxError
		if err := json.NewDecoder(strings.NewReader(s[start:])).Decode(&cut); !errors.As(err, &syntax) {
			return ""
		}
		from = start + max(1, int(syntax.Offset)-1)
	}
	return ""
}

// closingBracket is the index of the bracket that closes the one at start,
// counting both kinds and skipping strings, or -1 when the text ends first.
func closingBracket(s string, start int) int {
	depth := 0
	inString, escaped := false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}
