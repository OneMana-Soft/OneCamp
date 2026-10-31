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
	// Tables B1's kinds (kindsMore.go).
	Kinds.Register(Kind{ID: model.FieldStatus, Stored: true, HasOptions: true, FormulaKind: formula.KindText, Coerce: coerceSelect, Convert: toSelect, Config: statusConfig})
	Kinds.Register(Kind{ID: model.FieldPeople, Stored: true, FormulaKind: formula.KindText, Coerce: coercePeople, Convert: toPeople, NoDefault: true, Config: peopleConfig})
	Kinds.Register(Kind{ID: model.FieldLongText, Stored: true, FormulaKind: formula.KindText, Coerce: coerceText, Convert: toText})
	Kinds.Register(Kind{ID: model.FieldPhone, Stored: true, FormulaKind: formula.KindText, Coerce: coercePhone, Convert: toPhone})
	Kinds.Register(Kind{ID: model.FieldDuration, Stored: true, FormulaKind: formula.KindNumber, Coerce: coerceDuration, Convert: toDuration, Config: durationConfig})
	Kinds.Register(Kind{ID: model.FieldRating, Stored: true, FormulaKind: formula.KindNumber, Coerce: coerceRating, Convert: toRating, Config: ratingConfig})
	Kinds.Register(Kind{ID: model.FieldAutonumber, FormulaKind: formula.KindNumber, FromRow: rowNumber, Config: autonumberConfig})
	Kinds.Register(Kind{ID: model.FieldCreatedTime, FormulaKind: formula.KindDate, FromRow: rowCreatedTime})
	Kinds.Register(Kind{ID: model.FieldCreatedBy, FormulaKind: formula.KindText, FromRow: rowCreatedBy})
	Kinds.Register(Kind{ID: model.FieldModifiedTime, FormulaKind: formula.KindDate, FromRow: rowModifiedTime})
	Kinds.Register(Kind{ID: model.FieldModifiedBy, FormulaKind: formula.KindText, FromRow: rowModifiedBy})
	// Tables B1's files (kindsFiles.go).
	Kinds.Register(Kind{ID: model.FieldAttachment, Stored: true, FormulaKind: formula.KindText, Coerce: coerceFiles, NoDefault: true})
}
