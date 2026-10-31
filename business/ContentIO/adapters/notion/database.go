// Package notion reads Notion into OneCamp: its export zips (export.go) into
// docs, and its databases into tables (database.go). The provider that walks
// Notion's API (business/Import/providers/notion pages.go) makes its child
// databases into tables through the same TableFromDatabase.
package notion

import (
	"context"
	"fmt"
	"strings"

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	dataTableModels "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

// Database is a Notion database as it is brought across: its title and icon,
// its properties, and its rows' values by property name.
type Database struct {
	Title string
	// Icon is the database's emoji, if it has one.
	Icon    string
	Columns []Column
	// Rows hold each row's values by column name: a string, a float64, a bool,
	// or a []string for a multi-select.
	Rows []map[string]any
	// Private makes the table visible to its maker and admins only, as a
	// database under a private page should be.
	Private bool
}

// Column is one property of a database.
type Column struct {
	Name string
	// Type is Notion's property type: "title", "select", "number" ...
	Type string
	// Options are a select's or a multi-select's choices, by name.
	Options []string
}

// TableFromDatabase makes a Notion database into a table of the person's: a
// column per property, of the kind nearest Notion's, then its rows. A
// property no column kind holds (a formula, a relation, a person) comes
// across as its text. It is the API import's way, and goes through the one
// importer of every source's tables (dataTableBusiness.ImportBaseAs), so a
// row is never lost for a cell no column kind takes: the row comes across
// without it, and the notes say which value of which field was left out, for
// the import's report.
func TableFromDatabase(ctx context.Context, actor dataTableBusiness.Actor, db Database) (table *dataTableModels.DataTable, notes []dataTableBusiness.ImportNote, err error) {
	t := dataTableBusiness.ImportTable{Key: "database", Name: db.Title, Icon: db.Icon}
	if strings.TrimSpace(t.Name) == "" {
		t.Name = "Untitled database"
	}
	kinds := map[string]string{}
	for _, c := range db.Columns {
		f := dataTableBusiness.ImportField{Key: c.Name, Name: c.Name, Type: columnKind(c.Type)}
		if f.Type == dataTableModels.FieldSelect || f.Type == dataTableModels.FieldMultiSelect {
			opts := make([]map[string]any, 0, len(c.Options))
			for _, o := range c.Options {
				opts = append(opts, map[string]any{"label": o})
			}
			f.Config = map[string]any{"options": opts}
		}
		kinds[c.Name] = f.Type
		t.Fields = append(t.Fields, f)
	}
	rows := make([]dataTableBusiness.ImportRow, len(db.Rows))
	for i, row := range db.Rows {
		cells := map[string]any{}
		for col, v := range row {
			if cell := cellValue(kinds[col], v); cell != nil {
				cells[col] = cell
			}
		}
		rows[i] = dataTableBusiness.ImportRow{Key: fmt.Sprint(i), Cells: cells}
	}
	t.Rows = func(page func([]dataTableBusiness.ImportRow) error) error { return page(rows) }
	made, err := dataTableBusiness.ImportBaseAs(ctx, actor, dataTableBusiness.ImportBase{Private: db.Private, Tables: []dataTableBusiness.ImportTable{t}})
	if err != nil {
		return nil, nil, err
	}
	table, err = dataTableModels.GetTableByID(ctx, made.Tables[t.Key])
	return table, made.Notes, err
}

// columnKind is the column kind nearest a Notion property type.
func columnKind(notionType string) string {
	switch notionType {
	case "number":
		return dataTableModels.FieldNumber
	case "select", "status":
		return dataTableModels.FieldSelect
	case "multi_select":
		return dataTableModels.FieldMultiSelect
	case "date", "created_time", "last_edited_time":
		return dataTableModels.FieldDate
	case "checkbox":
		return dataTableModels.FieldCheckbox
	case "url":
		return dataTableModels.FieldURL
	case "email":
		return dataTableModels.FieldEmail
	}
	return dataTableModels.FieldText
}

// cellValue is a value as a column of kind keeps it; nil for none.
func cellValue(kind string, v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		if strings.TrimSpace(x) == "" {
			return nil
		}
		if kind == dataTableModels.FieldMultiSelect {
			return []string{x}
		}
		return x
	case []string:
		if len(x) == 0 {
			return nil
		}
		if kind == dataTableModels.FieldMultiSelect {
			return x
		}
		return strings.Join(x, ", ")
	}
	return v
}
