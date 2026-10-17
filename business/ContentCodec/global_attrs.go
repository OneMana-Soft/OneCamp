package contentcodec

import "strings"

// Attributes one package puts on blocks another owns (the editor's global
// attributes: B1's indent and direction; B2's block colour), as the codec
// reads and writes them: the same table, rule for rule, as the web app's
// lib/codec/globalAttrs.ts.
//
// A package keeps its rules in a file of its own and adds them to
// globalAttrs with one line. Normalising keeps an attribute only on the nodes
// it names, typed, and leaves it out at its default, so a doc without any
// reads as before. HTML carries each as its own attribute, after the node's
// own; Markdown has no way to say them and leaves them out.

type globalAttr struct {
	// name is the editor's attribute: "indent".
	name string
	// html is its HTML attribute: "data-indent".
	html string
	// on is the node types it goes on.
	on []string
	// def is its default, which is never written.
	def any
	// typed is the value as the editor's schema types it, or nil.
	typed func(v any) any
	// read is the value from HTML; foreign HTML (a paste, an import) may say less.
	read func(raw string, foreign bool) any
	// write is the HTML value, or "" to leave it out.
	write func(v any) string
}

var globalAttrs = concatGlobals(
	// One line per package: its rules, in a file of its own.
	blockLayoutAttrs, // B1: indent and direction
	docFormatAttrs,   // B2: a block's colour, a large quote (doc_format.go)
	connectedAttrs,   // C1: a to-do's task (connected.go)
)

func concatGlobals(sets ...[]globalAttr) []globalAttr {
	var out []globalAttr
	for _, s := range sets {
		out = append(out, s...)
	}
	return out
}

// globalsOn is the global attributes on a node type, in table order.
func globalsOn(typ string) []globalAttr {
	var out []globalAttr
	for _, g := range globalAttrs {
		for _, t := range g.on {
			if t == typ {
				out = append(out, g)
				break
			}
		}
	}
	return out
}

// writeGlobals writes a node's global attributes as HTML attributes, in
// table order.
func writeGlobals(b *strings.Builder, n *Node) {
	for _, g := range globalsOn(n.Type) {
		if v := g.write(g.typed(n.attr(g.name))); v != "" {
			writeAttr(b, g.html, v)
		}
	}
}

// isDefault reports whether a typed global value is its default (or none).
func (g globalAttr) isDefault(v any) bool {
	if v == nil {
		return true
	}
	if s, ok := v.(string); ok && s == "" {
		return true
	}
	return v == g.def
}
