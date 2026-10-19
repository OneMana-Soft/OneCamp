package business

// Writing a parsed text back: the canonical form, which parses to the same
// tree (spans aside). Capitals for keywords, one space between terms, AND
// written out where a compare-form clause or a group stands beside another,
// quotes only where a value needs them. lib/search/query.ts writes the same.

import (
	"strings"

	querySpec "github.com/akashc777/OneCamp/business/QuerySpec"
)

// String is the syntax tree as canonical text.
func (s Syntax) String() string {
	var b strings.Builder
	if s.Root != nil {
		b.WriteString(formatNode(s.Root))
	}
	if len(s.Sorts) > 0 {
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		b.WriteString("ORDER BY ")
		for i, k := range s.Sorts {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(formatField(k.Field, k.Quoted))
			if k.Desc {
				b.WriteString(" DESC")
			}
		}
	}
	return b.String()
}

func formatNode(n *Node) string {
	switch n.Kind {
	case NodeWord:
		return n.Text
	case NodePhrase:
		return quote(n.Text)
	case NodeClause:
		return formatClause(n)
	case NodeNot:
		child := n.Children[0]
		switch {
		case child.Kind == NodeAnd || child.Kind == NodeOr:
			return "NOT (" + formatNode(child) + ")"
		case child.Kind == NodeClause && child.Form != querySpec.FormModifier:
			return "NOT " + formatNode(child)
		}
		return "-" + formatNode(child)
	case NodeAnd:
		spelled := false
		parts := make([]string, len(n.Children))
		for i, c := range n.Children {
			if c.Kind == NodeOr {
				parts[i] = "(" + formatNode(c) + ")"
			} else {
				parts[i] = formatNode(c)
			}
			if spellsAnd(c) {
				spelled = true
			}
		}
		if spelled {
			return strings.Join(parts, " AND ")
		}
		return strings.Join(parts, " ")
	case NodeOr:
		parts := make([]string, len(n.Children))
		for i, c := range n.Children {
			parts[i] = formatNode(c)
		}
		return strings.Join(parts, " OR ")
	}
	return ""
}

// spellsAnd is a term that reads better with AND written beside it: a
// compare-form clause or a group.
func spellsAnd(n *Node) bool {
	switch n.Kind {
	case NodeClause:
		return n.Form != querySpec.FormModifier
	case NodeAnd, NodeOr:
		return true
	case NodeNot:
		return spellsAnd(n.Children[0])
	}
	return false
}

// The symbols as a compare-form clause writes them.
var compareSymbols = map[string]string{
	SymEq: "=", SymNe: "!=", SymLt: "<", SymLe: "<=", SymGt: ">", SymGe: ">=", SymContains: "~", SymNotContains: "!~",
}

func formatClause(n *Node) string {
	field := formatField(n.Field, n.Quoted)
	if n.Form == querySpec.FormModifier {
		op := ":"
		if n.Op != SymModifier {
			op = ":" + n.Op
		}
		if len(n.Values) == 0 {
			return field + op
		}
		v := n.Values[0]
		if needsQuote(v) || (n.Op == SymModifier && startsModifierOp(v)) {
			v = quote(v)
		}
		return field + op + v
	}
	switch n.Op {
	case SymEmpty:
		return field + " IS EMPTY"
	case SymNotEmpty:
		return field + " IS NOT EMPTY"
	case SymIn, SymNotIn:
		kw := " IN "
		if n.Op == SymNotIn {
			kw = " NOT IN "
		}
		return field + kw + formatList(n.Values)
	}
	sym := compareSymbols[n.Op]
	if n.List {
		return field + " " + sym + " " + formatList(n.Values)
	}
	if len(n.Values) == 0 {
		return field + " " + sym
	}
	v := n.Values[0]
	if needsQuote(v) {
		v = quote(v)
	}
	return field + " " + sym + " " + v
}

func formatList(values []string) string {
	parts := make([]string, len(values))
	for i, v := range values {
		if needsQuote(v) {
			v = quote(v)
		}
		parts[i] = v
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

func formatField(name string, quoted bool) string {
	if quoted || !isIdent(name) {
		return quote(name)
	}
	return name
}

// needsQuote is a value that wouldn't read back as one value unquoted.
func needsQuote(v string) bool {
	if v == "" {
		return true
	}
	for _, r := range v {
		if endsWord(r) {
			return true
		}
	}
	return false
}

// startsModifierOp is a value a modifier would read as starting with a
// comparison.
func startsModifierOp(v string) bool {
	for _, op := range modifierOps {
		if strings.HasPrefix(v, op) {
			return true
		}
	}
	return false
}

func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
