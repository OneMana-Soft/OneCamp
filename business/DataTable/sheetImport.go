package business

// Spreadsheet files into tables (tables S1, design/tables-vs-airtable/
// PROGRAM.md §15): a Content IO importer into tables (IntoTable), so the
// pipeline keeps the file, the job and its progress, while ImportBaseAs makes
// the tables, as it does every source's. It previews first (a Previewer):
// each sheet's columns, their guessed kinds and its first rows, read as the
// person says (number signs, day order, zone), until they confirm which
// sheets come and each column's name and kind. Each sheet becomes a table,
// private to the person importing as an imported base is, and its rows are
// written through the Row Ledger as the import's change sets.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	contentIOBusiness "github.com/akashc777/OneCamp/business/ContentIO"
	formula "github.com/akashc777/OneCamp/business/Formula"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

func init() {
	contentIOBusiness.Pipeline.RegisterImporter(sheetImporter{})
}

const (
	// sheetRowsPerPage is how many rows a sheet hands the importer at once.
	sheetRowsPerPage = 500
	// maxNamedSkips is how many of what an import left out its job names.
	maxNamedSkips = 100
	// A preview shows each cell of its sample cut to previewCellRunes, and
	// stops sampling once its samples hold previewTextBytes in all, so a
	// workbook of many wide sheets stays within what a job keeps.
	previewCellRunes = 80
	previewTextBytes = 1 << 20
)

// sheetImporter reads spreadsheet files into tables.
type sheetImporter struct{}

func (sheetImporter) Format() string { return "sheet" }

func (sheetImporter) Into() string { return contentIOBusiness.IntoTable }

func (sheetImporter) Accepts(filename, _ string) bool { return contentIOBusiness.IsSheetFile(filename) }

// Import is never called: a spreadsheet is made into tables by Make.
func (sheetImporter) Import(context.Context, contentIOBusiness.Input, contentIOBusiness.Output) error {
	return errors.New("A spreadsheet is imported into a table.")
}

// sheetChoices are what the person chose of a spreadsheet's preview: how its
// text reads, and what comes of its sheets.
type sheetChoices struct {
	// Signs are the signs its numbers are written with; none for a point
	// before the decimals and commas between groups.
	Signs formula.NumberSigns `json:"signs"`
	// DayFirst says whether 03/04/2026 is the 3rd of April (when none is
	// said) or March the 4th.
	DayFirst *bool `json:"day_first"`
	// Zone is the person's (IANA): a moment written with its own zone falls
	// on its day there. None for UTC.
	Zone   string        `json:"zone"`
	Sheets []sheetChoice `json:"sheets"`
}

// sheetChoice is what comes of one sheet, by its place among the file's.
type sheetChoice struct {
	Index int `json:"index"`
	// Bring says whether it comes; without it, a sheet comes unless hidden.
	Bring *bool `json:"bring"`
	// Into is a table the sheet's rows come into, rather than a table of
	// their own: each column into the field of its name (or the one its
	// choice names), read as that field's kind.
	Into *uuid.UUID `json:"into"`
	// MatchBy is a column, by key, whose value names the row of Into that a
	// row of the sheet changes; a row naming none is made. Without it, every
	// row is made.
	MatchBy string        `json:"match_by"`
	Fields  []fieldChoice `json:"fields"`
}

// fieldChoice is one column as the person chose it, by its key: its name, its
// kind (sheetKinds), or left out; or, for a sheet coming into a table, the
// field of that table it goes into (Field, by id).
type fieldChoice struct {
	Key   string `json:"key"`
	Name  string `json:"name"`
	Type  string `json:"type"`
	Field string `json:"field"`
	Leave bool   `json:"leave"`
}

// targetTable is a table a sheet comes into, with its fields, as the person
// importing may see them.
type targetTable struct {
	table  *model.DataTable
	fields []*model.Field
}

