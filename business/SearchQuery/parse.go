package business

// The parser (design/QUERY_STACK_CONTRACT.md §4.2): recursive descent over
// the lexer's tokens. It never fails: what it can't read becomes words, and
// an issue says where. lib/search/query.ts parses the same way, rule for rule;
// the fixtures in testdata/searchquery hold the two to one tree.
//
//	query    = [ expr ] [ order ]
//	order    = "ORDER BY" sortkey *( "," sortkey )
//	sortkey  = field [ "ASC" | "DESC" ]
//	expr     = and *( "OR" and )
//	and      = unary *( [ "AND" ] unary )
//	unary    = ( "NOT" | "-" ) unary | atom
//	atom     = "(" expr ")" | clause | phrase | word
//	clause   = field ":" [ cmp ] value
//	         | field cmp value
//	         | field ( "~" | "!~" ) value
//	         | field ( "IN" | "NOT IN" ) "(" value *( "," value ) ")"
//	         | field "IS" [ "NOT" ] "EMPTY"
//
// AND, OR and NOT are operators anywhere in capitals; in lower case only
// between clauses or brackets, so "terms and conditions" stays three words.

import (
	"strings"

	querySpec "github.com/akashc777/OneCamp/business/QuerySpec"
)

// Parse reads text into its syntax tree. Pure; it never fails.
func Parse(text string) Syntax {
	src := newSource(text)
	p := &parser{src: src, toks: lex(src)}
	if src.u16[len(src.runes)] > MaxText {
		p.issue(IssueTooLong, [2]int{MaxText, src.u16[len(src.runes)]})
	}
	root := p.parseOr(0)
	if p.atOrderBy(p.i) {
		p.parseOrder()
	}
	p.syn.Root = root
	return p.syn
}

type parser struct {
	src  source
	toks []tok
	i    int
	syn  Syntax
}

func (p *parser) issue(code string, span [2]int) {
	p.syn.Issues = append(p.syn.Issues, Issue{Code: code, Span: span})
}

func (p *parser) token(kind string, start, end int) {
	p.syn.Tokens = append(p.syn.Tokens, Token{Kind: kind, Span: p.src.span(start, end)})
}

func (p *parser) spanOf(t tok) querySpec.Span { return p.src.span(t.start, t.end) }

// end is where the text ends, in runes.
func (p *parser) end() int { return len(p.src.runes) }

func (p *parser) word(i int, w string) bool {
	return i < len(p.toks) && p.toks[i].kind == tWord && asciiFold(p.toks[i].text, w)
}

// asciiFold is whether a equals b ignoring the case of ASCII letters only:
// keywords are ASCII, and Unicode case folding (the Kelvin sign is k to
// strings.EqualFold) would let the Go and TypeScript parsers disagree.
func asciiFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// clauseLike is a node that came from a clause or brackets: what a lower-case
// and, or or not may join.
func clauseLike(n *Node) bool {
	if n == nil {
		return false
	}
	switch n.Kind {
	case NodeClause, NodeAnd, NodeOr:
		return true
	case NodeNot:
		return clauseLike(n.Children[0])
	}
	return false
}

// startsAtom is whether token i can begin a term.
func (p *parser) startsAtom(i int, depth int) bool {
	if i >= len(p.toks) {
		return false
	}
	switch p.toks[i].kind {
	case tRParen:
		return false
	case tComma:
		return true
	}
	return !(depth == 0 && p.atOrderBy(i))
}

// startsClauseOrGroup is whether token i begins a clause or brackets, maybe
// negated.
func (p *parser) startsClauseOrGroup(i int) bool {
	if i >= len(p.toks) {
		return false
	}
	switch p.toks[i].kind {
	case tLParen:
		return true
	case tNeg:
		return p.startsClauseOrGroup(i + 1)
	}
	_, _, ok := p.clauseAt(i)
	return ok
}

func (p *parser) isOrOp(i int, last *Node, depth int) bool {
	if i >= len(p.toks) || p.toks[i].kind != tWord || last == nil {
		return false
	}
	t := p.toks[i].text
	if t == "OR" {
		return p.startsAtom(i+1, depth)
	}
	return asciiFold(t, "or") && clauseLike(last) && p.startsClauseOrGroup(i+1)
}

