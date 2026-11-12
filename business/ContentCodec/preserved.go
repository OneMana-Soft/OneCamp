package contentcodec

// Reading what the collaboration service keeps (V-528 (1)).
//
// When the service writes a doc's body with a schema that lacks one of its
// nodes or marks (the web shipped a block first: deploys go web first, the
// backend right behind), it keeps each one as a marker
// (other-services/collaboration-service/preserved.js): a node as
// <div|span data-oc-preserved="name" data-oc-holds="none|inline|block"
// data-oc-attrs="{…}"> with its content inside, a mark as
// <span data-oc-mark="name" data-oc-attrs="{…}"> around what it marks. Such a
// body stays stored until the doc is next saved. The service reads markers
// back; so does every reader here (exports, AI reads, the API's GET, Blocks),
// once this codec knows the name: the node or mark under its own name, with
// the marker's attributes, which pass the same admission as its tag twin's
// (admit.go) and which Normalize then types and checks as it does a doc's
// JSON. A name this codec does not know reads as before (its content), and
// markers count only in our own bodies, never in foreign HTML.

import (
	"encoding/json"
	"strings"

	"golang.org/x/net/html"
)

// keptAttrs is a marker's attributes: its data-oc-attrs JSON object, or none.
func keptAttrs(n *html.Node) map[string]any {
	var out map[string]any
	if json.Unmarshal([]byte(attrVal(n, "data-oc-attrs")), &out) != nil {
		return nil
	}
	return out
}

// keptMark is the mark a marker span keeps, or nil when n is none or keeps a
// mark this codec does not know.
func (p *htmlParser) keptMark(n *html.Node) *Mark {
	name, ok := attrOf(n, "data-oc-mark")
	if !ok || p.foreign || markSpecs[name] == nil {
		return nil
	}
	m := &Mark{Type: name, Attrs: keptAttrs(n)}
	if !admitMark(m, p.foreign) {
		return nil
	}
	return m
}

// keptNode adds the node a marker keeps, answering false when n is no marker
// or keeps a node this codec does not know.
func (p *htmlParser) keptNode(n *html.Node) bool {
	name, ok := attrOf(n, "data-oc-preserved")
	if !ok || p.foreign || name == "doc" || name == "text" {
		return false
	}
	spec := specOf(name)
	if spec == nil {
		return false
	}
	node := newNode(name)
	node.Attrs = keptAttrs(n)
	if !admitNode(node, p.foreign, p.assets) {
		// As its tag twin reads: its words, then its content as any element's.
		if text := refusedText(node); text != "" {
			p.addTextValue(text)
		}
		return false
	}
	switch {
	case spec.inline:
		p.addInline(node)
	case spec.content == holdsNothing:
		p.atom(node)
	case spec.content == holdsText:
		// Text as it is, whitespace and all, as a <pre> is read.
		p.closeInline()
		p.openBlock(node, true)
		pn := p.top()
		var text strings.Builder
		preText(n, &text)
		if text.Len() > 0 {
			node.Content = []*Node{{Type: "text", Text: text.String()}}
		}
		p.closeTo(pn)
	default:
		p.closeInline()
		p.openBlock(node, false)
		pn := p.top()
		p.addChildren(n)
		p.closeTo(pn)
	}
	return true
}