// readChoices reads choices, and the options a sheet's text reads with by
// them: a point and commas, day first, and UTC when none are chosen.
func readChoices(choices json.RawMessage) (sheetChoices, SheetOptions, error) {
	var c sheetChoices
	opts := SheetOptions{Signs: formula.PlainSigns, DayFirst: true, Zone: time.UTC}
	if len(choices) > 0 && json.Unmarshal(choices, &c) != nil {
		return c, opts, contentIOBusiness.ReadError("The choices for this import couldn't be read.")
	}
	signs, ok := c.Signs.Checked()
	if !ok {
		return c, opts, contentIOBusiness.ReadError(fmt.Sprintf("Numbers aren't written with %q before their decimals and %q between groups.", c.Signs.Decimal, c.Signs.Group))
	}
	opts.Signs = signs
	if c.DayFirst != nil {
		opts.DayFirst = *c.DayFirst
	}
	if c.Zone != "" {
		zone, err := time.LoadLocation(c.Zone)
		if err != nil || c.Zone == "Local" { // Local is the server's zone, not the person's
			return c, opts, contentIOBusiness.ReadError(fmt.Sprintf("%q isn't a time zone.", c.Zone))
		}
		opts.Zone = zone
	}
	return c, opts, nil
}

// sheetPreview is what a spreadsheet's preview shows: how its text was read,
// and each sheet's guessed columns and first rows.
type sheetPreview struct {
	Signs    formula.NumberSigns `json:"signs"`
	DayFirst bool                `json:"day_first"`
	Zone     string              `json:"zone"`
	Sheets   []previewSheet      `json:"sheets"`
}

// previewSheet is one sheet in a preview, by its place among the file's.
type previewSheet struct {
	Index  int    `json:"index"`
	Name   string `json:"name"`
	Hidden bool   `json:"hidden,omitempty"`
	// Into, IntoName and MatchBy are, for a sheet chosen into a table, that
	// table and the column whose values name the rows it changes.
	Into     *uuid.UUID `json:"into,omitempty"`
	IntoName string     `json:"into_name,omitempty"`
	MatchBy  string     `json:"match_by,omitempty"`
	// Rows is how many rows with a value its header stands over.
	Rows   int            `json:"rows"`
	Fields []previewField `json:"fields"`
	// Sample is its first rows under the header, each cell as the file
	// shows it, cut to previewCellRunes.
	Sample [][]string    `json:"sample"`
	Notes  []previewNote `json:"notes,omitempty"`
}

// previewField is a column as it comes: its name and kind, or, coming into a
// table, those of the field it goes into (Field, by id); Leave is a column
// that doesn't come.
type previewField struct {
	Key    string         `json:"key"`
	Name   string         `json:"name"`
	Type   string         `json:"type"`
	Config map[string]any `json:"config,omitempty"`
	Field  string         `json:"field,omitempty"`
	Leave  bool           `json:"leave,omitempty"`
}

// previewNote is what a column keeps of its cells less than whole, by key.
type previewNote struct {
	Field string `json:"field"`
	Code  string `json:"code"`
	Msg   string `json:"msg"`
}

// sheetsAsChosen are a file's sheets with what the person importing chose of
// them, and the tables of theirs the choices bring sheets into.
type sheetsAsChosen struct {
	actor  Actor
	opts   SheetOptions
	chosen []sheetChoice
	sheets []contentIOBusiness.Sheet
	into   map[uuid.UUID]targetTable
}

// readAsChosen reads in's sheets for who as the choices say, with the tables
// they bring sheets into as who may open them.
func readAsChosen(ctx context.Context, who accessBusiness.Subject, in contentIOBusiness.Input, choices json.RawMessage) (sheetsAsChosen, error) {
	var r sheetsAsChosen
	id, err := uuid.Parse(who.UserUUID)
	if err != nil {
		return r, contentIOBusiness.ErrForbidden
	}
	r.actor = Actor{UserID: id, IsAdmin: who.Admin}
	c, opts, err := readChoices(choices)
	if err != nil {
		return r, err
	}
	r.opts, r.chosen = opts, c.Sheets
	if r.sheets, err = contentIOBusiness.ReadSheets(in); err != nil {
		return r, err
	}
	r.into, err = targets(ctx, r.actor, c.Sheets)
	return r, err
}