func (p *parser) isAndOp(i int, last *Node, depth int) bool {
	if i >= len(p.toks) || p.toks[i].kind != tWord || last == nil {
		return false
	}
	t := p.toks[i].text
	if t == "AND" {
		return p.startsAtom(i+1, depth)
	}
	return asciiFold(t, "and") && clauseLike(last) && p.startsClauseOrGroup(i+1)
}

func (p *parser) isNotOp(i int, last *Node, depth int) bool {
	if i >= len(p.toks) || p.toks[i].kind != tWord {
		return false
	}
	t := p.toks[i].text
	if t == "NOT" {
		return p.startsAtom(i+1, depth)
	}
	return asciiFold(t, "not") && (last == nil || clauseLike(last)) && p.startsClauseOrGroup(i+1)
}

func (p *parser) parseOr(depth int) *Node {
	var items []*Node
	first := p.parseAnd(depth)
	if first != nil {
		items = append(items, first)
	}
	for p.isOrOp(p.i, lastOf(items), depth) {
		t := p.toks[p.i]
		p.token(TokenKeyword, t.start, t.end)
		p.i++
		if next := p.parseAnd(depth); next != nil {
			items = append(items, next)
		}
	}
	return join(NodeOr, items)
}

func lastOf(items []*Node) *Node {
	if len(items) == 0 {
		return nil
	}
	return items[len(items)-1]
}

func (p *parser) parseAnd(depth int) *Node {
	var items []*Node
	for p.i < len(p.toks) {
		t := p.toks[p.i]
		if t.kind == tRParen {
			if depth > 0 {
				break
			}
			p.issue(IssueStrayParen, p.spanOf(t))
			p.token(TokenParen, t.start, t.end)
			p.i++
			continue
		}
		if depth == 0 && p.atOrderBy(p.i) {
			break
		}
		last := lastOf(items)
		if p.isOrOp(p.i, last, depth) {
			break
		}
		if p.isAndOp(p.i, last, depth) {
			p.token(TokenKeyword, t.start, t.end)
			p.i++
			continue
		}
		if n := p.parseUnary(depth, last); n != nil {
			items = append(items, n)
		}
	}
	return join(NodeAnd, items)
}

// join is items joined by kind: nothing, the one item, or a group, with
// groups of the same kind folded in ((a b) c is a b c).
func join(kind string, items []*Node) *Node {
	switch len(items) {
	case 0:
		return nil
	case 1:
		return items[0]
	}
	var children []*Node
	for _, it := range items {
		if it.Kind == kind {
			children = append(children, it.Children...)
		} else {
			children = append(children, it)
		}
	}
	return &Node{Kind: kind, Children: children, Span: querySpec.Span{children[0].Span[0], children[len(children)-1].Span[1]}}
}

func (p *parser) parseUnary(depth int, last *Node) *Node {
	t := p.toks[p.i]
	if t.kind == tNeg || p.isNotOp(p.i, last, depth) {
		if t.kind == tNeg {
			p.token(TokenNeg, t.start, t.end)
		} else {
			p.token(TokenKeyword, t.start, t.end)
		}
		p.i++
		if p.i >= len(p.toks) || !p.startsAtom(p.i, depth) {
			return &Node{Kind: NodeWord, Text: t.text, Span: p.spanOf(t)}
		}
		operand := p.parseUnary(depth, nil)
		if operand == nil {
			return nil
		}
		return &Node{Kind: NodeNot, Children: []*Node{operand}, Span: querySpec.Span{p.src.u16[t.start], operand.Span[1]}}
	}
	return p.parseAtom(depth)
}

func (p *parser) parseAtom(depth int) *Node {
	t := p.toks[p.i]
	switch t.kind {
	case tLParen:
		open := t
		p.token(TokenParen, t.start, t.end)
		p.i++
		if depth+1 > MaxBrackets {
			p.issue(IssueTooDeep, p.src.span(open.start, p.closeOf(p.i-1)))
		}
		inner := p.parseOr(depth + 1)
		if p.i < len(p.toks) && p.toks[p.i].kind == tRParen {
			c := p.toks[p.i]
			p.token(TokenParen, c.start, c.end)
			p.i++
			if inner != nil && (inner.Kind == NodeAnd || inner.Kind == NodeOr) {
				inner.Span = p.src.span(open.start, c.end)
			}
		} else {
			p.issue(IssueUnclosedParen, p.src.span(open.start, p.end()))
		}
		return inner
	case tComma:
		p.i++
		return &Node{Kind: NodeWord, Text: ",", Span: p.spanOf(t)}
	case tQuoted:
		if n, next, ok := p.clauseAt(p.i); ok {
			p.commitClause(p.i, next, n)
			return n
		}
		p.token(TokenPhrase, t.start, t.end)
		if !t.closed {
			p.issue(IssueUnclosedQuote, p.spanOf(t))
		}
		p.i++
		return &Node{Kind: NodePhrase, Text: t.text, Span: p.spanOf(t)}
	}
	if n, next, ok := p.clauseAt(p.i); ok {
		p.commitClause(p.i, next, n)
		return n
	}
	// A word, with whatever touches it (<3 is one word).
	text, start, end, next := p.rawValue(p.i)
	p.token(TokenWord, start, end)
	p.i = next
	return &Node{Kind: NodeWord, Text: text, Span: p.src.span(start, end)}
}

