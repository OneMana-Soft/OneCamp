// Package contentcodec converts a doc between the three forms it travels in:
// the editor's JSON (ProseMirror's, as Tiptap writes it), HTML (how the doc is
// stored, and how the collaboration service is given a body) and Markdown (how
// people and agents read and write it).
//
// The web app has the same codec in TypeScript (lib/codec). Both are held to
// the golden fixtures in testdata/codec, byte for byte, so a doc exported on
// the server and in the browser is the same file.
package contentcodec

import (
	"encoding/json"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
)

// Node is one node of a doc, in the editor's JSON.
type Node struct {
	Type    string         `json:"type"`
	Attrs   map[string]any `json:"attrs,omitempty"`
	Content []*Node        `json:"content,omitempty"`
	Marks   []*Mark        `json:"marks,omitempty"`
	Text    string         `json:"text,omitempty"`
}

// Mark is a mark on a text node or an inline node.
type Mark struct {
	Type  string         `json:"type"`
	Attrs map[string]any `json:"attrs,omitempty"`
}

func newNode(typ string) *Node { return &Node{Type: typ} }

func (n *Node) attr(name string) any {
	if n == nil || n.Attrs == nil {
		return nil
	}
	return n.Attrs[name]
}

// attrString is an attribute as a string: "" when it is missing, not a string
// or empty.
func (n *Node) attrString(name string) string {
	return stringOf(n.attr(name))
}

func (m *Mark) attrString(name string) string {
	if m == nil || m.Attrs == nil {
		return ""
	}
	return stringOf(m.Attrs[name])
}

func stringOf(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return formatNumber(t)
	case int:
		return formatNumber(float64(t))
	case bool:
		if t {
			return "true"
		}
		return "false"
	}
	return ""
}

// attrInt is a whole-number attribute, or def.
func (n *Node) attrInt(name string, def int) int {
	switch t := n.attr(name).(type) {
	case float64:
		return int(t)
	case int:
		return t
	case string:
		v := 0
		if t == "" {
			return def
		}
		for _, c := range t {
			if c < '0' || c > '9' {
				return def
			}
			v = v*10 + int(c-'0')
			if v > 1<<30 {
				return def
			}
		}
		return v
	}
	return def
}

func (n *Node) attrBool(name string, def bool) bool {
	switch t := n.attr(name).(type) {
	case bool:
		return t
	case string:
		return t == "true"
	}
	return def
}

func (n *Node) setAttr(name string, v any) {
	if n.Attrs == nil {
		n.Attrs = map[string]any{}
	}
	n.Attrs[name] = v
}

// formatNumber writes a number the way JavaScript's String(n) does for the
// values a doc holds (integers, and the odd decimal width).
func formatNumber(f float64) string {
	if f == float64(int64(f)) && f < 1e15 && f > -1e15 {
		return itoa(int64(f))
	}
	b, _ := json.Marshal(f)
	return string(b)
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [24]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// Walk visits the node and everything under it, parents first. Returning
// false from visit skips what is under that node.
func (n *Node) Walk(visit func(*Node) bool) {
	if n == nil || !visit(n) {
		return
	}
	for _, c := range n.Content {
		c.Walk(visit)
	}
}

// PlainText is the doc's text: blocks on lines of their own, inline atoms by
// their label. It is what search and previews read.
func PlainText(n *Node) string {
	var b strings.Builder
	plainText(&b, n)
	return strings.TrimFunc(b.String(), isSpaceRune)
}

func plainText(b *strings.Builder, n *Node) {
	if n == nil {
		return
	}
	switch n.Type {
	case "text":
		b.WriteString(n.Text)
		return
	case "hardBreak":
		b.WriteByte('\n')
		return
	case "mention":
		b.WriteString("@" + helpers.FirstNonEmpty(n.attrString("label"), n.attrString("id")))
		return
	case "channelMention":
		b.WriteString("#" + helpers.FirstNonEmpty(n.attrString("label"), n.attrString("id")))
		return
	case "entityMention":
		b.WriteString(helpers.FirstNonEmpty(n.attrString("label"), n.attrString("id")))
		return
	case "inlineMath":
		b.WriteString("$" + n.attrString("latex") + "$")
		return
	case "blockMath":
		b.WriteString(n.attrString("latex"))
	case "linkMention":
		b.WriteString(helpers.FirstNonEmpty(n.attrString("title"), n.attrString("href")))
		return
	case "dateMention":
		b.WriteString(dateWords(n.attr("date")))
		return
	}
	spec := specOf(n.Type)
	for _, c := range n.Content {
		plainText(b, c)
	}
	if spec == nil || !spec.inline {
		if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
			b.WriteByte('\n')
		}
	}
}
