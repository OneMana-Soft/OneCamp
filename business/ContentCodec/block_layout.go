package contentcodec

import (
	"strconv"
	"strings"
)

// B1's rules: a block's indent (0–8, data-indent) and its text direction
// ("ltr" or "rtl", dir; none is automatic). The same as the web app's
// lib/codec/blockLayout.ts. Columns are nodes of their own (schema.go): HTML
// keeps them, Markdown writes their blocks in order.

// indentNodes are the blocks Tab nests (the editor's block-commands/nodes.ts).
var indentNodes = []string{"blockquote", "bulletList", "callout", "chartEmbed", "codeBlock", "collapsible", "diffEmbed", "heading", "horizontalRule", "image", "orderedList", "paragraph", "poll", "recordingEmbed", "tableEmbed", "taskList"}

// dirNodes are the blocks with a direction of their own.
var dirNodes = []string{"blockquote", "bulletList", "callout", "collapsible", "heading", "orderedList", "paragraph", "taskList"}

const maxIndent = 8

// indentOf is an indent as a whole number in 0–8; anything else is 0.
func indentOf(v any) float64 {
	n := 0
	switch t := v.(type) {
	case float64:
		// Clamped first: int() of a float beyond int64 is the machine's choice.
		switch {
		case t != t || t < 0:
			n = 0
		case t > maxIndent:
			n = maxIndent
		default:
			n = int(t)
		}
	case int:
		n = t
	case string:
		s := strings.TrimSpace(t)
		if s != "" && strings.Trim(s, "0123456789") == "" {
			i, err := strconv.Atoi(s)
			if err != nil {
				// More digits than a number holds: as deep as it goes.
				i = maxIndent
			}
			n = i
		}
	}
	if n < 0 {
		n = 0
	}
	if n > maxIndent {
		n = maxIndent
	}
	return float64(n)
}

func dirOf(v any) any {
	if s, ok := v.(string); ok && (s == "ltr" || s == "rtl") {
		return s
	}
	return nil
}

var blockLayoutAttrs = []globalAttr{
	{
		name:  "indent",
		html:  "data-indent",
		on:    indentNodes,
		def:   float64(0),
		typed: func(v any) any { return indentOf(v) },
		// Another tool's data-indent means something else.
		read: func(raw string, foreign bool) any {
			if foreign {
				return float64(0)
			}
			return indentOf(raw)
		},
		write: func(v any) string {
			if i := indentOf(v); i > 0 {
				return itoa(int64(i))
			}
			return ""
		},
	},
	{
		name:  "dir",
		html:  "dir",
		on:    dirNodes,
		def:   nil,
		typed: dirOf,
		// Pasted text marked left to right (Google Docs marks every line so)
		// is left to its letters; one marked right to left keeps that.
		read: func(raw string, foreign bool) any {
			d := dirOf(strings.ToLower(strings.TrimSpace(raw)))
			if foreign && d == "ltr" {
				return nil
			}
			return d
		},
		write: func(v any) string {
			if d, ok := dirOf(v).(string); ok {
				return d
			}
			return ""
		},
	},
}
