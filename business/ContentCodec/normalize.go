package contentcodec

import (
	"sort"
	"strconv"
	"strings"
)

// Normalize is a doc in the codec's canonical form, valid for schema:
//
//   - every node holds what its type may hold (a list holds items, an item
//     starts with a paragraph, a paragraph holds no blocks, a code block holds
//     unmarked text, a container is never empty);
//   - a node or mark the schema lacks is kept as well as it can be: a table as
//     its Markdown in a code block, a mention as its text, anything else by its
//     content;
//   - attributes are the type's own, defaults filled in, and those that are
//     null or empty left out;
//   - marks are in the schema's order, once each, and code excludes the rest;
//   - neighbouring text with the same marks is one text node.
//
// The fixtures hold canonical docs, and every reader in this package returns
// one, so two docs are the same doc when their canonical JSON is equal.
func Normalize(doc *Node, schema *Schema) *Node {
	if schema == nil {
		schema = shipped()
	}
	n := &normalizer{schema: schema}
	if doc == nil {
		doc = newNode("doc")
	}
	out := newNode("doc")
	if doc.Type == "doc" {
		out.Content = n.blocks(doc.Content)
	} else {
		out.Content = n.blocks([]*Node{doc})
	}
	if len(out.Content) == 0 {
		out.Content = []*Node{newNode("paragraph")}
	}
	return out
}

type normalizer struct {
	schema *Schema
}

// blocks normalises a run of children that must be blocks: inline runs are
// gathered into paragraphs.
func (n *normalizer) blocks(in []*Node) []*Node {
	var out []*Node
	var inline []*Node
	flush := func() {
		if len(inline) == 0 {
			return
		}
		p := newNode("paragraph")
		p.Content = n.inline(inline)
		inline = nil
		if len(p.Content) > 0 {
			out = append(out, p)
		}
	}
	for _, c := range in {
		if c == nil {
			continue
		}
		if isInlineNode(c) {
			inline = append(inline, c)
			continue
		}
		flush()
		out = append(out, n.block(c)...)
	}
	flush()
	return out
}

func isInlineNode(c *Node) bool {
	if c.Type == "text" {
		return true
	}
	s := specOf(c.Type)
	return s != nil && s.inline
}

