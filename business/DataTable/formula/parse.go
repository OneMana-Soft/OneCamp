package formula

import (
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// MaxLength is the longest formula, in characters.
const MaxLength = 2000

// maxDepth bounds nesting, so a formula can't exhaust the stack.
const maxDepth = 64

// env is one row being worked out.
type env struct {
	// cell is a field's value in the row, formulas included.
	cell func(id string) Value
	now  time.Time
	loc  *time.Location
	// steps is the work done for the row, and read the work done for every
	// row of the read so far (Program.read).
	steps int
	read  *int
	// spent is why the work stopped partway, once it has: the formula's
	// value, whatever reads it, so ISERROR can't turn it into an answer.
	spent Value
}

// Work is counted in steps of about 50 ns: one for each part of a formula
// worked out, one for each bytesPerStep of text a part gives or a function
// counts its way through, and one for each slowBytesPerStep of text a
// function changes or compares a character at a time (UPPER, LOWER,
// SUBSTITUTE, comparing text ignoring case). A row's formulas have maxSteps
// between them, and every row of one read of a table (a page) has
// maxReadSteps between them, so neither a row nor a page can hold the server
// for a second (the worst page measured took 0.75 s), while a page of 4 KB
// notes put together ten ways uses a fifth of a read.
const (
	maxSteps         = 1000000
	maxReadSteps     = 10000000
	bytesPerStep     = 32
	slowBytesPerStep = 4
	// compareBytesPerStep is for the bytes a search compares (search).
	compareBytesPerStep = 1024
)

var (
	tooMuch     = Error("This formula takes too much working out")
	tooMuchRead = Error("This table's formulas take too much working out")
)

// run works out a node, within the row's and the read's budgets.
func (e *env) run(n node) Value {
	switch {
	case e.spent.Kind == KindError:
		return e.spent
	case e.steps >= maxSteps:
		e.spent = tooMuch
		return tooMuch
	case *e.read >= maxReadSteps:
		e.spent = tooMuchRead
		return tooMuchRead
	}
	v := n.eval(e)
	e.spend(1 + len(v.Str)/bytesPerStep)
	return v
}

func (e *env) spend(steps int) {
	e.steps += steps
	*e.read += steps
}

// scan charges for n bytes of text counted through; slow, for n bytes changed
// or compared a character at a time.
func (e *env) scan(n int) { e.spend(n / bytesPerStep) }
func (e *env) slow(n int) { e.spend(n / slowBytesPerStep) }

// search charges for looking through hay bytes for a needle bytes long, at
// its worst: strings.Index compares a needle longer than 63 bytes whole at as
// many as one place in 16 when the text repeats.
func (e *env) search(hay, needle int) {
	if needle > 63 && needle <= hay {
		e.spend(((hay-needle)/16 + 4) * needle / compareBytesPerStep)
	}
}

// trimmed is s without the spaces around it, charged as slow work: past the
// first byte that isn't ASCII, trimming goes a character at a time.
func (e *env) trimmed(s string) string {
	t := strings.TrimSpace(s)
	e.slow(len(s) - len(t))
	return t
}

// truthy is how IF, AND, OR and NOT read a value.
func (e *env) truthy(v Value) bool {
	switch v.Kind {
	case KindNumber:
		return v.Num != 0
	case KindText:
		return e.trimmed(v.Str) != ""
	case KindBool:
		return v.Bool
	case KindDate:
		return true
	default:
		return false
	}
}

// local is a moment as the reader's clock shows it, so its day is the
// reader's day when it's compared, printed or taken apart. A whole day is a
// calendar date, the same everywhere, and stays as it is.
func (e *env) local(v Value) Value {
	if v.Kind == KindDate && !v.Day {
		v.Time = v.Time.In(e.loc)
	}
	return v
}

type node interface {
	eval(e *env) Value
	// kind is what the node gives, from the kinds of the fields it reads.
	kind(fieldKind func(id string) Kind) Kind
}

type numLit struct{ v float64 }
type strLit struct{ s string }
type boolLit struct{ b bool }
type fieldRef struct{ id string }
type unary struct {
	op string
	x  node
}
type binary struct {
	op   string
	l, r node
}
type call struct {
	name string
	fn   *function
	args []node
}

func (n *numLit) eval(*env) Value             { return Number(n.v) }
func (n *numLit) kind(func(string) Kind) Kind { return KindNumber }
func (n *strLit) eval(*env) Value             { return Text(n.s) }
func (n *strLit) kind(func(string) Kind) Kind {
	if n.s == "" {
		return KindBlank // IF({x}, 5, "") gives a number
	}
	return KindText
}
func (n *boolLit) eval(*env) Value                { return Bool(n.b) }
func (n *boolLit) kind(func(string) Kind) Kind    { return KindBool }
func (n *fieldRef) eval(e *env) Value             { return e.local(e.cell(n.id)) }
func (n *fieldRef) kind(k func(string) Kind) Kind { return k(n.id) }

func (n *unary) eval(e *env) Value {
	v := e.run(n.x)
	if v.Kind == KindError {
		return v
	}
	f, ok := v.number()
	if !ok {
		return Error(fmt.Sprintf("%q isn't a number", clip(v.text())))
	}
	if n.op == "-" {
		f = -f
	}
	return Number(f)
}

func (n *unary) kind(func(string) Kind) Kind { return KindNumber }

func (n *binary) eval(e *env) Value {
	l := e.run(n.l)
	if l.Kind == KindError {
		return l
	}
	r := e.run(n.r)
	if r.Kind == KindError {
		return r
	}
	switch n.op {
	case "&":
		ls, rs := l.text(), r.text()
		if len(ls)+len(rs) > 4*maxText {
			return tooLong()
		}
		return textResult(ls+rs, maxText)
	case "+", "-", "*", "/":
		return arith(n.op, l, r)
	}
	c := compare(e, l, r)
	switch n.op {
	case "=":
		return Bool(c == 0)
	case "!=":
		return Bool(c != 0)
	case "<":
		return Bool(c < 0)
	case ">":
		return Bool(c > 0)
	case "<=":
		return Bool(c <= 0)
	default: // ">="
		return Bool(c >= 0)
	}
}

func (n *binary) kind(k func(string) Kind) Kind {
	switch n.op {
	case "&":
		return KindText
	case "+", "-":
		lk, rk := n.l.kind(k), n.r.kind(k)
		switch {
		case lk == KindDate && rk == KindDate:
			return KindNumber
		case lk == KindDate || rk == KindDate:
			return KindDate
		}
		return KindNumber
	case "*", "/":
		return KindNumber
	default:
		return KindBool
	}
}

func (n *call) eval(e *env) Value {
	if n.fn.lazy != nil {
		return n.fn.lazy(e, n.args)
	}
	vals := make([]Value, len(n.args))
	for i, a := range n.args {
		v := e.run(a)
		if v.Kind == KindError && !n.fn.takesErrors {
			return v
		}
		vals[i] = v
	}
	return n.fn.eager(e, vals)
}

func (n *call) kind(k func(string) Kind) Kind {
	if n.fn.branches == nil {
		return n.fn.result
	}
	// IF and SWITCH give what their branches give, when they agree.
	out := KindBlank
	for _, b := range n.fn.branches(n.args) {
		bk := b.kind(k)
		switch {
		case bk == KindBlank:
		case out == KindBlank:
			out = bk
		case out != bk:
			return KindText
		}
	}
	return out
}

// arith is + - * /. A date plus or minus a number moves it by that many days,
// and one date minus another is the days between them.
func arith(op string, l, r Value) Value {
	if l.Kind == KindDate || r.Kind == KindDate {
		switch {
		case l.Kind == KindDate && r.Kind == KindDate && op == "-":
			return Number(daysBetween(r, l))
		case l.Kind == KindDate && (op == "+" || op == "-"):
			n, ok := r.number()
			if !ok {
				return Error(fmt.Sprintf("%q isn't a number of days", clip(r.text())))
			}
			if op == "-" {
				n = -n
			}
			return addTo(l, n, "day")
		case r.Kind == KindDate && op == "+":
			n, ok := l.number()
			if !ok {
				return Error(fmt.Sprintf("%q isn't a number of days", clip(l.text())))
			}
			return addTo(r, n, "day")
		}
		return Error("A date can only have days added to it or taken from it")
	}
	a, ok := l.number()
	if !ok {
		return Error(fmt.Sprintf("%q isn't a number", clip(l.text())))
	}
	b, ok := r.number()
	if !ok {
		return Error(fmt.Sprintf("%q isn't a number", clip(r.text())))
	}
	switch op {
	case "+":
		return Number(a + b)
	case "-":
		return Number(a - b)
	case "*":
		return Number(a * b)
	default:
		if b == 0 {
			return Error("Divided by zero")
		}
		return Number(a / b)
	}
}

// compare orders two values as a spreadsheet would: numbers by value (a blank
// is 0), dates in time (a blank before any), and anything else as text,
// ignoring case. A moment's day is the reader's (env.local).
func compare(e *env, l, r Value) int {
	if l.Kind == KindDate || r.Kind == KindDate {
		ld, lok := asDate(l)
		rd, rok := asDate(r)
		switch {
		case lok && rok:
			return compareTimes(e.local(ld), e.local(rd))
		case l.Kind == KindBlank:
			return -1
		case r.Kind == KindBlank:
			return 1
		}
	} else if isNumeric(l) || isNumeric(r) {
		a, aok := l.number()
		b, bok := r.number()
		if aok && bok {
			switch {
			case tidy(a) < tidy(b):
				return -1
			case tidy(a) > tidy(b):
				return 1
			}
			return 0
		}
	}
	c, walked := compareFold(l.text(), r.text())
	e.slow(walked)
	return c
}

// compareFold orders two texts as comparing them lowercased would, without
// copying either (a cell can be long), and says how many bytes it went
// through to tell.
func compareFold(a, b string) (int, int) {
	walked := 0
	for a != "" && b != "" {
		ra, wa := rune(a[0]), 1
		if ra >= utf8.RuneSelf {
			ra, wa = utf8.DecodeRuneInString(a)
		}
		rb, wb := rune(b[0]), 1
		if rb >= utf8.RuneSelf {
			rb, wb = utf8.DecodeRuneInString(b)
		}
		walked += wa + wb
		if ra != rb {
			if la, lb := unicode.ToLower(ra), unicode.ToLower(rb); la != lb {
				if la < lb {
					return -1, walked
				}
				return 1, walked
			}
		}
		a, b = a[wa:], b[wb:]
	}
	switch {
	case a == b:
		return 0, walked
	case a == "":
		return -1, walked
	}
	return 1, walked
}

func isNumeric(v Value) bool { return v.Kind == KindNumber || v.Kind == KindBool }

// asDate reads a value as a date: a date, or text written as one.
func asDate(v Value) (Value, bool) {
	switch v.Kind {
	case KindDate:
		return v, true
	case KindText:
		return ParseDate(v.Str)
	}
	return Value{}, false
}

func compareTimes(a, b Value) int {
	at, bt := a.Time, b.Time
	if a.Day != b.Day {
		// A day against a moment: compare the days.
		at = time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
		bt = time.Date(bt.Year(), bt.Month(), bt.Day(), 0, 0, 0, 0, time.UTC)
	}
	switch {
	case at.Before(bt):
		return -1
	case at.After(bt):
		return 1
	}
	return 0
}

// daysBetween is to minus from, in days: whole for two days, with a fraction
// for moments. Counted in seconds since 1970, which spans every year a date
// can have (a time.Duration overflows past 292 years).
func daysBetween(from, to Value) float64 {
	d := secondsBetween(from.Time, to.Time) / 86400
	if from.Day && to.Day {
		return math.Round(d)
	}
	return d
}

func secondsBetween(from, to time.Time) float64 {
	return float64(to.Unix()-from.Unix()) + float64(to.Nanosecond()-from.Nanosecond())/1e9
}

// parser reads tokens into a tree. field resolves a {reference} to a field's
// id; refs collects the ids read.
type parser struct {
	toks  []token
	i     int
	depth int
	field func(ref string) (string, error)
	refs  map[string]bool
}

func (p *parser) peek() token { return p.toks[p.i] }

func (p *parser) next() token {
	t := p.toks[p.i]
	if t.kind != tEOF {
		p.i++
	}
	return t
}

// parse reads a whole formula.
func parse(src string, field func(ref string) (string, error)) (node, map[string]bool, error) {
	if strings.TrimSpace(src) == "" {
		return nil, nil, &SyntaxError{Msg: "Write a formula"}
	}
	if utf8.RuneCountInString(src) > MaxLength {
		return nil, nil, &SyntaxError{Msg: fmt.Sprintf("A formula can be at most %d characters", MaxLength)}
	}
	toks, err := lex(src)
	if err != nil {
		return nil, nil, err
	}
	p := &parser{toks: toks, field: field, refs: map[string]bool{}}
	n, err := p.comparison()
	if err != nil {
		return nil, nil, err
	}
	if t := p.peek(); t.kind != tEOF {
		return nil, nil, syntaxErr(t.pos, "%q doesn't belong here", clip(t.text))
	}
	return n, p.refs, nil
}

func (p *parser) enter(pos int) error {
	p.depth++
	if p.depth > maxDepth {
		return syntaxErr(pos, "This formula is nested too deeply")
	}
	return nil
}

func (p *parser) leave() { p.depth-- }

func (p *parser) comparison() (node, error) {
	l, err := p.concat()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.kind != tOp {
			return l, nil
		}
		op := t.text
		switch op {
		case "<>":
			op = "!="
		case "=", "!=", "<", ">", "<=", ">=":
		default:
			return l, nil
		}
		p.next()
		r, err := p.concat()
		if err != nil {
			return nil, err
		}
		l = &binary{op: op, l: l, r: r}
	}
}

