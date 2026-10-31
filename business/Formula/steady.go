package formula

// pure is the functions whose value is their arguments' alone: never the
// moment (TODAY, NOW), the reader's zone, or a date read from text. Every
// other function, the date functions and any added later, is not, so a
// formula calling one is worked out on each read, never kept (airtable-03).
var pure = map[string]bool{
	"IF": true, "SWITCH": true, "AND": true, "OR": true, "NOT": true,
	"BLANK": true, "ISBLANK": true, "ISERROR": true, "TRUE": true, "FALSE": true,
	"SUM": true, "AVERAGE": true, "MIN": true, "MAX": true,
	"ROUND": true, "ROUNDUP": true, "ROUNDDOWN": true, "CEILING": true, "FLOOR": true,
	"ABS": true, "SQRT": true, "MOD": true, "POWER": true,
	"CONCATENATE": true, "CONCAT": true, "LEN": true, "UPPER": true, "LOWER": true, "TRIM": true,
	"LEFT": true, "RIGHT": true, "MID": true, "FIND": true, "SUBSTITUTE": true, "REPT": true,
}

// Steady is whether formula id gives the same value whoever reads it and
// whenever, from the same cells: it calls only pure functions, nothing in it
// is a date (a date compares and prints in the reader's zone), and every
// field it reads is steady too. steadyField answers for a field that isn't
// one of this Program's formulas (a rollup, or a {Link}.{Field} list, by its
// ref); a stored cell that isn't a date is steady by itself.
func (p *Program) Steady(id string, steadyField func(ref string) bool) bool {
	return p.steady(id, steadyField, map[string]bool{})
}

func (p *Program) steady(id string, steadyField func(string) bool, seen map[string]bool) bool {
	if seen[id] {
		return false // a cycle: an error, never kept
	}
	seen[id] = true
	var c *compiled
	for i := range p.order {
		if p.order[i].id == id {
			c = &p.order[i]
		}
	}
	if c == nil || c.err != nil || c.kind == KindDate || c.kind == KindError {
		return false
	}
	fieldKind := func(ref string) Kind {
		if _, _, linked := SplitLinkedRef(ref); linked {
			return KindText
		}
		return p.kinds[ref]
	}
	isFormula := func(ref string) bool {
		for i := range p.order {
			if p.order[i].id == ref {
				return true
			}
		}
		return false
	}
	var walk func(n node) bool
	walk = func(n node) bool {
		if n == nil {
			return true
		}
		if n.kind(fieldKind) == KindDate {
			return false
		}
		switch n := n.(type) {
		case *fieldRef:
			if isFormula(n.id) {
				return p.steady(n.id, steadyField, seen)
			}
			return fieldKind(n.id) != KindDate && steadyField(n.id)
		case *unary:
			return walk(n.x)
		case *binary:
			return walk(n.l) && walk(n.r)
		case *call:
			if !pure[n.name] {
				return false
			}
			for _, a := range n.args {
				if !walk(a) {
					return false
				}
			}
			return true
		case *numLit, *strLit, *boolLit:
			return true
		}
		return false // a node this walk doesn't know: not kept
	}
	return walk(c.root)
}