// block normalises one block, which may become several (or none).
func (n *normalizer) block(c *Node) []*Node {
	spec := specOf(c.Type)
	if spec == nil {
		// A type no rule knows: its content, if it has any.
		return n.blocks(c.Content)
	}
	if !n.schema.Has(c.Type) {
		return n.degrade(c)
	}
	switch c.Type {
	case "doc":
		return n.blocks(c.Content)
	case "column":
		// A column outside a row of columns is its blocks.
		return n.blocks(c.Content)
	case "listItem", "taskItem":
		// An item outside a list: a list of its own.
		list := newNode("bulletList")
		if c.Type == "taskItem" {
			list = newNode("taskList")
		}
		list.Content = []*Node{c}
		return n.block(list)
	case "tableRow", "tableCell", "tableHeader":
		return n.blocks(c.Content)
	case "collapsible":
		// An old toggle kept its title in an attribute, beside a shared
		// open: the title becomes the toggle's first line, its summary (B2,
		// doc_format.go).
		_, title := c.Attrs["title"]
		_, open := c.Attrs["open"]
		if title || open {
			line := newNode("paragraph")
			if t, _ := c.Attrs["title"].(string); t != "" {
				line.Content = []*Node{{Type: "text", Text: t}}
			}
			c = &Node{Type: c.Type, Attrs: c.Attrs, Content: append([]*Node{line}, c.Content...)}
		}
	}
	out := &Node{Type: c.Type, Attrs: n.attrs(spec, c.Attrs)}
	switch spec.content {
	case holdsNothing:
		return []*Node{out}
	case holdsInline:
		// A textblock never holds a block: one found inside is lifted out, and
		// splits it.
		var res []*Node
		var run []*Node
		emit := func() {
			tb := &Node{Type: c.Type, Attrs: out.Attrs}
			tb.Content = n.inline(run)
			run = nil
			res = append(res, tb)
		}
		for _, k := range c.Content {
			if k == nil {
				continue
			}
			if isInlineNode(k) || specOf(k.Type) == nil && len(k.Content) == 0 && k.Text != "" {
				run = append(run, k)
				continue
			}
			if len(run) > 0 {
				emit()
			}
			res = append(res, n.block(k)...)
		}
		if len(run) > 0 || len(res) == 0 {
			emit()
		}
		return res
	case holdsText:
		out.Content = n.codeText(c.Content)
		return []*Node{out}
	case holdsBlocks:
		out.Content = n.blocks(c.Content)
		if len(out.Content) == 0 {
			out.Content = []*Node{newNode("paragraph")}
		}
		return []*Node{out}
	case holdsSummaryBody:
		out.Content = n.blocks(c.Content)
		if len(out.Content) == 0 || out.Content[0].Type != "paragraph" && out.Content[0].Type != "heading" {
			out.Content = append([]*Node{newNode("paragraph")}, out.Content...)
		}
		return []*Node{out}
	case holdsListItems, holdsTaskItems:
		item := "listItem"
		if spec.content == holdsTaskItems {
			item = "taskItem"
		}
		out.Content = n.items(c.Content, item)
		if len(out.Content) == 0 {
			return nil
		}
		return []*Node{out}
	case holdsRows:
		for _, r := range c.Content {
			if r == nil {
				continue
			}
			row := newNode("tableRow")
			cells := r.Content
			if r.Type != "tableRow" {
				cells = []*Node{r}
			}
			for _, cell := range cells {
				if cell == nil {
					continue
				}
				typ := cell.Type
				if typ != "tableCell" && typ != "tableHeader" {
					cell = &Node{Type: "tableCell", Content: []*Node{cell}}
					typ = "tableCell"
				}
				nc := &Node{Type: typ, Attrs: n.attrs(specOf(typ), cell.Attrs)}
				nc.Content = n.blocks(cell.Content)
				if len(nc.Content) == 0 {
					nc.Content = []*Node{newNode("paragraph")}
				}
				row.Content = append(row.Content, nc)
			}
			if len(row.Content) > 0 {
				out.Content = append(out.Content, row)
			}
		}
		if len(out.Content) == 0 {
			return nil
		}
		return []*Node{out}
	case holdsColumns:
		out.Content = n.columns(c.Content)
		if len(out.Content) == 0 {
			return nil
		}
		return []*Node{out}
	}
	return []*Node{out}
}

// columns normalises a row's children into columns: blocks found loose in a
// row make a column of their own.
func (n *normalizer) columns(in []*Node) []*Node {
	var out []*Node
	var loose []*Node
	flush := func() {
		if len(loose) > 0 {
			out = append(out, n.column(&Node{Type: "column", Content: loose})...)
		}
		loose = nil
	}
	for _, c := range in {
		if c == nil {
			continue
		}
		if c.Type == "column" {
			flush()
			out = append(out, n.column(c)...)
			continue
		}
		loose = append(loose, c)
	}
	flush()
	return out
}

// column normalises one column of a row: its width, and blocks (never none).
func (n *normalizer) column(c *Node) []*Node {
	if !n.schema.Has("column") {
		return n.blocks(c.Content)
	}
	out := &Node{Type: "column", Attrs: n.attrs(specOf("column"), c.Attrs)}
	out.Content = n.blocks(c.Content)
	if len(out.Content) == 0 {
		out.Content = []*Node{newNode("paragraph")}
	}
	return []*Node{out}
}

// items normalises a list's children into items of the list's kind.
func (n *normalizer) items(in []*Node, item string) []*Node {
	var out []*Node
	var loose []*Node // blocks found between items: they join the item before
	for _, c := range in {
		if c == nil {
			continue
		}
		if c.Type == "listItem" || c.Type == "taskItem" {
			// Its own attributes (a task's check) and any another package
			// puts on it (a colour).
			it := &Node{Type: item, Attrs: n.attrs(specOf(item), c.Attrs)}
			it.Content = n.itemBody(c.Content)
			out = append(out, it)
			continue
		}
		if len(out) == 0 {
			loose = append(loose, c)
			continue
		}
		last := out[len(out)-1]
		last.Content = n.itemBody(append(last.Content, c))
	}
	if len(loose) > 0 {
		it := &Node{Type: item, Attrs: n.attrs(specOf(item), nil)}
		it.Content = n.itemBody(loose)
		out = append([]*Node{it}, out...)
	}
	return out
}