// binaryLevel reads one level of left-associative operators.
func (p *parser) binaryLevel(ops string, below func() (node, error)) (node, error) {
	l, err := below()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.kind != tOp || len(t.text) != 1 || !strings.Contains(ops, t.text) {
			return l, nil
		}
		p.next()
		r, err := below()
		if err != nil {
			return nil, err
		}
		l = &binary{op: t.text, l: l, r: r}
	}
}

func (p *parser) concat() (node, error) { return p.binaryLevel("&", p.additive) }

func (p *parser) additive() (node, error) { return p.binaryLevel("+-", p.term) }

func (p *parser) term() (node, error) { return p.binaryLevel("*/", p.unary) }

func (p *parser) unary() (node, error) {
	t := p.peek()
	if t.kind == tOp && (t.text == "-" || t.text == "+") {
		if err := p.enter(t.pos); err != nil {
			return nil, err
		}
		defer p.leave()
		p.next()
		x, err := p.unary()
		if err != nil {
			return nil, err
		}
		return &unary{op: t.text, x: x}, nil
	}
	return p.primary()
}

func (p *parser) primary() (node, error) {
	t := p.next()
	switch t.kind {
	case tNum:
		return &numLit{v: t.num}, nil
	case tStr:
		return &strLit{s: t.text}, nil
	case tField:
		id, err := p.field(t.text)
		if err != nil {
			return nil, syntaxErr(t.pos, "%s", err.Error())
		}
		p.refs[id] = true
		return &fieldRef{id: id}, nil
	case tLParen:
		if err := p.enter(t.pos); err != nil {
			return nil, err
		}
		defer p.leave()
		n, err := p.comparison()
		if err != nil {
			return nil, err
		}
		if c := p.next(); c.kind != tRParen {
			return nil, syntaxErr(c.pos, "A ( has no closing )")
		}
		return n, nil
	case tIdent:
		return p.ident(t)
	case tEOF:
		return nil, syntaxErr(t.pos, "The formula ends too soon")
	}
	return nil, syntaxErr(t.pos, "%q doesn't belong here", clip(t.text))
}

