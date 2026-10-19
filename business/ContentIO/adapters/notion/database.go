// Package notion reads Notion into OneCamp: its export zips (export.go) into
// docs, and its databases into tables (database.go). The provider that walks
// Notion's API (business/Import/providers/notion pages.go) makes its child
// databases into tables through the same TableFromDatabase.
package notion

import (
	"context"
	"strings"

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	rowLedger "github.com/akashc777/OneCamp/business/RowLedger"
	"github.com/akashc777/OneCamp/helpers"
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
// column per property, of the kind nearest Notion's, then its rows, and says
// how many rows couldn't be added. A property no column kind holds (a formula,
// a relation, a person) comes across as its text. It is the one way a Notion
// database becomes a table: the API import, and the tables and projects
// programmes' importers, call it.
func TableFromDatabase(ctx context.Context, actor dataTableBusiness.Actor, db Database) (table *dataTableModels.DataTable, rowsLeftOut int, err error) {
	actor.Kind = rowLedger.ActorImport
	name := strings.TrimSpace(db.Title)
	if name == "" {
		name = "Untitled database"
	}
	if r := []rune(name); len(r) > 120 {
		name = string(r[:120])
	}
	visibility := dataTableModels.VisibilityWorkspace
	if db.Private {
		visibility = dataTableModels.VisibilityPrivate
	}
	fields := make([]dataTableBusiness.FieldInput, 0, len(db.Columns))
	for i, c := range db.Columns {
		f := dataTableBusiness.FieldInput{Name: c.Name, Type: columnKind(c.Type), Position: float64(i)}
		if f.Type == dataTableModels.FieldSelect || f.Type == dataTableModels.FieldMultiSelect {
			opts := make([]map[string]any, 0, len(c.Options))
			for _, o := range c.Options {
				opts = append(opts, map[string]any{"label": o})
			}
			f.Config = map[string]any{"options": opts}
		}
		fields = append(fields, f)
	}
	icon := db.Icon
	if len(icon) > 64 {
		icon = ""
	}
	table, err = dataTableBusiness.CreateTableFromTemplate(ctx, dataTableBusiness.TableInput{Name: name, Icon: icon, Visibility: visibility}, fields, nil, actor)
	if err != nil {
		return nil, 0, err
	}
	made, err := dataTableBusiness.ListTableFields(ctx, table.Id, actor)
	if err != nil {
		return table, len(db.Rows), err
	}
	byName := make(map[string]*dataTableModels.Field, len(made))
	for _, f := range made {
		byName[f.Name] = f
	}
	for i, row := range db.Rows {
		values := map[string]any{}
		for col, v := range row {
			if f := byName[col]; f != nil {
				if cell := cellValue(f.Type, v); cell != nil {
					values[f.Id.String()] = cell
				}
			}
		}
		at := float64(i)
		if _, err := dataTableBusiness.CreateRow(ctx, table.Id, dataTableBusiness.RowInput{Values: values, Position: &at}, actor); err != nil {
			helpers.LogWarnWithContext(ctx, "notion: a row of %q could not be added: %v", name, err)
			rowsLeftOut++
		}
	}
	return table, rowsLeftOut, nil
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