// itemBody is an item's content: a paragraph first, then blocks.
func (n *normalizer) itemBody(in []*Node) []*Node {
	body := n.blocks(in)
	if len(body) == 0 || body[0].Type != "paragraph" {
		body = append([]*Node{newNode("paragraph")}, body...)
	}
	return body
}

// codeText is a code block's content: its text, unmarked, as one text node.
func (n *normalizer) codeText(in []*Node) []*Node {
	var b strings.Builder
	var walk func([]*Node)
	walk = func(ns []*Node) {
		for _, c := range ns {
			if c == nil {
				continue
			}
			switch c.Type {
			case "text":
				b.WriteString(c.Text)
			case "hardBreak":
				b.WriteByte('\n')
			default:
				if isInlineNode(c) {
					b.WriteString(PlainText(c))
				} else {
					if b.Len() > 0 {
						b.WriteByte('\n')
					}
					walk(c.Content)
				}
			}
		}
	}
	walk(in)
	if b.Len() == 0 {
		return nil
	}
	return []*Node{{Type: "text", Text: b.String()}}
}

// inline normalises inline content: marks in order, neighbouring text joined,
// empty text and inline nodes the schema lacks dropped (a mention it lacks
// becomes its text).
func (n *normalizer) inline(in []*Node) []*Node {
	var out []*Node
	// The text after the first of a run of texts with the same marks, joined
	// to it once when the run ends (joining at each copied the run so far).
	var run strings.Builder
	flush := func() {
		if run.Len() > 0 {
			out[len(out)-1].Text += run.String()
			run.Reset()
		}
	}
	for _, c := range in {
		if c == nil {
			continue
		}
		var k *Node
		switch {
		case c.Type == "text":
			if c.Text == "" {
				continue
			}
			k = &Node{Type: "text", Text: c.Text, Marks: n.marks(c.Marks)}
		case specOf(c.Type) != nil && specOf(c.Type).inline:
			if !n.schema.Has(c.Type) {
				t := PlainText(c)
				if t == "" {
					continue
				}
				k = &Node{Type: "text", Text: t, Marks: n.marks(c.Marks)}
			} else {
				k = &Node{Type: c.Type, Attrs: n.attrs(specOf(c.Type), c.Attrs), Marks: n.marks(c.Marks)}
			}
		default:
			// Not inline: its text, so nothing is lost.
			t := PlainText(c)
			if t == "" {
				continue
			}
			k = &Node{Type: "text", Text: t}
		}
		if k.Type == "text" && len(out) > 0 {
			prev := out[len(out)-1]
			if prev.Type == "text" && sameMarks(prev.Marks, k.Marks) {
				run.WriteString(k.Text)
				continue
			}
		}
		flush()
		out = append(out, k)
	}
	flush()
	return out
}

// marks is a node's marks in canonical order: known to the schema, once per
// type, code alone.
func (n *normalizer) marks(in []*Mark) []*Mark {
	byType := map[string]*Mark{}
	for _, m := range in {
		if m == nil {
			continue
		}
		spec := markSpecs[m.Type]
		if spec == nil || !n.schema.HasMark(m.Type) {
			continue
		}
		nm := &Mark{Type: m.Type, Attrs: n.attrs(nil, m.Attrs, spec.attrs...)}
		if m.Type == "link" && nm.attrString("href") == "" {
			continue
		}
		// Colours are names (B2, N15): a text colour that isn't one of ours
		// goes, and a highlight is yellow unless it names another.
		switch m.Type {
		case "textStyle":
			if c := colourName(nm.Attrs["color"]); c != "" {
				nm.Attrs["color"] = c
			} else {
				delete(nm.Attrs, "color")
			}
		case "highlight":
			nm.Attrs = map[string]any{"color": highlightColour(nm.Attrs["color"])}
		}
		if m.Type == "textStyle" && len(nm.Attrs) == 0 {
			continue
		}
		byType[m.Type] = nm
	}
	if c, ok := byType["code"]; ok {
		return []*Mark{c}
	}
	out := make([]*Mark, 0, len(byType))
	for _, m := range byType {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return markSpecs[out[i].Type].rank < markSpecs[out[j].Type].rank })
	if len(out) == 0 {
		return nil
	}
	return out
}

func sameMarks(a, b []*Mark) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].eq(b[i]) {
			return false
		}
	}
	return true
}

