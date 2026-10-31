package business

// Importing a base (tables T-AT): the one way another tool's tables become
// OneCamp's, whatever the tool. A source (Airtable's provider, Notion's
// databases) says what it has in OneCamp's own terms: tables, fields of
// OneCamp's kinds, rows whose cells are as those kinds store them, the links
// between rows by the source's own keys, and views. ImportBase makes it
// through the same writes as everyone's:
//
//   - each table with its plain fields (CreateTableFromTemplate);
//   - the links between tables, each pair of a source's two sides one
//     relation that shows from both (two_way), so a link is made once;
//   - rollups, then formulas, which can read them;
//   - the rows, a batch at a time, through the Row Ledger as an import's
//     change sets (Batch, kind "import"), so they can be seen and undone;
//     a value outside a select's options becomes one of them, as the
//     source's own import makes it (addOptions), and a cell no field kind
//     accepts is left out and said, never the row;
//   - then each row's links, once every table's rows are there (a batch's
//     patch sets them, setLinks);
//   - the views.
//
// What it can't bring is said in its notes, by table, field and row.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	fieldTypes "github.com/akashc777/OneCamp/business/FieldTypes"
	rowLedger "github.com/akashc777/OneCamp/business/RowLedger"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// ImportBase is what a source brings: its tables, made private to the
// person importing when Private is set (as a private page's databases).
type ImportBase struct {
	Private bool
	Tables  []ImportTable
	// Notes are what the source already knows it brings as less than it was
	// (a kind OneCamp doesn't have yet); the import's own follow them.
	Notes []ImportNote
	// Files keeps a file a files cell brings, as a file of the table, and
	// gives it as the cell holds it; false when it wasn't kept (it is then
	// left out of its cell, and said). nil leaves every file out.
	Files func(table uuid.UUID, f ImportFile) (fieldTypes.FileRef, bool)
}

// ImportFile is a file in a files cell, as a source brings it: its name,
// type and size, and From, what the source fetches it by.
type ImportFile struct {
	Name, Mime string
	Size       int64
	From       any
}

// ImportTable is one table a source brings.
type ImportTable struct {
	Key, Name, Description, Icon string
	// Fields in their order; the first is the table's name field.
	Fields []ImportField
	// Rows hands the table's rows a page at a time, in the source's order,
	// until it returns (nil when it has no rows).
	Rows  func(page func([]ImportRow) error) error
	Views []ImportView
	// Into is a table the person has whose rows these come into, rather than
	// a table made for them (uuid.Nil): each field's cells go into the field
	// of Into its Into names, and nothing else of the table changes. What an
	// import that fails wrote there is undone.
	Into uuid.UUID
	// MatchBy is a field, by Key, whose value names the row of Into a row
	// changes: the first in the table's order holding it, or the row an
	// earlier one made. A row naming none is made, and a row without a value
	// there too. "" makes every row.
	MatchBy string
}

// ImportField is one field: of a OneCamp kind (model.Field*), with that
// kind's config.
type ImportField struct {
	Key, Name, Type, Description string
	Config                       map[string]any
	// LinksTo is a relation's table, by its Key; Pair the field of that table
	// showing the same links, by its Key ("" when the source has one side).
	LinksTo, Pair string
	// A rollup reads Of, a field of the linked table (by Key), through
	// Through, this table's relation (by Key), as Aggregate (rollupConfig).
	Through, Of, Aggregate string
	// Formula is a formula's text, its fields named {Name}.
	Formula string
	// Fallback is the stored kind a rollup or a formula is made as when it
	// can't be made as one (its source's cells are then kept as values);
	// "" leaves it out.
	Fallback string
	// Into is the field of the table's Into its cells go into.
	Into uuid.UUID
}

// ImportRow is one row: its cells by field Key, as the field's kind stores
// them, and its links by relation field Key, to rows by their Key.
type ImportRow struct {
	Key   string
	Cells map[string]any
	Links map[string][]string
}

// PersonRef is a person in a people cell a source brings, by email (an
// Airtable collaborator, a Google Sheet's chip): the tables stage of an
// import matches it to a member of the workspace before the row is written,
// and leaves out one who isn't a member.
type PersonRef struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
}

// ImportView is one view: of a OneCamp view type (grid, board, calendar,
// chart), with that type's config.
type ImportView struct {
	Name, Type string
	Config     map[string]any
}

// ImportNote is something an import couldn't bring, and why.
type ImportNote struct {
	Table, Field, Row string
	Code, Msg         string
}

// ImportedBase is what an import made: OneCamp's ids by the source's keys
// (a field's and a row's as "<table key>/<key>"), how many rows it made and
// changed (in a table it came into), and notes.
type ImportedBase struct {
	Tables      map[string]uuid.UUID
	Fields      map[string]uuid.UUID
	Rows        map[string]uuid.UUID
	RowsMade    int
	RowsChanged int
	Notes       []ImportNote
}

