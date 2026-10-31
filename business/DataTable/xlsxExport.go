package business

// A table's view as an Excel workbook (tables, package 2b: POST
// /tables/{id}/export): the rows the view shows, in its order, read as its
// reader reads them, and the fields it shows, in its order, each cell written
// as what it holds by contentIOBusiness.WriteXLSX as the rows stream in. Who
// may: whoever may open the table, as for its CSV export, and only what a
// read of its rows gives them (a link to a table they can't open reads as a
// private row). A table they can't open, and one that isn't there, are not
// found, alike.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	contentIOBusiness "github.com/akashc777/OneCamp/business/ContentIO"
	fieldTypes "github.com/akashc777/OneCamp/business/FieldTypes"
	query "github.com/akashc777/OneCamp/business/QuerySpec"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// ExportInput is a view's export: the fields it shows, in order, with the
// widths it draws them at (pixels, by field id); and its rows, those a spec
// finds in its order (the view's rules, as Row Query answers them; none for
// every row in the table's order) or else those Rows names, in order (rules
// only the browser works out). Rows given empty names none: a view the page
// filtered down to nothing exports its header alone.
type ExportInput struct {
	Fields []uuid.UUID        `json:"fields"`
	Widths map[string]float64 `json:"widths,omitempty"`
	Spec   *query.Spec        `json:"spec,omitempty"`
	Rows   []uuid.UUID        `json:"rows,omitempty"`
}

// maxExportRowIDs is the most rows an export names by id: as many as a
// request's body holds (the app's routes take 8 MiB, and an id is about 39
// bytes of it), with room for its fields.
const maxExportRowIDs = 200_000

var (
	errExportFields = errors.New("name the fields to export: each a field of the table, once")
	errExportRows   = fmt.Errorf("name a view's rows by its rules or by their ids, not both, and at most %d ids", maxExportRowIDs)
)

// IsExportInput is a view's export asked for wrongly (errExportFields,
// errExportRows): a 400 in its own words.
func IsExportInput(err error) bool { return err == errExportFields || err == errExportRows }

// TableExport is a view's export made ready: the table and its fields
// checked, and its first rows read, so a refusal comes before any of the
// file does.
type TableExport struct {
	// Name is the table's, which the workbook's sheet and its file take.
	Name    string
	fields  []*model.Field
	columns []contentIOBusiness.WorkbookColumn
	// kinds are what each formula and rollup gives, by field id.
	kinds map[string]string
	zone  *time.Location
	// rows are those read and not yet written; more, whether next has more.
	rows []*model.Row
	more bool
	next rowWindows
}

// rowWindows reads a view's rows a window at a time: the next window, and
// whether there are more after it.
type rowWindows func(ctx context.Context) ([]*model.Row, bool, error)

// PrepareExport readies table's export of in for actor (see the file's
// comment for who may).
func PrepareExport(ctx context.Context, table uuid.UUID, actor Actor, in ExportInput) (*TableExport, error) {
	t, err := loadViewable(ctx, table, actor)
	if err != nil {
		return nil, unseen(err)
	}
	all, err := model.ListFields(ctx, t.Id)
	if err != nil {
		return nil, errRowsUnavailable
	}
	fields, err := fieldsNamed(all, in.Fields)
	if err != nil {
		return nil, err
	}
	if (in.Spec != nil && in.Rows != nil) || len(in.Rows) > maxExportRowIDs {
		return nil, errExportRows
	}
	var next rowWindows
	if in.Rows != nil {
		next = rowsNamed(t.Id, actor, in.Rows)
	} else {
		spec := query.Spec{}
		if in.Spec != nil {
			spec = *in.Spec
		}
		next = rowsFound(t.Id, actor, spec)
	}
	e := &TableExport{Name: t.Name, fields: fields, zone: zoneFrom(ctx), next: next}
	if e.rows, e.more, err = next(ctx); err != nil {
		// The rows' read checks again (RowsByID): access lost since is as above.
		return nil, unseen(err)
	}
	e.kinds = resultKinds(planComputed(asViewer(ctx, actor), all).program, fields)
	for _, f := range fields {
		e.columns = append(e.columns, e.column(f, in.Widths[f.Id.String()]))
	}
	return e, nil
}

