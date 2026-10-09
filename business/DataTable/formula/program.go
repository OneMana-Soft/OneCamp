package formula

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/akashc777/OneCamp/helpers/topo"
)

// Field is one of a table's fields as formulas see it.
type Field struct {
	ID   string
	Name string
	// Kind is how its cells read in a formula. Formula fields work theirs out.
	Kind Kind
	// IsFormula marks a formula field; Formula is its expression as stored,
	// with fields named by id (Canonical).
	IsFormula bool
	Formula   string
}

// idRef is how a stored formula names a field: {#<id>}, which survives the
// field being renamed. A field's own name can't start that way.
const idRef = "#"

// ErrCycle is a formula that reads its own value, directly or through
// another formula.
var ErrCycle = errors.New("This formula reads its own value, through another formula")

type compiled struct {
	id   string
	root node
	kind Kind
	err  error
}

// Program is a table's formulas, ready to run on the rows of one read of it
// (a page): each formula read once, in an order where it comes after the
// formulas it reads. The rows share the read's Budget, so a Program is for
// one read, from one goroutine.
type Program struct {
	order  []compiled
	kinds  map[string]Kind
	budget *Budget
}

// Budget is the work one read can take (maxReadSteps): its own table's
// formulas, those of the tables it links to, and the rollups adding them up
// all draw on the same one.
type Budget struct{ spent int }

// NewBudget is a read's budget, none of it spent.
func NewBudget() *Budget { return &Budget{} }

// Spend charges n steps, and says whether the budget had them.
func (b *Budget) Spend(n int) bool {
	b.spent += n
	return b.spent <= maxReadSteps
}

// Within has the program's rows draw on budget b.
func (p *Program) Within(b *Budget) *Program {
	p.budget = b
	return p
}

// RanOut is the value of anything a read's budget ran out before working out.
func RanOut() Value { return tooMuchRead }

// resolver finds a field by its id ({#id}) or name ({Name}, ignoring case).
func resolver(fields []Field) func(ref string) (string, error) {
	byID := map[string]bool{}
	byName := map[string]string{}
	for _, f := range fields {
		byID[f.ID] = true
		name := strings.ToLower(strings.TrimSpace(f.Name))
		if _, taken := byName[name]; !taken {
			byName[name] = f.ID
		}
	}
	return func(ref string) (string, error) {
		if id := strings.TrimPrefix(ref, idRef); id != ref && byID[id] {
			return id, nil
		}
		// By name, a name that starts with # included ({# of seats}).
		if id, ok := byName[strings.ToLower(ref)]; ok {
			return id, nil
		}
		if strings.HasPrefix(ref, idRef) {
			return "", errors.New("A field this formula reads has been deleted")
		}
		return "", fmt.Errorf("There's no field called %q", clip(ref))
	}
}

// Compile reads a table's formula fields. One that can't be read keeps its
// error, shown in every row, and the rest still work.
func Compile(fields []Field) *Program {
	resolve := resolver(fields)
	kinds := map[string]Kind{}
	formulas := map[string]*compiled{}
	deps := map[string][]string{}
	isFormula := map[string]bool{}
	for _, f := range fields {
		isFormula[f.ID] = f.IsFormula
	}
	var ids []string
	for _, f := range fields {
		kinds[f.ID] = f.Kind
		if !f.IsFormula {
			continue
		}
		c := &compiled{id: f.ID}
		formulas[f.ID] = c
		ids = append(ids, f.ID)
		if strings.TrimSpace(f.Formula) == "" {
			continue // blank until it's written
		}
		root, refs, err := parse(f.Formula, resolve)
		if err != nil {
			c.err = err
			continue
		}
		if refs[f.ID] {
			c.err = errors.New("A formula can't read its own value")
			continue
		}
		c.root = root
		for id := range refs {
			if isFormula[id] {
				deps[f.ID] = append(deps[f.ID], id)
			}
		}
	}

	// Each formula after those it reads; what can't be placed reads itself
	// through a loop.
	ordered, looped := topo.Order(ids, func(id string) []string { return deps[id] })
	p := &Program{kinds: kinds, budget: NewBudget()}
	for _, id := range ordered {
		c := formulas[id]
		switch {
		case c.err != nil:
			c.kind = KindText
		case c.root == nil:
			c.kind = KindBlank
		default:
			c.kind = c.root.kind(func(ref string) Kind { return kinds[ref] })
		}
		kinds[id] = c.kind
		p.order = append(p.order, *c)
	}
	for _, id := range looped {
		c := formulas[id]
		c.err, c.root, c.kind = ErrCycle, nil, KindText
		kinds[id] = KindText
		p.order = append(p.order, *c)
	}
	return p
}

// Kind is what a formula field gives (number, text, checkbox, date), for its
// column: how it's aligned, sorted and filtered.
func (p *Program) Kind(id string) Kind { return p.kinds[id] }

// Err is why a formula field can't be worked out, or nil.
func (p *Program) Err(id string) error {
	for _, c := range p.order {
		if c.id == id {
			return c.err
		}
	}
	return nil
}

// Empty is true when the table has no formula fields.
func (p *Program) Empty() bool { return len(p.order) == 0 }

// maxRowText is how much text a row's formulas can give between them, in
// bytes as sent (sentLen): as much as a row can hold, so formulas can't make
// a row many times its size.
const maxRowText = 100 << 10

var tooMuchText = Error("This row's formulas give more text than a row can hold")

// shortWhys is the reasons a value is missing rather than wrong, by text:
// the work or the text a formula was allowed ran out, or what a Short value
// says.
var shortWhys sync.Map

func init() {
	for _, v := range []Value{tooMuch, tooMuchRead, tooMuchText} {
		shortWhys.Store(v.Str, true)
	}
}