// importer is one ImportBase in progress.
type importer struct {
	ctx    context.Context
	actor  Actor
	base   ImportBase
	out    *ImportedBase
	tables map[string]*ImportTable
	// links holds every row's links until all the rows are there.
	links map[string][]ImportRow
	// stored is the fields whose cells a row writes, by "<table>/<field>".
	stored map[string]bool
	// op is the import's operation: every change set it writes is of it, so
	// one undo takes all it wrote to a table.
	op uuid.UUID
	// wrote is a change set the import wrote to a table it came into, by the
	// table's Key: the one an undo of what it wrote there names.
	wrote map[string]uuid.UUID
	// gave is the options addOptions gave the fields of a table it came
	// into, by the table's Key and the field: their ids, which takeBack takes
	// out again.
	gave map[string]map[uuid.UUID][]string
	// filled is how many rows it wrote to each table, made or changed, by the
	// table's Key (indexSoon counts a big fill for the planner).
	filled map[string]int
}

// newImporter is an import of base for actor with every map it writes to
// made: ImportBaseAs's, and any test's that runs one of its steps, so a step
// never meets a map the import itself would have made.
func newImporter(ctx context.Context, actor Actor, base ImportBase) *importer {
	im := &importer{ctx: ctx, actor: actor, base: base,
		out:    &ImportedBase{Tables: map[string]uuid.UUID{}, Fields: map[string]uuid.UUID{}, Rows: map[string]uuid.UUID{}, Notes: append([]ImportNote(nil), base.Notes...)},
		tables: map[string]*ImportTable{}, links: map[string][]ImportRow{}, stored: map[string]bool{},
		op: uuid.New(), wrote: map[string]uuid.UUID{}, gave: map[string]map[uuid.UUID][]string{}, filled: map[string]int{}}
	for i := range base.Tables {
		im.tables[base.Tables[i].Key] = &base.Tables[i]
	}
	return im
}

func (im *importer) note(table, field, row, code, msg string) {
	im.out.Notes = append(im.out.Notes, ImportNote{Table: table, Field: field, Row: row, Code: code, Msg: msg})
}

func (im *importer) field(table, key string) (uuid.UUID, bool) {
	id, ok := im.out.Fields[table+"/"+key]
	return id, ok
}

// ImportBaseAs makes base for actor (the person importing, as an import's
// actor kind), as one operation. On an error the tables it made are taken
// away again, and what it wrote to a table it came into is undone, so a retry
// starts clean; what it made is in the result either way.
func ImportBaseAs(ctx context.Context, actor Actor, base ImportBase) (*ImportedBase, error) {
	actor.Kind = rowLedger.ActorImport
	im := newImporter(ctx, actor, base)
	err := im.run()
	if err != nil {
		im.takeBack()
		return im.out, err
	}
	// The tables it made or filled are answered by Row Query now, not at the
	// repair sweep's next pass, and a big fill is counted for the planner.
	for key, id := range im.out.Tables {
		indexSoon(ctx, id, im.filled[key])
	}
	return im.out, nil
}

// takeBack takes away the tables the import made and undoes what it wrote to
// a table it came into, then takes out the options it gave that table's
// fields, whatever cut the import off: a job cancelled or past its time
// leaves no half import behind, so it runs on a context that outlives the
// import's. What it can't take back is logged; a table whose rows it couldn't
// take back keeps the options they hold.
func (im *importer) takeBack() {
	ctx := context.WithoutCancel(im.ctx)
	for key, id := range im.out.Tables {
		var err error
		if set, ok := im.wrote[key]; ok {
			_, err = Undo(ctx, id, &set, im.actor)
		} else if im.tables[key].Into == uuid.Nil {
			err = DeleteTable(ctx, id, im.actor)
		}
		if err != nil {
			helpers.LogErrorWithContext(ctx, "tables: import: table %s couldn't be taken back after the import failed: %v", id, err)
			continue
		}
		im.dropOptions(ctx, id, im.gave[key])
	}
}