// unseen is err with a table the reader can't open said as one that isn't
// there: whether it is is not theirs to learn. Pure.
func unseen(err error) error {
	if errors.Is(err, errForbidden) {
		return errNotFound
	}
	return err
}

// fieldsNamed are the fields of all named by ids, in their order: each must
// be one of them, once.
func fieldsNamed(all []*model.Field, ids []uuid.UUID) ([]*model.Field, error) {
	byID := make(map[uuid.UUID]*model.Field, len(all))
	for _, f := range all {
		byID[f.Id] = f
	}
	if len(ids) == 0 {
		return nil, errExportFields
	}
	out := make([]*model.Field, 0, len(ids))
	seen := map[uuid.UUID]bool{}
	for _, id := range ids {
		f, ok := byID[id]
		if !ok || seen[id] {
			return nil, errExportFields
		}
		seen[id] = true
		out = append(out, f)
	}
	return out, nil
}

// rowsFound are spec's rows in its order, a window at a time, as Row Query
// finds them for actor (QueryRows).
func rowsFound(table uuid.UUID, actor Actor, spec query.Spec) rowWindows {
	var after query.Cursor
	return func(ctx context.Context) ([]*model.Row, bool, error) {
		w, err := QueryRows(ctx, table, actor, spec, query.Page{Size: query.MaxPage, After: after}, false)
		if err != nil {
			return nil, false, err
		}
		after = w.Next
		return w.Rows, w.Next != "", nil
	}
}

// rowsNamed are the rows of ids that are the table's, in their order, a
// window at a time, each once, as actor may see it (RowsByID).
func rowsNamed(table uuid.UUID, actor Actor, ids []uuid.UUID) rowWindows {
	seen := make(map[uuid.UUID]bool, len(ids))
	left := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			left = append(left, id)
		}
	}
	return func(ctx context.Context) ([]*model.Row, bool, error) {
		if len(left) == 0 {
			return nil, false, nil
		}
		n := min(len(left), MaxRowsByID)
		rows, err := RowsByID(ctx, table, actor, left[:n])
		left = left[n:]
		return rows, len(left) > 0, err
	}
}

// WriteXLSX writes the export to w as an Excel workbook, its rows as they're
// read, and answers what was cut to Excel's limits, which its Notes sheet
// says too.
func (e *TableExport) WriteXLSX(ctx context.Context, w io.Writer) ([]string, error) {
	rows := func(row func([]contentIOBusiness.Cell) error) error {
		for {
			for _, r := range e.rows {
				values := parseRowValues(r.Values)
				cells := make([]contentIOBusiness.Cell, len(e.fields))
				for i, f := range e.fields {
					cells[i] = e.cell(f, values[f.Id.String()])
				}
				if err := row(cells); err != nil {
					return err
				}
			}
			if !e.more {
				return nil
			}
			var err error
			if e.rows, e.more, err = e.next(ctx); err != nil {
				return err
			}
		}
	}
	return contentIOBusiness.WriteXLSX(w, []contentIOBusiness.WorkbookSheet{{Name: e.Name, Columns: e.columns, Rows: rows}})
}

// exportFormat is a number field's format (its config's "format"), as the
// grid shows its numbers: a currency's sign, fixed decimal places, a
// percent, or for a formula's or a rollup's number a length of time.
type exportFormat struct {
	Currency string `json:"currency"`
	Decimals *int   `json:"decimals"`
	Percent  bool   `json:"percent"`
	Duration string `json:"duration"`
}

// formatOf is f's number format; a duration's, its config's "format" too.
func formatOf(f *model.Field) exportFormat {
	var cfg struct {
		Format json.RawMessage `json:"format"`
	}
	_ = json.Unmarshal([]byte(f.Config), &cfg)
	var out exportFormat
	if f.Type == model.FieldDuration {
		_ = json.Unmarshal(cfg.Format, &out.Duration)
		return out
	}
	_ = json.Unmarshal(cfg.Format, &out)
	return out
}

