package business

import (
	formula "github.com/akashc777/OneCamp/business/Formula"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

// One Register per kind, in the order the column menu offers them. A package
// adding a kind adds one line here (append-only: PROGRAM.md §3.6).
func init() {
	Kinds.Register(Kind{ID: model.FieldText, Stored: true, FormulaKind: formula.KindText, Coerce: coerceText, Convert: toText})
	Kinds.Register(Kind{ID: model.FieldNumber, Stored: true, FormulaKind: formula.KindNumber, Coerce: coerceNumber, Convert: toNumber})
	Kinds.Register(Kind{ID: model.FieldSelect, Stored: true, HasOptions: true, FormulaKind: formula.KindText, Coerce: coerceSelect, Convert: toSelect})
	Kinds.Register(Kind{ID: model.FieldMultiSelect, Stored: true, HasOptions: true, FormulaKind: formula.KindText, Coerce: coerceMultiSelect, Convert: toMultiSelect})
	Kinds.Register(Kind{ID: model.FieldDate, Stored: true, FormulaKind: formula.KindDate, Coerce: coerceDate, Convert: toDate})
	Kinds.Register(Kind{ID: model.FieldCheckbox, Stored: true, FormulaKind: formula.KindBool, Coerce: coerceCheckbox, Convert: toCheckbox})
	Kinds.Register(Kind{ID: model.FieldPerson, Stored: true, FormulaKind: formula.KindText, Coerce: coerceText, Convert: toText})
	Kinds.Register(Kind{ID: model.FieldURL, Stored: true, FormulaKind: formula.KindText, Coerce: coerceText, Convert: toText})
	Kinds.Register(Kind{ID: model.FieldEmail, Stored: true, FormulaKind: formula.KindText, Coerce: coerceText, Convert: toText})
	// A relation's cells are stored when it links to OneCamp items ({id,
	// type}), and are links in data_table_links when it links to a table's
	// rows, which business/DataTable takes out of a write before it's checked
	// (takeLinks, stripComputed). It converts from nothing, and to nothing.
	Kinds.Register(Kind{ID: model.FieldRelation, Stored: true, FormulaKind: formula.KindText, Coerce: coerceRelation, NoDefault: true})
	Kinds.Register(Kind{ID: model.FieldFormula, FormulaKind: formula.KindText})
	Kinds.Register(Kind{ID: model.FieldRollup, FormulaKind: formula.KindText})
}