// dropOptions takes the options the import gave table's fields (their ids,
// by field) out of them, as a person takes one out: left out of the field's
// options and named in removedOptionsKey, so an option added since stays.
// One a live row holds (another person took it up while the import ran)
// stays too, as taking it out would empty that row's cell. A field no longer
// of a kind with options has none to take out. What it can't take out is
// logged.
func (im *importer) dropOptions(ctx context.Context, table uuid.UUID, gave map[uuid.UUID][]string) {
	if len(gave) == 0 {
		return
	}
	fields, err := model.ListFields(ctx, table)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "tables: import: table %s's fields couldn't be read to take out the options the import gave them: %v", table, err)
		return
	}
	for _, f := range fields {
		ids := gave[f.Id]
		if len(ids) == 0 || !hasOptions(f.Type) {
			continue
		}
		mine := map[string]bool{}
		for _, id := range ids {
			mine[id] = true
		}
		cfg := map[string]any{}
		_ = json.Unmarshal([]byte(f.Config), &cfg)
		opts := fieldTypes.Options(cfg)
		var labels []string
		for _, o := range opts {
			if mine[fieldTypes.OptionID(o)] {
				labels = append(labels, fieldTypes.OptionLabel(o))
			}
		}
		if len(labels) == 0 {
			continue
		}
		held, err := model.LabelsHeld(ctx, table, f.Id, labels)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "tables: import: the options the import gave field %s couldn't be checked to take them out after it failed: %v", f.Id, err)
			continue
		}
		kept, removed := []any{}, []any{}
		for _, o := range opts {
			if id := fieldTypes.OptionID(o); mine[id] && !held[fieldTypes.OptionLabel(o)] {
				removed = append(removed, id)
			} else {
				kept = append(kept, o)
			}
		}
		if len(removed) == 0 {
			continue
		}
		cfg["options"], cfg[removedOptionsKey] = kept, removed
		if _, err := UpdateField(ctx, table, f.Id, FieldPatch{Config: cfg}, im.actor); err != nil {
			helpers.LogErrorWithContext(ctx, "tables: import: the options the import gave field %s couldn't be taken out after it failed: %v", f.Id, err)
		}
	}
}

func (im *importer) run() error {
	for i := range im.base.Tables {
		if t := &im.base.Tables[i]; t.Into != uuid.Nil {
			im.intoTable(t)
		} else if err := im.makeTable(t); err != nil {
			return err
		}
	}
	// A table the rows come into keeps its own fields and views.
	for i := range im.base.Tables {
		if t := &im.base.Tables[i]; t.Into == uuid.Nil {
			im.makeRelations(t)
		}
	}
	for i := range im.base.Tables {
		if t := &im.base.Tables[i]; t.Into == uuid.Nil {
			im.makeComputed(t)
		}
	}
	for i := range im.base.Tables {
		if err := im.makeRows(&im.base.Tables[i]); err != nil {
			return err
		}
	}
	for i := range im.base.Tables {
		if err := im.makeLinks(&im.base.Tables[i]); err != nil {
			return err
		}
	}
	for i := range im.base.Tables {
		if t := &im.base.Tables[i]; t.Into == uuid.Nil {
			im.makeViews(t)
		}
	}
	return nil
}

// intoTable takes t's rows into t.Into: a field's cells into the field of
// Into its Into names.
func (im *importer) intoTable(t *ImportTable) {
	im.out.Tables[t.Key] = t.Into
	for _, f := range t.Fields {
		if f.Into != uuid.Nil {
			im.out.Fields[t.Key+"/"+f.Key] = f.Into
			im.stored[t.Key+"/"+f.Key] = true
		}
	}
}

// plain is whether a field is made with its table: not a relation, a rollup
// or a formula, which read other fields.
func plain(f ImportField) bool {
	return f.Type != model.FieldRelation && f.Type != model.FieldRollup && f.Type != model.FieldFormula
}

// importName is a name as a table or a field takes it.
func importName(s, none string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		s = none
	}
	if r := []rune(s); len(r) > 120 {
		s = string(r[:120])
	}
	return s
}

// uniqueName is name, numbered on from 2 ("Notes 2") while one taken has it,
// ignoring case; seen holds those taken, and takes it.
func uniqueName(name string, seen map[string]bool) string {
	out := name
	for n := 2; seen[strings.ToLower(out)]; n++ {
		out = fmt.Sprintf("%s %d", name, n)
	}
	seen[strings.ToLower(out)] = true
	return out
}

func (im *importer) makeTable(t *ImportTable) error {
	visibility := model.VisibilityWorkspace
	if im.base.Private {
		visibility = model.VisibilityPrivate
	}
	icon := t.Icon
	if len(icon) > 64 {
		icon = ""
	}
	var fields []FieldInput
	seen := map[string]bool{}
	for i, f := range t.Fields {
		if !plain(f) {
			continue
		}
		name := uniqueName(importName(f.Name, "Field"), seen)
		t.Fields[i].Name = name
		fields = append(fields, FieldInput{Name: name, Type: f.Type, Config: f.Config, Description: f.Description, Position: float64(i)})
	}
	table, err := CreateTableFromTemplate(im.ctx, TableInput{Name: importName(t.Name, "Untitled table"), Description: t.Description, Icon: icon, Visibility: visibility}, fields, nil, im.actor)
	if err != nil {
		return fmt.Errorf("make %q: %w", t.Name, err)
	}
	im.out.Tables[t.Key] = table.Id
	made, err := model.ListFields(im.ctx, table.Id)
	if err != nil {
		return fmt.Errorf("read %q's fields: %w", t.Name, err)
	}
	byName := map[string]*model.Field{}
	for _, f := range made {
		byName[strings.ToLower(f.Name)] = f
	}
	for _, f := range t.Fields {
		if !plain(f) {
			continue
		}
		if m := byName[strings.ToLower(f.Name)]; m != nil {
			im.out.Fields[t.Key+"/"+f.Key] = m.Id
			im.stored[t.Key+"/"+f.Key] = true
		} else {
			im.note(t.Key, f.Key, "", "FIELD_LEFT_OUT", fmt.Sprintf("%q couldn't be made as a %s field, so its cells aren't brought.", f.Name, f.Type))
		}
	}
	return nil
}

