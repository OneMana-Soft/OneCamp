package business

// The lexer: a text into words, quoted strings, brackets, commas, operators
// and "-" (design/QUERY_STACK_CONTRACT.md §4.1). It knows nothing of fields;
// the parser decides what each token is in its place. lib/search/query.ts
// lexes the same way, rule for rule.

import (
	"unicode"
	"unicode/utf16"
)

type tokKind int

const (
	tWord tokKind = iota
	tQuoted
	tLParen
	tRParen
	tComma
	tOp
	tNeg
)

type tok struct {
	kind tokKind
	// text is a word as written, a quoted string's content without its
	// quotes and escapes, or an operator's symbol.
	text string
	// start and end are rune indexes, [start, end).
	start, end int
	// closed is a quoted string that had its closing quote.
	closed bool
	// glued is a token that starts where the one before it ended.
	glued bool
}

// source is a text as runes, with each rune's place in UTF-16 code units.
type source struct {
	runes []rune
	u16   []int // u16[i] is where rune i starts; u16[len(runes)] is the length
}

func newSource(text string) source {
	runes := []rune(text)
	u16 := make([]int, len(runes)+1)
	at := 0
	for i, r := range runes {
		u16[i] = at
		at += len(utf16.Encode([]rune{r}))
	}
	u16[len(runes)] = at
	return source{runes: runes, u16: u16}
}

// span is runes [start, end) in UTF-16 code units.
func (s source) span(start, end int) [2]int { return [2]int{s.u16[start], s.u16[end]} }

func (s source) text(start, end int) string { return string(s.runes[start:end]) }

// The operators, longest first.
var ops = []string{"!=", "<=", ">=", "!~", "=", "<", ">", "~"}

// leadingOp is the operator rs starts with, or "".
func leadingOp(rs []rune) string {
	for _, op := range ops {
		if len(rs) >= len(op) && string(rs[:len(op)]) == op {
			return op
		}
	}
	return ""
}

// identLen is how many runes of rs make a field name: [A-Za-z_][A-Za-z0-9_]*.
func identLen(rs []rune) int {
	n := 0
	for n < len(rs) {
		r := rs[n]
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (n > 0 && r >= '0' && r <= '9') {
			n++
			continue
		}
		break
	}
	return n
}

// isIdent is whether s is a field name and nothing else.
func isIdent(s string) bool {
	rs := []rune(s)
	return len(rs) > 0 && identLen(rs) == len(rs)
}

func endsWord(r rune) bool {
	return unicode.IsSpace(r) || r == '(' || r == ')' || r == '"' || r == ','
}

func lex(src source) []tok {
	rs := src.runes
	var toks []tok
	prevEnd := -1
	add := func(t tok) {
		t.glued = t.start == prevEnd
		toks = append(toks, t)
		prevEnd = t.end
	}
	i := 0
	for i < len(rs) {
		r := rs[i]
		switch {
		case unicode.IsSpace(r):
			i++
		case r == '(':
			add(tok{kind: tLParen, text: "(", start: i, end: i + 1})
			i++
		case r == ')':
			add(tok{kind: tRParen, text: ")", start: i, end: i + 1})
			i++
		case r == ',':
			add(tok{kind: tComma, text: ",", start: i, end: i + 1})
			i++
		case r == '"':
			j := i + 1
			var text []rune
			closed := false
			for j < len(rs) {
				if rs[j] == '\\' && j+1 < len(rs) && (rs[j+1] == '"' || rs[j+1] == '\\') {
					text = append(text, rs[j+1])
					j += 2
					continue
				}
				if rs[j] == '"' {
					closed = true
					j++
					break
				}
				text = append(text, rs[j])
				j++
			}
			add(tok{kind: tQuoted, text: string(text), start: i, end: j, closed: closed})
			i = j
		default:
			j := i
			for j < len(rs) && !endsWord(rs[j]) {
				j++
			}
			// A "-" before anything at all, a bracket or a quote included, is
			// a negation: -word, -"a b", -(a OR b), -from:@sam.
			if r == '-' && i+1 < len(rs) && !unicode.IsSpace(rs[i+1]) && rs[i+1] != ')' && rs[i+1] != ',' {
				add(tok{kind: tNeg, text: "-", start: i, end: i + 1})
				i++
				continue
			}
			for _, t := range splitWord(rs, i, j) {
				add(t)
			}
			i = j
		}
	}
	return toks
}

// splitWord is the word rs[start:end] as tokens: an operator it starts with,
// or a field name and the operator touching it (status=done, due<=today), and
// what follows, the same way again. A field name followed by ":" is one word:
// the parser reads modifiers.
func splitWord(rs []rune, start, end int) []tok {
	w := rs[start:end]
	if op := leadingOp(w); op != "" {
		out := []tok{{kind: tOp, text: op, start: start, end: start + len(op)}}
		if start+len(op) < end {
			out = append(out, splitWord(rs, start+len(op), end)...)
		}
		return out
	}
	if n := identLen(w); n > 0 && n < len(w) {
		if op := leadingOp(w[n:]); op != "" {
			out := []tok{
				{kind: tWord, text: string(w[:n]), start: start, end: start + n},
				{kind: tOp, text: op, start: start + n, end: start + n + len(op)},
			}
			if start+n+len(op) < end {
				out = append(out, splitWord(rs, start+n+len(op), end)...)
			}
			return out
		}
	}
	return []tok{{kind: tWord, text: string(w), start: start, end: end}}
}