// Short is an error value saying why a read left something out: no answer,
// rather than a wrong one, so a total reading it falls short (Unfinished).
func Short(why string) Value {
	shortWhys.Store(why, true)
	return Error(why)
}

func isShort(why string) bool {
	_, ok := shortWhys.Load(why)
	return ok
}

// IsUnfinished is whether a value is one a formula gave because the work or
// the text it was allowed ran out, or a Short one, rather than an answer.
func (v Value) IsUnfinished() bool {
	return v.Kind == KindError && isShort(v.Str)
}

// Unfinished is true for a cell, as sent (Value.JSON), that wasn't worked
// out because the work or the text it was allowed ran out, or that's a Short
// value: a total that reads it falls short.
func Unfinished(cell interface{}) bool {
	m, ok := cell.(map[string]interface{})
	if !ok {
		return false
	}
	why, _ := m["error"].(string)
	return why != "" && isShort(why)
}

// ReadCost is the steps reading a stored cell takes, from its JSON, for Run's
// cell to say: text at the slow rate, as it's decoded and checked for being
// only spaces, and a list or an object at a step a byte, as each item in it
// is decoded and labelled.
func ReadCost(raw string) int {
	if raw != "" && (raw[0] == '[' || raw[0] == '{') {
		return len(raw)
	}
	return len(raw) / slowBytesPerStep
}

// Run works out every formula for one row. cell reads a field that isn't a
// formula, once a row however many times formulas name it, and says how many
// steps that took (ReadCost); now is the moment TODAY() and NOW() read, and
// loc the reader's time zone, where today is.
func (p *Program) Run(cell func(id string) (Value, int), now time.Time, loc *time.Location) map[string]Value {
	out := make(map[string]Value, len(p.order))
	read := map[string]Value{}
	e := &env{now: now, loc: loc, read: &p.budget.spent}
	e.cell = func(id string) Value {
		if v, ok := out[id]; ok {
			return v
		}
		v, ok := read[id]
		if !ok {
			var steps int
			v, steps = cell(id)
			e.spend(steps)
			read[id] = v
		}
		return v
	}
	text := 0
	for _, c := range p.order {
		var v Value
		switch {
		case c.err != nil:
			v = Error(Message(c.err))
		case c.root == nil:
			v = Blank()
		default:
			if v = e.run(c.root); e.spent.Kind == KindError {
				v = e.spent
			}
		}
		if v.Str != "" {
			if n := sentLen(v.Str); n > maxRowText-text {
				v = tooMuchText
			} else {
				text += n
			}
		}
		out[c.id] = v
	}
	return out
}

// Refs is the fields a formula reads, as it writes them ("Price" for
// {Price}), in the order it reads them; none when it can't be read.
func Refs(src string) []string {
	if utf8.RuneCountInString(src) > MaxLength {
		return nil
	}
	toks, err := lex(src)
	if err != nil {
		return nil
	}
	var out []string
	for _, t := range toks {
		if t.kind == tField {
			out = append(out, t.text)
		}
	}
	return out
}

// Check reads a formula about to be saved for field self, without running
// it: what it gives, or why it can't be read (a *SyntaxError says where).
func Check(src string, fields []Field, self string) (Kind, error) {
	draft := make([]Field, 0, len(fields)+1)
	found := false
	for _, f := range fields {
		if f.ID == self {
			f.IsFormula, f.Formula, found = true, src, true
		}
		draft = append(draft, f)
	}
	if !found {
		draft = append(draft, Field{ID: self, IsFormula: true, Formula: src})
	}
	if strings.TrimSpace(src) == "" {
		return KindBlank, &SyntaxError{Msg: "Write a formula"}
	}
	p := Compile(draft)
	for _, c := range p.order {
		if c.id == self {
			return c.kind, c.err
		}
	}
	return KindBlank, nil
}

// Canonical is a formula as it's stored: each {Field name} written as the
// field's id, so renaming the field doesn't break it. Everything else is kept
// as written.
func Canonical(src string, fields []Field) (string, error) {
	resolve := resolver(fields)
	return rewrite(src, func(ref string) (string, error) {
		id, err := resolve(ref)
		if err != nil {
			return "", err
		}
		return idRef + id, nil
	})
}

// DeletedField is how a formula shows a field that's been deleted since.
const DeletedField = "Deleted field"

// Display is a stored formula as people read and edit it: each field by its
// name, and one deleted since as {Deleted field}, so it can be put right.
func Display(src string, fields []Field) string {
	names := map[string]string{}
	for _, f := range fields {
		names[f.ID] = f.Name
	}
	out, err := rewrite(src, func(ref string) (string, error) {
		if strings.HasPrefix(ref, idRef) {
			if name, ok := names[strings.TrimPrefix(ref, idRef)]; ok {
				return name, nil
			}
			return DeletedField, nil
		}
		return ref, nil
	})
	if err != nil {
		return src
	}
	return out
}

// rewrite replaces each {field} in a formula with what to gives, escaping
// braces and backslashes in it, and keeps every other character as it was.
func rewrite(src string, to func(ref string) (string, error)) (string, error) {
	toks, err := lex(src)
	if err != nil {
		return "", err
	}
	rs := []rune(src)
	var b strings.Builder
	at := 0
	for _, t := range toks {
		if t.kind != tField {
			continue
		}
		ref, err := to(t.text)
		if err != nil {
			return "", &SyntaxError{Pos: t.pos + 1, Msg: err.Error()}
		}
		b.WriteString(string(rs[at:t.pos]))
		b.WriteString("{" + escapeRef(ref) + "}")
		at = t.end
	}
	b.WriteString(string(rs[at:]))
	return b.String(), nil
}

func escapeRef(s string) string {
	return strings.NewReplacer(`\`, `\\`, `}`, `\}`).Replace(s)
}