// column is f's column: its name, its width in characters from the view's
// pixels (Excel's are about seven pixels each), its numbers' decimal places,
// a length of time's seconds, and a select's or a status's options.
func (e *TableExport) column(f *model.Field, px float64) contentIOBusiness.WorkbookColumn {
	c := contentIOBusiness.WorkbookColumn{Name: f.Name}
	if px > 0 {
		c.Width = math.Round(px/7*100) / 100
	}
	format := formatOf(f)
	if format.Decimals != nil && *format.Decimals >= 0 && *format.Decimals <= 6 {
		c.Fixed, c.Places = true, *format.Decimals
	}
	c.Seconds = format.Duration == "h:mm:ss"
	if f.Type == model.FieldSelect || f.Type == model.FieldStatus {
		for _, o := range fieldTypes.OptionsOf(f) {
			c.Choices = append(c.Choices, o.Label)
		}
	}
	return c
}

// cell is a cell of field f, as its reader reads it, as the workbook holds
// it: a number, a day or a moment, a length of time and a checkbox as
// themselves; a formula that couldn't be worked out as Excel's #VALUE!; and
// anything else as the grid's text of it, a list as its names, with what the
// reader can't open as one phrase, never by its id.
func (e *TableExport) cell(f *model.Field, v any) contentIOBusiness.Cell {
	switch x := v.(type) {
	case nil:
		if f.Type == model.FieldCheckbox {
			return contentIOBusiness.BoolCell(false)
		}
		return contentIOBusiness.Cell{}
	case []any:
		return contentIOBusiness.Cell{Text: listText(x)}
	case map[string]any:
		if _, failed := x["error"]; failed {
			return contentIOBusiness.Cell{Kind: contentIOBusiness.CellError, Text: "#VALUE!"}
		}
		return contentIOBusiness.Cell{Text: listText([]any{x})}
	case bool:
		return contentIOBusiness.BoolCell(x)
	case float64:
		return e.number(f, x)
	case string:
		gives := f.Type
		if k, ok := e.kinds[f.Id.String()]; ok {
			gives = k
		}
		switch gives {
		case model.FieldDate, model.FieldCreatedTime, model.FieldModifiedTime:
			if c, ok := e.moment(x); ok {
				return c
			}
		}
		return contentIOBusiness.Cell{Text: x}
	}
	return contentIOBusiness.Cell{Text: strings.Join(fieldTypes.Labels(v), ", ")}
}

// number is n, a cell of f, as its format shows it: an id with its prefix
// as text, a duration's seconds as a length of time, and a percent or an
// amount of money as one.
func (e *TableExport) number(f *model.Field, n float64) contentIOBusiness.Cell {
	if f.Type == model.FieldAutonumber {
		if p := fieldTypes.IDPrefix(f); p != "" {
			return contentIOBusiness.Cell{Text: fmt.Sprintf("%s-%d", p, int64(n))}
		}
	}
	format := formatOf(f)
	if f.Type == model.FieldDuration || format.Duration != "" {
		return contentIOBusiness.DurationCell(n)
	}
	c := contentIOBusiness.Cell{Kind: contentIOBusiness.CellNumber, Number: n, Percent: format.Percent}
	if format.Currency == "USD" || format.Currency == "INR" {
		c.Currency = format.Currency
	}
	return c
}

// moment is s, a day (2026-10-23) or a moment (RFC 3339), as a cell: a day
// as itself, a moment as the reader's clock shows it.
func (e *TableExport) moment(s string) (contentIOBusiness.Cell, bool) {
	if d, err := time.Parse(time.DateOnly, s); err == nil {
		return contentIOBusiness.DayCell(d), true
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return contentIOBusiness.Cell{}, false
	}
	at := t.In(e.zone)
	return contentIOBusiness.DayCell(time.Date(at.Year(), at.Month(), at.Day(), at.Hour(), at.Minute(), at.Second(), 0, time.UTC)), true
}

// listText is a list cell, or a link alone, as one line, as the CSV export
// writes it: each item's name in turn, and the items the reader can't open,
// or that come without a name, as one phrase, never by their ids.
func listText(items []any) string {
	var names []string
	hidden := 0
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			name := fieldTypes.RefName(m)
			switch {
			case fieldTypes.IsHiddenRef(m) || (name == "" && m["id"] != nil):
				hidden++
			case name != "":
				names = append(names, name)
			}
			continue
		}
		names = append(names, fieldTypes.Labels(it)...)
	}
	switch {
	case hidden == 1:
		names = append(names, "Something you can't open")
	case hidden > 1:
		names = append(names, helpers.Thousands(int64(hidden))+" things you can't open")
	}
	return strings.Join(names, ", ")
}