func (p *parser) ident(t token) (node, error) {
	name := strings.ToUpper(t.text)
	if p.peek().kind != tLParen {
		switch name {
		case "TRUE":
			return &boolLit{b: true}, nil
		case "FALSE":
			return &boolLit{b: false}, nil
		}
		return nil, syntaxErr(t.pos, "%s isn't a function; a field's name goes in braces, like {%s}", clip(t.text), clip(t.text))
	}
	fn, ok := functions[name]
	if !ok {
		return nil, syntaxErr(t.pos, "There's no function called %s", clip(t.text))
	}
	if err := p.enter(t.pos); err != nil {
		return nil, err
	}
	defer p.leave()
	p.next() // (
	var args []node
	if p.peek().kind != tRParen {
		for {
			a, err := p.comparison()
			if err != nil {
				return nil, err
			}
			args = append(args, a)
			if p.peek().kind != tComma {
				break
			}
			p.next()
		}
	}
	if c := p.next(); c.kind != tRParen {
		return nil, syntaxErr(c.pos, "%s( has no closing )", name)
	}
	if len(args) < fn.min || (fn.max >= 0 && len(args) > fn.max) {
		return nil, syntaxErr(t.pos, "%s takes %s", name, fn.arity())
	}
	return &call{name: name, fn: fn, args: args}, nil
}