func (m *Mark) eq(o *Mark) bool {
	if m.Type != o.Type || len(m.Attrs) != len(o.Attrs) {
		return false
	}
	for k, v := range m.Attrs {
		if stringOf(v) != stringOf(o.Attrs[k]) {
			return false
		}
	}
	return true
}

// attrs is a node's attributes in canonical form: only the type's own,
// defaults filled in, values typed as the editor types them, null and empty
// ones left out.
func (n *normalizer) attrs(spec *nodeSpec, in map[string]any, markAttrs ...attrSpec) map[string]any {
	specs := markAttrs
	typ := ""
	if spec != nil {
		specs = spec.attrs
		typ = spec.name
	}
	var globals []globalAttr
	if spec != nil {
		globals = globalsOn(spec.name)
	}
	if len(specs) == 0 && len(globals) == 0 {
		return nil
	}
	out := map[string]any{}
	for _, a := range specs {
		v, ok := in[a.name]
		if !ok || v == nil {
			v = a.def
		}
		v = typedAttr(typ, a.name, v)
		if v == nil {
			continue
		}
		if s, ok := v.(string); ok && s == "" {
			continue
		}
		out[a.name] = v
	}
	// Another package's attributes on this block (an indent, a direction),
	// left out at their default.
	for _, g := range globals {
		if v := g.typed(in[g.name]); !g.isDefault(v) {
			out[g.name] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// typedAttr is an attribute's value as the editor's schema types it.
func typedAttr(typ, name string, v any) any {
	if t, ok := typedTechnicalAttr(typ, name, v); ok {
		return t
	}
	if m, ok := typedMediaAttr(typ, name, v); ok {
		return m
	}
	if c, ok := typedConnectedAttr(typ, name, v); ok {
		return c
	}
	switch {
	case typ == "heading" && name == "level":
		l := toInt(v, 1)
		if l < 1 {
			l = 1
		}
		if l > 6 {
			l = 6
		}
		return float64(l)
	case typ == "orderedList" && name == "start":
		s := toInt(v, 1)
		if s < 0 {
			s = 1
		}
		return float64(s)
	case typ == "taskItem" && name == "checked":
		switch t := v.(type) {
		case bool:
			return t
		case string:
			return t != "false" && t != ""
		}
		return v != nil
	case (typ == "tableCell" || typ == "tableHeader") && (name == "colspan" || name == "rowspan"):
		s := toInt(v, 1)
		if s < 1 {
			s = 1
		}
		return float64(s)
	case typ == "image":
		// Tiptap reads an attribute with no parse rule of its own through
		// fromString: a number-like value becomes a number, "true" and
		// "false" booleans.
		if s, ok := v.(string); ok {
			return fromString(s)
		}
		return v
	case typ == "callout" && name == "icon":
		return orNil(calloutIcon(v))
	case typ == "column" && name == "width":
		if w := toInt(v, 0); w >= 1 && w <= 99 {
			return float64(w)
		}
		return nil
	case typ == "orderedList" && name == "type", typ == "codeBlock" && name == "language":
		if s, ok := v.(string); ok {
			return s
		}
		return nil
	}
	return v
}

func toInt(v any, def int) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return def
		}
		return i
	}
	return def
}

// fromString is Tiptap's: "300" is 300, "true" true.
func fromString(s string) any {
	if s == "true" {
		return true
	}
	if s == "false" {
		return false
	}
	if isNumberLike(s) {
		f, err := strconv.ParseFloat(s, 64)
		if err == nil {
			return f
		}
	}
	return s
}

// isNumberLike matches Tiptap's /^[+-]?(?:\d*\.)?\d+$/.
func isNumberLike(s string) bool {
	if s == "" {
		return false
	}
	i := 0
	if s[0] == '+' || s[0] == '-' {
		i++
	}
	rest := s[i:]
	if rest == "" {
		return false
	}
	dot := strings.IndexByte(rest, '.')
	digits := func(t string, min int) bool {
		if len(t) < min {
			return false
		}
		for _, c := range t {
			if c < '0' || c > '9' {
				return false
			}
		}
		return true
	}
	if dot < 0 {
		return digits(rest, 1)
	}
	return digits(rest[:dot], 0) && digits(rest[dot+1:], 1)
}
