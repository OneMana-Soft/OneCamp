// Package htmldom is what the import adapters read other tools' HTML exports
// with: finding a page's parts by id and class, and rewriting a tool's own
// markup into the HTML a doc's blocks read (business/ContentCodec FromHTML).
package htmldom

import (
	"bytes"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// First is the first element under n, in document order, that matches.
func First(n *html.Node, match func(*html.Node) bool) *html.Node {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && match(c) {
			return c
		}
		if f := First(c, match); f != nil {
			return f
		}
	}
	return nil
}

// All is every element under n that matches, in document order, not looking
// inside a match.
func All(n *html.Node, match func(*html.Node) bool) []*html.Node {
	var out []*html.Node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && match(c) {
			out = append(out, c)
			continue
		}
		out = append(out, All(c, match)...)
	}
	return out
}

// Nested is every element under n that matches, those inside another match
// included, innermost first: rewriting each in turn leaves none inside another
// unrewritten (a toggle in a toggle, a to-do list in a to-do).
func Nested(n *html.Node, match func(*html.Node) bool) []*html.Node {
	var out []*html.Node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		out = append(out, Nested(c, match)...)
		if c.Type == html.ElementNode && match(c) {
			out = append(out, c)
		}
	}
	return out
}

// ByID is the element under n with this id.
func ByID(n *html.Node, id string) *html.Node {
	return First(n, func(x *html.Node) bool { return Attr(x, "id") == id })
}

// Attr is an attribute's value, "" when it has none.
func Attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// HasClass says whether the element has the class.
func HasClass(n *html.Node, class string) bool {
	for _, c := range strings.Fields(Attr(n, "class")) {
		if c == class {
			return true
		}
	}
	return false
}

// Classed matches elements with the class.
func Classed(class string) func(*html.Node) bool {
	return func(n *html.Node) bool { return HasClass(n, class) }
}

// Tag matches elements of the tag.
func Tag(a atom.Atom) func(*html.Node) bool {
	return func(n *html.Node) bool { return n.DataAtom == a }
}

// Text is a node's text, its whitespace collapsed.
func Text(n *html.Node) string {
	return strings.Join(strings.Fields(RawText(n)), " ")
}

// RawText is a node's text as it is: a code block's lines.
func RawText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		if x.Type == html.TextNode {
			b.WriteString(x.Data)
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// Element makes an element holding kids, then the children of from (moved).
func Element(a atom.Atom, kids []*html.Node, from *html.Node) *html.Node {
	n := &html.Node{Type: html.ElementNode, DataAtom: a, Data: a.String()}
	for _, k := range kids {
		n.AppendChild(k)
	}
	if from != nil {
		for c := from.FirstChild; c != nil; {
			next := c.NextSibling
			from.RemoveChild(c)
			n.AppendChild(c)
			c = next
		}
	}
	return n
}

// TextNode is a text node.
func TextNode(s string) *html.Node { return &html.Node{Type: html.TextNode, Data: s} }

// Checkbox is a checkbox input, ticked or not: what a to-do item starts with.
func Checkbox(checked bool) *html.Node {
	box := &html.Node{Type: html.ElementNode, DataAtom: atom.Input, Data: "input", Attr: []html.Attribute{{Key: "type", Val: "checkbox"}}}
	if checked {
		box.Attr = append(box.Attr, html.Attribute{Key: "checked"})
	}
	return box
}

// Replace puts with where old is.
func Replace(old, with *html.Node) {
	old.Parent.InsertBefore(with, old)
	old.Parent.RemoveChild(old)
}

// Remove takes n out of the tree.
func Remove(n *html.Node) {
	if n.Parent != nil {
		n.Parent.RemoveChild(n)
	}
}

// Inner is the HTML of n's children.
func Inner(n *html.Node) (string, error) {
	var b bytes.Buffer
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if err := html.Render(&b, c); err != nil {
			return "", err
		}
	}
	return b.String(), nil
}