// closeOf is the rune where the bracket at token i closes, or the text's end.
func (p *parser) closeOf(i int) int {
	depth := 0
	for j := i; j < len(p.toks); j++ {
		switch p.toks[j].kind {
		case tLParen:
			depth++
		case tRParen:
			depth--
			if depth == 0 {
				return p.toks[j].end
			}
		}
	}
	return p.end()
}

// rawValue is the text of token i and every token touching it after it, up
// to a space, a bracket, a quote or a comma: a value as typed (-7d, a=b, <3).
func (p *parser) rawValue(i int) (text string, start, end, next int) {
	j := i
	for j+1 < len(p.toks) && p.toks[j+1].glued {
		k := p.toks[j+1].kind
		if k != tWord && k != tOp && k != tNeg {
			break
		}
		j++
	}
	start, end = p.toks[i].start, p.toks[j].end
	return p.src.text(start, end), start, end, j + 1
}

// clauseParts are where a clause's pieces sit, for its tokens.
type clauseParts struct {
	field, op [2]int   // runes
	keywords  [][2]int // IN, NOT, IS, EMPTY
	values    [][2]int
	phrases   [][2]int // quoted values
	parens    [][2]int
	commas    [][2]int
	issues    []Issue
}

// clauseAt reads a clause at token i, if one starts there: the clause, the
// token after it, and whether there was one. It changes nothing; the caller
// commits it.
func (p *parser) clauseAt(i int) (*Node, int, bool) {
	n, next, _, ok := p.readClause(i)
	return n, next, ok
}

func (p *parser) commitClause(i, next int, n *Node) {
	_, _, parts, _ := p.readClause(i)
	p.token(TokenField, parts.field[0], parts.field[1])
	// Tokens in the order they sit in the text.
	type piece struct {
		kind       string
		start, end int
	}
	pieces := []piece{}
	if parts.op[1] > parts.op[0] {
		pieces = append(pieces, piece{TokenOp, parts.op[0], parts.op[1]})
	}
	for _, k := range parts.keywords {
		pieces = append(pieces, piece{TokenKeyword, k[0], k[1]})
	}
	for _, v := range parts.values {
		pieces = append(pieces, piece{TokenValue, v[0], v[1]})
	}
	for _, v := range parts.phrases {
		pieces = append(pieces, piece{TokenPhrase, v[0], v[1]})
	}
	for _, v := range parts.parens {
		pieces = append(pieces, piece{TokenParen, v[0], v[1]})
	}
	for _, v := range parts.commas {
		pieces = append(pieces, piece{TokenComma, v[0], v[1]})
	}
	for a := 1; a < len(pieces); a++ {
		for b := a; b > 0 && pieces[b].start < pieces[b-1].start; b-- {
			pieces[b], pieces[b-1] = pieces[b-1], pieces[b]
		}
	}
	for _, pc := range pieces {
		p.token(pc.kind, pc.start, pc.end)
	}
	p.syn.Issues = append(p.syn.Issues, parts.issues...)
	p.i = next
}

// modifierOps are the comparisons a modifier's value may start with.
var modifierOps = []string{"!=", "<=", ">=", "=", "<", ">"}