// makeRelations makes t's links to tables, each pair once: the side met
// first is made showing from both, and the other side is its other half.
func (im *importer) makeRelations(t *ImportTable) {
	for _, f := range t.Fields {
		if f.Type != model.FieldRelation {
			continue
		}
		if _, done := im.field(t.Key, f.Key); done {
			continue // the other half of a pair made from the other table
		}
		target, ok := im.out.Tables[f.LinksTo]
		if !ok {
			im.note(t.Key, f.Key, "", "LINK_LEFT_OUT", fmt.Sprintf("%q links to a table that wasn't brought, so its links aren't.", f.Name))
			continue
		}
		cfg := map[string]any{"relation_target": linksTable, "table_id": target.String(), "two_way": f.Pair != ""}
		made, err := CreateField(im.ctx, im.out.Tables[t.Key], FieldInput{Name: f.Name, Type: model.FieldRelation, Config: cfg, Description: f.Description}, im.actor)
		if err != nil {
			im.note(t.Key, f.Key, "", "LINK_LEFT_OUT", fmt.Sprintf("%q couldn't be made: %v", f.Name, err))
			continue
		}
		im.out.Fields[t.Key+"/"+f.Key] = made.Id
		if f.Pair == "" {
			continue
		}
		other := im.tables[f.LinksTo]
		var pair *ImportField
		for i := range other.Fields {
			if other.Fields[i].Key == f.Pair {
				pair = &other.Fields[i]
			}
		}
		fields, err := model.ListFields(im.ctx, target)
		if err != nil {
			continue
		}
		for _, of := range fields {
			if l, ok := linkOf(of); ok && l.inverseOf == made.Id.String() {
				im.out.Fields[f.LinksTo+"/"+f.Pair] = of.Id
				if pair != nil && strings.TrimSpace(pair.Name) != "" && pair.Name != of.Name {
					name := importName(pair.Name, of.Name)
					_, _ = UpdateField(im.ctx, target, of.Id, FieldPatch{Name: &name}, im.actor)
				}
			}
		}
	}
}

// makeComputed makes t's rollups, then its formulas (which can read them).
func (im *importer) makeComputed(t *ImportTable) {
	table := im.out.Tables[t.Key]
	for _, f := range t.Fields {
		if f.Type != model.FieldRollup {
			continue
		}
		through, ok := im.field(t.Key, f.Through)
		var of uuid.UUID
		var linked string
		for _, rf := range t.Fields {
			if rf.Key == f.Through {
				linked = rf.LinksTo
			}
		}
		if f.Of != "" {
			of, _ = im.field(linked, f.Of)
		}
		if !ok || (f.Of != "" && of == uuid.Nil) || f.Aggregate == "" {
			im.fallback(t, f, "ROLLUP_AS_VALUES", fmt.Sprintf("%q couldn't be made as a rollup here", f.Name))
			continue
		}
		cfg := map[string]any{"relation": through.String(), "aggregate": f.Aggregate}
		if of != uuid.Nil {
			cfg["field"] = of.String()
		}
		for k, v := range f.Config {
			if _, set := cfg[k]; !set {
				cfg[k] = v
			}
		}
		made, err := CreateField(im.ctx, table, FieldInput{Name: f.Name, Type: model.FieldRollup, Config: cfg, Description: f.Description}, im.actor)
		if err != nil {
			im.fallback(t, f, "ROLLUP_AS_VALUES", fmt.Sprintf("%q couldn't be made as a rollup (%v)", f.Name, err))
			continue
		}
		im.out.Fields[t.Key+"/"+f.Key] = made.Id
	}
	// Formulas read each other by name: each pass makes those whose fields
	// are there, until a pass makes none.
	left := []ImportField{}
	for _, f := range t.Fields {
		if f.Type == model.FieldFormula {
			left = append(left, f)
		}
	}
	for len(left) > 0 {
		var still []ImportField
		var lastErr = map[string]error{}
		for _, f := range left {
			made, err := CreateField(im.ctx, table, FieldInput{Name: f.Name, Type: model.FieldFormula, Config: map[string]any{"formula": f.Formula}, Description: f.Description}, im.actor)
			if err != nil {
				still, lastErr[f.Key] = append(still, f), err
				continue
			}
			im.out.Fields[t.Key+"/"+f.Key] = made.Id
		}
		if len(still) == len(left) {
			for _, f := range still {
				im.fallback(t, f, "FORMULA_AS_VALUES", fmt.Sprintf("%q's formula couldn't be read here (%v); it was %s", f.Name, lastErr[f.Key], f.Formula))
			}
			break
		}
		left = still
	}
}