// Preview reads in's sheets for who as the choices say, and shows each one
// as it would come, and its first rows.
func (sheetImporter) Preview(ctx context.Context, who accessBusiness.Subject, in contentIOBusiness.Input, choices json.RawMessage) (json.RawMessage, error) {
	r, err := readAsChosen(ctx, who, in, choices)
	if err != nil {
		return nil, err
	}
	out, err := previewOf(r.sheets, r.opts, r.chosen, r.into)
	if err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

// previewOf is sheets as their preview shows them, read with opts and made
// what chosen says, as the import makes them (SheetColumns.chosen): each
// column as it comes or left out, a sheet coming into a table with that
// table and the column naming its rows, the notes, and the first rows. Pure.
func previewOf(sheets []contentIOBusiness.Sheet, opts SheetOptions, chosen []sheetChoice, into map[uuid.UUID]targetTable) (sheetPreview, error) {
	out := sheetPreview{Signs: opts.Signs, DayFirst: opts.DayFirst, Zone: opts.Zone.String(), Sheets: []previewSheet{}}
	choices, err := choicesOf(sheets, chosen)
	if err != nil {
		return out, err
	}
	text := 0
	for i, s := range sheets {
		cols, err := GuessColumns(s, opts)
		if err != nil {
			return out, err
		}
		var table ImportTable
		if err := cols.chosen(choices[i], into, &table); err != nil {
			return out, err
		}
		p := previewSheet{Index: i, Name: s.Name, Hidden: s.Hidden, MatchBy: table.MatchBy, Rows: cols.Rows, Fields: []previewField{}, Sample: [][]string{}}
		if table.Into != uuid.Nil {
			p.Into, p.IntoName = &table.Into, table.Name
		}
		for j, f := range cols.Fields {
			shown := previewField{Key: f.Key, Name: f.Name, Type: f.Type, Config: f.Config, Leave: cols.reads[j] == nil}
			if f.Into != uuid.Nil {
				shown.Field = f.Into.String()
			}
			p.Fields = append(p.Fields, shown)
		}
		for _, n := range cols.Notes {
			p.Notes = append(p.Notes, previewNote{Field: n.Field, Code: n.Code, Msg: n.Msg})
		}
		for _, row := range cols.sample {
			if text >= previewTextBytes {
				break
			}
			shown := make([]string, len(row))
			for j, cell := range row {
				shown[j] = helpers.TruncateRunes(cell, previewCellRunes)
				text += len(shown[j])
			}
			p.Sample = append(p.Sample, shown)
		}
		out.Sheets = append(out.Sheets, p)
	}
	return out, nil
}

// Make makes a table of each of in's sheets for who, as the choices the
// person confirmed say.
func (sheetImporter) Make(ctx context.Context, who accessBusiness.Subject, in contentIOBusiness.Input, choices json.RawMessage, report func(done, total int)) (contentIOBusiness.Made, error) {
	r, err := readAsChosen(ctx, who, in, choices)
	if err != nil {
		return contentIOBusiness.Made{}, err
	}
	base, err := sheetBase(r.sheets, r.opts, r.chosen, r.into, report)
	if err != nil {
		return contentIOBusiness.Made{}, err
	}
	made, err := ImportBaseAs(ctx, r.actor, base)
	return madeOf(base, made), err
}

// sheetBase is sheets as a base to import, private to the person importing:
// a table of each that comes (as chosen; a hidden sheet, a workbook's lists
// and lookups, only when chosen), named after it (numbered when an earlier
// sheet has its name), its columns' kinds guessed with opts and made what
// the person chose, and notes for what doesn't come whole. A hidden sheet
// left out unchosen, and an empty one, are said. A sheet chosen into a table
// comes into that one of into. As its rows are read, report hears how many of
// them all.
func sheetBase(sheets []contentIOBusiness.Sheet, opts SheetOptions, chosen []sheetChoice, into map[uuid.UUID]targetTable, report func(done, total int)) (ImportBase, error) {
	choices, err := choicesOf(sheets, chosen)
	if err != nil {
		return ImportBase{}, err
	}
	base := ImportBase{Private: true}
	names := map[string]bool{}
	done, total := 0, 0
	for i, s := range sheets {
		key := uniqueName(cmp.Or(strings.TrimSpace(s.Name), fmt.Sprintf("Sheet %d", i+1)), names)
		ch := choices[i]
		if ch.Bring != nil && !*ch.Bring {
			continue
		}
		if s.Hidden && ch.Bring == nil {
			base.Notes = append(base.Notes, ImportNote{Table: key, Code: "SHEET_LEFT_OUT", Msg: "The sheet is hidden in the file, so it wasn't brought."})
			continue
		}
		cols, err := GuessColumns(s, opts)
		if err != nil {
			return ImportBase{}, err
		}
		table := ImportTable{Key: key, Name: key}
		if err := cols.chosen(ch, into, &table); err != nil {
			return ImportBase{}, err
		}
		table.Fields = cols.kept()
		if len(table.Fields) == 0 {
			base.Notes = append(base.Notes, ImportNote{Table: key, Code: "SHEET_LEFT_OUT", Msg: "The sheet has no columns to bring."})
			continue
		}
		total += cols.Rows
		for _, n := range cols.Notes {
			n.Table = key
			// A column that doesn't come is no field of the import, so it's
			// said by its name.
			if i := slices.IndexFunc(cols.Fields, func(f ImportField) bool { return f.Key == n.Field }); i >= 0 && cols.reads[i] == nil {
				n.Field = cols.Fields[i].Name
			}
			base.Notes = append(base.Notes, n)
		}
		table.Rows = sheetRows(s, cols, func(n int) {
			done += n
			report(done, total)
		})
		base.Tables = append(base.Tables, table)
	}
	return base, nil
}

// SheetsBase is a source's spreadsheet as a base, as a TableSource brings one
// (Google Sheets): a table of each sheet that isn't hidden, its columns' kinds
// guessed with opts from what its cells hold and say (dropdowns, people chips,
// links), with notes for what doesn't come whole. Like Airtable's bases, it
// isn't private to the person importing.
func SheetsBase(sheets []contentIOBusiness.Sheet, opts SheetOptions, report func(done, total int)) (ImportBase, error) {
	base, err := sheetBase(sheets, opts, nil, nil, report)
	base.Private = false
	return base, err
}

// choicesOf are the choices by the sheet each is of, which sheets must have.
func choicesOf(sheets []contentIOBusiness.Sheet, chosen []sheetChoice) (map[int]sheetChoice, error) {
	out := map[int]sheetChoice{}
	for _, ch := range chosen {
		if ch.Index < 0 || ch.Index >= len(sheets) {
			return nil, contentIOBusiness.ReadError("The choices name a sheet the file doesn't have.")
		}
		out[ch.Index] = ch
	}
	return out, nil
}

// chosen makes the columns what ch chose of them, into table: each as chosen,
// or, for a sheet chosen into a table of the person's (one of into), each
// into a field of that table, its rows matched by ch.MatchBy.
func (s *SheetColumns) chosen(ch sheetChoice, into map[uuid.UUID]targetTable, table *ImportTable) error {
	if ch.Into == nil {
		if ch.MatchBy != "" {
			return contentIOBusiness.ReadError("Rows are matched by a column only when the sheet comes into a table.")
		}
		for _, f := range ch.Fields {
			if f.Field != "" {
				return contentIOBusiness.ReadError("A column goes into a field only when the sheet comes into a table.")
			}
			if err := s.choose(f); err != nil {
				return err
			}
		}
		return nil
	}
	target, ok := into[*ch.Into]
	if !ok {
		return errNoTarget
	}
	if err := s.into(target.fields, ch.Fields); err != nil {
		return err
	}
	if ch.MatchBy != "" {
		i := slices.IndexFunc(s.Fields, func(f ImportField) bool { return f.Key == ch.MatchBy })
		if i < 0 || s.reads[i] == nil {
			return contentIOBusiness.ReadError(fmt.Sprintf("The choices match rows by a column that doesn't come (%q).", ch.MatchBy))
		}
		if !matchKinds[s.Fields[i].Type] {
			return contentIOBusiness.ReadError(fmt.Sprintf("%q can't name a row: a row is named by text, a number, a date, a length of time, an email, a link or a phone number.", s.Fields[i].Name))
		}
	}
	table.Name, table.Into, table.MatchBy = target.table.Name, target.table.Id, ch.MatchBy
	return nil
}

// errNoTarget is a sheet chosen into a table that isn't there, or that the
// person importing can't open: which, it doesn't say.
var errNoTarget = contentIOBusiness.ReadError("The choices bring a sheet into a table that isn't there, or that you can't open.")

// targets are the tables the choices bring sheets into, each with its fields,
// as the person importing may open them; what they may write there, Batch
// says as the rows come.
func targets(ctx context.Context, actor Actor, chosen []sheetChoice) (map[uuid.UUID]targetTable, error) {
	out := map[uuid.UUID]targetTable{}
	for _, ch := range chosen {
		if ch.Into == nil {
			continue
		}
		if _, ok := out[*ch.Into]; ok {
			continue
		}
		t, err := loadViewable(ctx, *ch.Into, actor)
		if errors.Is(err, errNotFound) || errors.Is(err, errForbidden) {
			return nil, errNoTarget
		}
		if err != nil {
			return nil, err
		}
		fields, err := model.ListFields(ctx, t.Id)
		if err != nil {
			return nil, fmt.Errorf("failed to read the fields of the table to bring a sheet into")
		}
		out[t.Id] = targetTable{table: t, fields: fields}
	}
	return out, nil
}

// sheetRows hands a sheet's rows under its header to an import a page at a
// time, as cols store them, telling read how many each page held: each row
// GuessColumns counted, one whose only value its field keeps as none (an
// unticked box) among them. A row with nothing in it is left out.
func sheetRows(s contentIOBusiness.Sheet, cols *SheetColumns, read func(n int)) func(page func([]ImportRow) error) error {
	return func(page func([]ImportRow) error) error {
		var rows []ImportRow
		_, err := eachRow(s, func(header bool, cells []contentIOBusiness.Cell) error {
			if header {
				return nil
			}
			rows = append(rows, ImportRow{Cells: cols.Row(cells)})
			if len(rows) < sheetRowsPerPage {
				return nil
			}
			read(len(rows))
			err := page(rows)
			rows = nil
			return err
		})
		if err != nil || len(rows) == 0 {
			return err
		}
		read(len(rows))
		return page(rows)
	}
}

// madeOf is an import of base as its job keeps it: the tables made, the
// first to show, how many rows they hold, and the first maxNamedSkips of
// what was left out, each by its table's, field's and row's names. Pure.
func madeOf(base ImportBase, made *ImportedBase) contentIOBusiness.Made {
	var out contentIOBusiness.Made
	if made == nil {
		return out
	}
	fields := map[string]string{}       // their names, by "<table key>/<field key>"
	var into *contentIOBusiness.Created // the first table the rows came into
	for _, t := range base.Tables {
		if id, ok := made.Tables[t.Key]; ok {
			table := contentIOBusiness.Created{Kind: "table", UUID: id.String(), Title: t.Name}
			if t.Into == uuid.Nil {
				out.Created = append(out.Created, table)
			} else if into == nil {
				into = &table
			}
		}
		for _, f := range t.Fields {
			fields[t.Key+"/"+f.Key] = f.Name
		}
	}
	out.Show = into
	if len(out.Created) > 0 {
		out.Show = &out.Created[0]
	}
	out.Count = made.RowsMade + made.RowsChanged
	for _, n := range made.Notes[:min(len(made.Notes), maxNamedSkips)] {
		field := cmp.Or(fields[n.Table+"/"+n.Field], n.Field)
		what := strings.Join(nonEmpty(n.Table, field, n.Row), " / ")
		out.Skipped = append(out.Skipped, contentIOBusiness.Skipped{What: what, Why: n.Msg})
	}
	return out
}

// nonEmpty is parts without the empty ones. Pure.
func nonEmpty(parts ...string) []string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