func (p *parser) readClause(i int) (*Node, int, clauseParts, bool) {
	var parts clauseParts
	if i >= len(p.toks) {
		return nil, i, parts, false
	}
	t := p.toks[i]

	// A modifier: from:@sam, due:<friday, "Stage":done.
	modifier := func(field string, quoted bool, fieldStart, fieldEnd, restStart, restEnd, after int) (*Node, int, clauseParts, bool) {
		n := &Node{Kind: NodeClause, Field: field, Quoted: quoted, Form: querySpec.FormModifier, Op: SymModifier}
		parts.field = [2]int{fieldStart, fieldEnd}
		opStart := restStart - 1 // the colon
		rest := p.src.runes[restStart:restEnd]
		for _, op := range modifierOps {
			if len(rest) >= len(op) && string(rest[:len(op)]) == op {
				n.Op = op
				restStart += len(op)
				break
			}
		}
		parts.op = [2]int{opStart, restStart}
		end := restEnd
		switch {
		case restStart < restEnd:
			n.Values = []string{p.src.text(restStart, restEnd)}
			parts.values = append(parts.values, [2]int{restStart, restEnd})
		case after < len(p.toks) && p.toks[after].glued && p.toks[after].kind == tQuoted:
			q := p.toks[after]
			n.Values = []string{q.text}
			parts.phrases = append(parts.phrases, [2]int{q.start, q.end})
			if !q.closed {
				parts.issues = append(parts.issues, Issue{Code: IssueUnclosedQuote, Span: p.spanOf(q)})
			}
			end = q.end
			after++
		default:
			parts.issues = append(parts.issues, Issue{Code: IssueEmptyValue, Span: p.src.span(fieldStart, restEnd)})
		}
		n.Span = p.src.span(fieldStart, end)
		return n, after, parts, true
	}

	if t.kind == tWord {
		rs := []rune(t.text)
		if n := identLen(rs); n > 0 && n < len(rs) && rs[n] == ':' {
			return modifier(string(rs[:n]), false, t.start, t.start+n, t.start+n+1, t.end, i+1)
		}
	}
	if t.kind == tQuoted && t.closed && i+1 < len(p.toks) {
		next := p.toks[i+1]
		if next.glued && next.kind == tWord && strings.HasPrefix(next.text, ":") {
			return modifier(t.text, true, t.start, t.end, next.start+1, next.end, i+2)
		}
	}

	// The compare form: a field name or a quoted name, then an operator.
	if !((t.kind == tWord && isIdent(t.text)) || (t.kind == tQuoted && t.closed)) {
		return nil, i, parts, false
	}
	n := &Node{Kind: NodeClause, Field: t.text, Quoted: t.kind == tQuoted, Form: querySpec.FormCompare}
	parts.field = [2]int{t.start, t.end}
	j := i + 1
	if j >= len(p.toks) {
		return nil, i, parts, false
	}
	finish := func(endRune, next int) (*Node, int, clauseParts, bool) {
		n.Span = p.src.span(t.start, endRune)
		return n, next, parts, true
	}
	switch {
	case p.toks[j].kind == tOp:
		op := p.toks[j]
		n.Op = op.text
		parts.op = [2]int{op.start, op.end}
		k := j + 1
		if (n.Op == SymEq || n.Op == SymNe) && k < len(p.toks) && p.toks[k].kind == tLParen {
			n.List = true
			end, next := p.readList(k, n, &parts)
			return finish(end, next)
		}
		if k < len(p.toks) {
			switch p.toks[k].kind {
			case tQuoted:
				q := p.toks[k]
				n.Values = []string{q.text}
				parts.phrases = append(parts.phrases, [2]int{q.start, q.end})
				if !q.closed {
					parts.issues = append(parts.issues, Issue{Code: IssueUnclosedQuote, Span: p.spanOf(q)})
				}
				return finish(q.end, k+1)
			case tWord, tOp, tNeg:
				text, start, end, next := p.rawValue(k)
				n.Values = []string{text}
				parts.values = append(parts.values, [2]int{start, end})
				return finish(end, next)
			}
		}
		parts.issues = append(parts.issues, Issue{Code: IssueEmptyValue, Span: p.src.span(t.start, op.end)})
		return finish(op.end, k)
	case p.word(j, "in") && j+1 < len(p.toks) && p.toks[j+1].kind == tLParen:
		n.Op = SymIn
		n.List = true
		parts.keywords = append(parts.keywords, [2]int{p.toks[j].start, p.toks[j].end})
		end, next := p.readList(j+1, n, &parts)
		return finish(end, next)
	case p.word(j, "not") && p.word(j+1, "in") && j+2 < len(p.toks) && p.toks[j+2].kind == tLParen:
		n.Op = SymNotIn
		n.List = true
		parts.keywords = append(parts.keywords, [2]int{p.toks[j].start, p.toks[j].end}, [2]int{p.toks[j+1].start, p.toks[j+1].end})
		end, next := p.readList(j+2, n, &parts)
		return finish(end, next)
	case p.word(j, "is") && (p.word(j+1, "empty") || p.word(j+1, "null")):
		n.Op = SymEmpty
		parts.keywords = append(parts.keywords, [2]int{p.toks[j].start, p.toks[j].end}, [2]int{p.toks[j+1].start, p.toks[j+1].end})
		return finish(p.toks[j+1].end, j+2)
	case p.word(j, "is") && p.word(j+1, "not") && (p.word(j+2, "empty") || p.word(j+2, "null")):
		n.Op = SymNotEmpty
		parts.keywords = append(parts.keywords, [2]int{p.toks[j].start, p.toks[j].end}, [2]int{p.toks[j+1].start, p.toks[j+1].end}, [2]int{p.toks[j+2].start, p.toks[j+2].end})
		return finish(p.toks[j+2].end, j+3)
	}
	return nil, i, parts, false
}