// fallback makes a rollup or a formula that couldn't be made as its stored
// Fallback kind, keeping its source's cells as values, and says so.
func (im *importer) fallback(t *ImportTable, f ImportField, code, why string) {
	if f.Fallback == "" {
		im.note(t.Key, f.Key, "", strings.Replace(code, "_AS_VALUES", "_LEFT_OUT", 1), why+", so it isn't brought.")
		return
	}
	made, err := CreateField(im.ctx, im.out.Tables[t.Key], FieldInput{Name: f.Name, Type: f.Fallback, Description: f.Description}, im.actor)
	if err != nil {
		im.note(t.Key, f.Key, "", strings.Replace(code, "_AS_VALUES", "_LEFT_OUT", 1), fmt.Sprintf("%s, and couldn't be kept as values: %v", why, err))
		return
	}
	im.out.Fields[t.Key+"/"+f.Key] = made.Id
	im.stored[t.Key+"/"+f.Key] = true
	im.note(t.Key, f.Key, "", code, why+", so its values are kept as they were, and no longer worked out.")
}

// writable is the fields whose cells are written with a row (stored ones: not
// worked out on each read, and not links, written after every row is there).
func (im *importer) writable(t *ImportTable) map[string]uuid.UUID {
	out := map[string]uuid.UUID{}
	for _, f := range t.Fields {
		if !im.stored[t.Key+"/"+f.Key] {
			continue
		}
		if id, ok := im.field(t.Key, f.Key); ok {
			out[f.Key] = id
		}
	}
	return out
}

func (im *importer) makeRows(t *ImportTable) error {
	if t.Rows == nil {
		return nil
	}
	table := im.out.Tables[t.Key]
	cols := im.writable(t)
	keyOf := map[string]string{} // field id → its key, for a refused cell's note
	for k, id := range cols {
		keyOf[id.String()] = k
	}
	files := map[string]bool{} // files fields, by key
	for _, f := range t.Fields {
		if f.Type == model.FieldAttachment {
			files[f.Key] = true
		}
	}
	options, err := im.selectFields(t, table, cols)
	if err != nil {
		return fmt.Errorf("read %q's fields: %w", t.Name, err)
	}
	match, err := im.matching(t, table, cols)
	if err != nil {
		return fmt.Errorf("read %q's rows: %w", t.Name, err)
	}
	return t.Rows(func(page []ImportRow) error {
		for len(page) > 0 {
			batch := page[:match.batch(page)]
			page = page[len(batch):]
			in := BatchInput{Kind: string(rowLedger.ChangeImport), Operation: im.op}
			var made, changed []ImportRow // the rows in.Create and in.Patch write
			for _, r := range batch {
				values := map[string]any{}
				for k, v := range r.Cells {
					if id, ok := cols[k]; ok && v != nil {
						if files[k] {
							if v = im.keepFiles(t, table, k, r.Key, v); v == nil {
								continue
							}
						}
						values[id.String()] = v
					}
				}
				row, ok, repeated := match.row(r)
				if repeated {
					im.note(t.Key, t.MatchBy, r.Key, "KEY_REPEATED", fmt.Sprintf("An earlier row names %q too, so this one changes the same row.", match.value(r)))
				}
				if ok {
					in.Patch = append(in.Patch, PatchInput{Row: row, Values: values})
					changed = append(changed, r)
				} else {
					in.Create = append(in.Create, RowInput{Values: values})
					made = append(made, r)
				}
				if len(r.Links) > 0 {
					im.links[t.Key] = append(im.links[t.Key], ImportRow{Key: r.Key, Links: r.Links})
				}
			}
			written := slices.Clone(in.Create) // a changed row's values bring options as a made row's do
			for _, p := range in.Patch {
				written = append(written, RowInput{Values: p.Values})
			}
			if err := im.addOptions(t, table, options, keyOf, written); err != nil {
				return fmt.Errorf("read %q's fields: %w", t.Name, err)
			}
			res, err := Batch(im.ctx, table, in, im.actor)
			var refused BatchErrors
			if errors.As(err, &refused) {
				// A cell no kind accepts is left out and said, as is a row
				// refused whole (one too large to keep); the rest go in.
				patched := map[uuid.UUID]int{}
				for i, p := range in.Patch {
					patched[p.Row] = i
				}
				wholeMade, wholeChanged := map[int]bool{}, map[int]bool{}
				for _, e := range refused {
					i, whole, rows, values := -1, wholeMade, made, map[string]any(nil)
					if e.Index != nil && *e.Index < len(made) {
						i, values = *e.Index, in.Create[*e.Index].Values
					} else if j, ok := patched[e.Row]; ok && e.Index == nil {
						i, whole, rows, values = j, wholeChanged, changed, in.Patch[j].Values
					}
					switch {
					case i < 0:
					case e.Field == "":
						whole[i] = true
						im.note(t.Key, "", rows[i].Key, "ROW_LEFT_OUT", "The row couldn't be brought ("+e.Msg+").")
					default:
						delete(values, e.Field)
						im.note(t.Key, keyOf[e.Field], rows[i].Key, "CELL_LEFT_OUT", e.Msg)
					}
				}
				made, in.Create = withoutRows(made, wholeMade), withoutRows(in.Create, wholeMade)
				in.Patch = withoutRows(in.Patch, wholeChanged)
				if len(in.Create)+len(in.Patch) == 0 {
					continue
				}
				res, err = Batch(im.ctx, table, in, im.actor)
			}
			if err != nil {
				return fmt.Errorf("rows of %q: %w", t.Name, err)
			}
			if _, ok := im.wrote[t.Key]; !ok && t.Into != uuid.Nil {
				im.wrote[t.Key] = res.ChangeSet
			}
			for i, r := range res.Created {
				if i < len(made) {
					im.out.Rows[t.Key+"/"+made[i].Key] = r.Id
					match.made(made[i], r.Id)
				}
			}
			im.out.RowsMade += len(res.Created)
			im.out.RowsChanged += len(in.Patch)
			im.filled[t.Key] += len(res.Created) + len(in.Patch)
		}
		return nil
	})
}

// rowMatch names the row of a table that a row coming into it changes: the
// one its value of the table's MatchBy field names, among the table's rows
// and those the import made.
type rowMatch struct {
	key  string               // MatchBy; "" names no row
	text func(string) string  // a value's text as it names a row (matchText)
	rows map[string]uuid.UUID // a value's text, its row
	seen map[string]bool      // the values the import's rows named
}

// matching is t's rowMatch: none unless its rows come into a table by a
// field.
func (im *importer) matching(t *ImportTable, table uuid.UUID, cols map[string]uuid.UUID) (*rowMatch, error) {
	if t.Into == uuid.Nil || t.MatchBy == "" {
		return &rowMatch{}, nil
	}
	field, ok := cols[t.MatchBy]
	if !ok {
		return nil, fmt.Errorf("its rows are matched by %q, which doesn't come", t.MatchBy)
	}
	var kind string
	if i := slices.IndexFunc(t.Fields, func(f ImportField) bool { return f.Key == t.MatchBy }); i >= 0 {
		kind = t.Fields[i].Type
	}
	text := matchText(kind)
	rows, err := model.RowsByValue(im.ctx, table, field, text)
	if err != nil {
		return nil, err
	}
	return &rowMatch{key: t.MatchBy, text: text, rows: rows, seen: map[string]bool{}}, nil
}

// matchText is the text a value of a field of kind names a row by, as people
// tell such values apart: an email whatever its case, a link whatever the
// case of its scheme and host (linkKey), a phone number by its digits
// however they're spaced (fieldTypes.PhoneKey), any other as written.
func matchText(kind string) func(string) string {
	switch kind {
	case model.FieldEmail:
		return helpers.NormalizeEmail
	case model.FieldURL:
		return linkKey
	case model.FieldPhone:
		return fieldTypes.PhoneKey
	}
	return func(s string) string { return s }
}

// linkKey is a link as it names a page: its scheme and host in lower case,
// which the web reads whatever their case, and the rest as written, which a
// site may not. Text that isn't such a link is itself. Pure.
func linkKey(s string) string {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return s
	}
	u.Host = strings.ToLower(u.Host) // Parse lowers the scheme
	return u.String()
}