// readList reads a bracketed list of values from the bracket at token k into
// n: where it ends (in runes) and the token after it.
func (p *parser) readList(k int, n *Node, parts *clauseParts) (int, int) {
	open := p.toks[k]
	parts.parens = append(parts.parens, [2]int{open.start, open.end})
	j := k + 1
	for j < len(p.toks) {
		t := p.toks[j]
		switch t.kind {
		case tRParen:
			parts.parens = append(parts.parens, [2]int{t.start, t.end})
			return t.end, j + 1
		case tComma:
			parts.commas = append(parts.commas, [2]int{t.start, t.end})
			j++
		case tQuoted:
			n.Values = append(n.Values, t.text)
			parts.phrases = append(parts.phrases, [2]int{t.start, t.end})
			if !t.closed {
				parts.issues = append(parts.issues, Issue{Code: IssueUnclosedQuote, Span: p.spanOf(t)})
			}
			j++
		case tLParen:
			// Brackets inside a list: a list holds values only.
			parts.issues = append(parts.issues, Issue{Code: IssueBadList, Span: p.spanOf(t)})
			parts.parens = append(parts.parens, [2]int{t.start, t.end})
			j++
		default:
			text, start, end, next := p.rawValue(j)
			n.Values = append(n.Values, text)
			parts.values = append(parts.values, [2]int{start, end})
			j = next
		}
	}
	parts.issues = append(parts.issues, Issue{Code: IssueUnclosedParen, Span: p.src.span(open.start, p.end())})
	return p.end(), j
}

// atOrderBy is whether ORDER BY and a field start at token i.
func (p *parser) atOrderBy(i int) bool {
	if !p.word(i, "order") || !p.word(i+1, "by") || i+2 >= len(p.toks) {
		return false
	}
	t := p.toks[i+2]
	return (t.kind == tWord && isIdent(t.text)) || (t.kind == tQuoted && t.closed)
}

func (p *parser) parseOrder() {
	p.token(TokenKeyword, p.toks[p.i].start, p.toks[p.i].end)
	p.token(TokenKeyword, p.toks[p.i+1].start, p.toks[p.i+1].end)
	p.i += 2
	for p.i < len(p.toks) {
		t := p.toks[p.i]
		if !((t.kind == tWord && isIdent(t.text)) || (t.kind == tQuoted && t.closed)) {
			break
		}
		key := SortKey{Field: t.text, Quoted: t.kind == tQuoted, Span: p.spanOf(t)}
		p.token(TokenField, t.start, t.end)
		p.i++
		if p.word(p.i, "asc") || p.word(p.i, "desc") {
			d := p.toks[p.i]
			key.Desc = asciiFold(d.text, "desc")
			key.Span[1] = p.src.u16[d.end]
			p.token(TokenKeyword, d.start, d.end)
			p.i++
		}
		if len(p.syn.Sorts) == querySpec.MaxSorts {
			p.issue(IssueTooManySorts, key.Span)
		}
		p.syn.Sorts = append(p.syn.Sorts, key)
		if p.i < len(p.toks) && p.toks[p.i].kind == tComma {
			c := p.toks[p.i]
			p.token(TokenComma, c.start, c.end)
			p.i++
			continue
		}
		break
	}
	if p.i < len(p.toks) {
		p.issue(IssueAfterOrder, p.src.span(p.toks[p.i].start, p.end()))
		p.i = len(p.toks)
	}
}