// value is the text r names a row by, as RowsByValue reads a stored value: a
// string as matchText has it, a number as written; "" for none.
func (m *rowMatch) value(r ImportRow) string {
	if m.key == "" {
		return ""
	}
	switch v := r.Cells[m.key].(type) {
	case string:
		return m.text(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

// batch is how many of page's rows one batch writes: at most maxBatch, ending
// before a row that names a row an earlier one in it does, so each batch
// writes a row once and a later row's values win.
func (m *rowMatch) batch(page []ImportRow) int {
	n := min(maxBatch, len(page))
	named := map[string]bool{}
	for i, r := range page[:n] {
		if v := m.value(r); v != "" {
			if named[v] {
				return i
			}
			named[v] = true
		}
	}
	return n
}

// row is the row r changes, when it names one, and whether an earlier row of
// the import named the same.
func (m *rowMatch) row(r ImportRow) (row uuid.UUID, ok, repeated bool) {
	v := m.value(r)
	if v == "" {
		return uuid.Nil, false, false
	}
	repeated, m.seen[v] = m.seen[v], true
	row, ok = m.rows[v]
	return row, ok, repeated
}

// made takes row, which the import made of r, as the one r's value names.
func (m *rowMatch) made(r ImportRow, row uuid.UUID) {
	if v := m.value(r); v != "" {
		m.rows[v] = row
	}
}

// withoutRows is rows without those at the places drop names. Pure.
func withoutRows[T any](rows []T, drop map[int]bool) []T {
	if len(drop) == 0 {
		return rows
	}
	out := make([]T, 0, len(rows))
	for i, r := range rows {
		if !drop[i] {
			out = append(out, r)
		}
	}
	return out
}

// tableOptions is what the rows step keeps of a table's select and
// multi-select fields between batches: each as stored (in the source's
// order), and those the person importing may not change.
type tableOptions struct {
	cols    map[string]uuid.UUID // the fields a row writes, by key
	fields  []*model.Field
	refused map[uuid.UUID]bool
}

// selectFields is the select and multi-select fields among cols (the fields a
// row writes, by key) as the table stores them, in t's order.
func (im *importer) selectFields(t *ImportTable, table uuid.UUID, cols map[string]uuid.UUID) (*tableOptions, error) {
	out := &tableOptions{cols: cols, refused: map[uuid.UUID]bool{}}
	has := false
	for _, f := range t.Fields {
		_, written := cols[f.Key]
		has = has || (written && (f.Type == model.FieldSelect || f.Type == model.FieldMultiSelect))
	}
	if !has {
		return out, nil
	}
	return out, im.readSelects(t, table, out)
}

// readSelects reads o's fields as the table stores them now.
func (im *importer) readSelects(t *ImportTable, table uuid.UUID, o *tableOptions) error {
	stored, err := model.ListFields(im.ctx, table)
	if err != nil {
		return err
	}
	byID := map[uuid.UUID]*model.Field{}
	for _, f := range stored {
		byID[f.Id] = f
	}
	o.fields = o.fields[:0]
	for _, f := range t.Fields {
		if s := byID[o.cols[f.Key]]; s != nil && (s.Type == model.FieldSelect || s.Type == model.FieldMultiSelect) {
			o.fields = append(o.fields, s)
		}
	}
	return nil
}

// optionKey is a label as the app tells options apart: by their text,
// ignoring case and the spaces around it ("" for none).
func optionKey(label string) string { return strings.ToLower(strings.TrimSpace(label)) }

// addOptions makes each value a batch's rows give a select or a multi-select
// that isn't one of its options an option of it, so the cell keeps the value,
// as Airtable's and Notion's own imports do: once a field however many rows
// carry it (the next batch finds it there), as the app adds one by hand (an
// id as the web makes one, fieldTypes.NewOptionID, its label and no colour),
// and said in the notes. Each field's options are saved through the
// field-edit API as the person importing (UpdateField), so its own check
// (loadManageable) decides: in a table the import made, theirs, always; in
// one already there, only when they may change its fields, and kept in
// im.gave for a take-back. A value it adds no option for (one the app would
// take for an option there already, as it differs only in case or spaces) is
// left out of its cell and said, as before.
func (im *importer) addOptions(t *ImportTable, table uuid.UUID, o *tableOptions, keyOf map[string]string, rows []RowInput) error {
	added := false
	for _, f := range o.fields {
		if o.refused[f.Id] {
			continue
		}
		id := f.Id.String()
		taken := map[string]bool{"": true}
		for _, opt := range fieldTypes.OptionsOf(f) {
			taken[optionKey(opt.Label)] = true
		}
		var labels []string
		for _, r := range rows {
			for _, l := range fieldTypes.NotOptions(f, r.Values[id]) {
				if k := optionKey(l); !taken[k] {
					taken[k] = true
					labels = append(labels, l)
				}
			}
		}
		if len(labels) == 0 {
			continue
		}
		cfg := map[string]any{}
		_ = json.Unmarshal([]byte(f.Config), &cfg)
		list := []any{}
		for _, opt := range fieldTypes.Options(cfg) {
			list = append(list, opt)
		}
		made := make([]string, len(labels))
		for i, l := range labels {
			made[i] = fieldTypes.NewOptionID()
			list = append(list, map[string]any{"id": made[i], "label": l})
		}
		cfg["options"] = list
		if _, err := UpdateField(im.ctx, table, f.Id, FieldPatch{Config: cfg}, im.actor); err != nil {
			if o.refused[f.Id] = IsForbidden(err); !o.refused[f.Id] {
				helpers.LogWarnWithContext(im.ctx, "business/DataTable import: options for field %s weren't added: %v", f.Id, err)
			}
			continue
		}
		if t.Into != uuid.Nil { // a table the import made goes whole on a take-back
			if im.gave[t.Key] == nil {
				im.gave[t.Key] = map[uuid.UUID][]string{}
			}
			im.gave[t.Key][f.Id] = append(im.gave[t.Key][f.Id], made...)
		}
		added = true
		for _, l := range labels {
			im.note(t.Key, keyOf[id], "", "OPTION_ADDED", fmt.Sprintf("%q gained the option %q", f.Name, l))
		}
	}
	if !added {
		return nil
	}
	return im.readSelects(t, table, o)
}

// keepFiles is a files cell's ImportFiles kept as files of the table, as the
// cell holds them; nil when none was kept. A file not kept is said.
func (im *importer) keepFiles(t *ImportTable, table uuid.UUID, field, row string, v any) any {
	list, _ := v.([]any)
	var out []any
	for _, x := range list {
		f, ok := x.(ImportFile)
		if !ok {
			continue
		}
		if im.base.Files == nil {
			im.note(t.Key, field, row, "FILE_LEFT_OUT", fmt.Sprintf("%q couldn't be brought.", f.Name))
			continue
		}
		ref, kept := im.base.Files(table, f)
		if !kept {
			im.note(t.Key, field, row, "FILE_LEFT_OUT", fmt.Sprintf("%q couldn't be brought.", f.Name))
			continue
		}
		out = append(out, map[string]any{"id": ref.ID, "name": ref.Name, "size": float64(ref.Size), "mime": ref.Mime})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// makeLinks sets each row's links through t's relations, once every table's
// rows are there. Of a pair, the side made first carries them (the other
// shows the same links).
func (im *importer) makeLinks(t *ImportTable) error {
	rows := im.links[t.Key]
	if len(rows) == 0 {
		return nil
	}
	table := im.out.Tables[t.Key]
	fields, err := model.ListFields(im.ctx, table)
	if err != nil {
		return err
	}
	owns := map[string]bool{} // our field ids that store their links
	for _, f := range fields {
		if l, ok := linkOf(f); ok && l.inverseOf == "" {
			owns[f.Id.String()] = true
		}
	}
	linkedTable := map[string]string{}
	for _, f := range t.Fields {
		if f.Type == model.FieldRelation {
			linkedTable[f.Key] = f.LinksTo
		}
	}
	var patch []PatchInput
	flush := func() error {
		if len(patch) == 0 {
			return nil
		}
		_, err := Batch(im.ctx, table, BatchInput{Kind: string(rowLedger.ChangeImport), Patch: patch, Operation: im.op}, im.actor)
		patch = patch[:0]
		var refused BatchErrors
		if errors.As(err, &refused) {
			for _, e := range refused {
				im.note(t.Key, "", e.Row.String(), "LINKS_LEFT_OUT", e.Msg)
			}
			return nil
		}
		return err
	}
	for _, r := range rows {
		id, ok := im.out.Rows[t.Key+"/"+r.Key]
		if !ok {
			continue
		}
		values := map[string]any{}
		for fk, to := range r.Links {
			fid, ok := im.field(t.Key, fk)
			if !ok || !owns[fid.String()] {
				continue
			}
			var ids []any // as a written cell holds them (linkItems)
			for _, k := range to {
				if other, ok := im.out.Rows[linkedTable[fk]+"/"+k]; ok {
					ids = append(ids, other.String())
				}
			}
			if len(ids) > maxLinks {
				im.note(t.Key, fk, r.Key, "LINKS_LEFT_OUT", fmt.Sprintf("A row links to at most %d rows through a field: %d of its %d links are brought.", maxLinks, maxLinks, len(ids)))
				ids = ids[:maxLinks]
			}
			if len(ids) > 0 {
				values[fid.String()] = ids
			}
		}
		if len(values) == 0 {
			continue
		}
		patch = append(patch, PatchInput{Row: id, Values: values})
		if len(patch) == maxBatch {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

func (im *importer) makeViews(t *ImportTable) {
	table := im.out.Tables[t.Key]
	for i, v := range t.Views {
		cfg := map[string]any{}
		for k, val := range v.Config {
			cfg[k] = val
		}
		if _, err := CreateView(im.ctx, table, ViewInput{Name: importName(v.Name, "View"), Type: v.Type, Config: cfg, Position: float64(i)}, im.actor); err != nil {
			im.note(t.Key, "", "", "VIEW_LEFT_OUT", fmt.Sprintf("The view %q couldn't be made: %v", v.Name, err))
		}
	}
}
