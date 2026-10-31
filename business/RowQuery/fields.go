package business

// What a spec's field names in a table (tables A3): a stored cell of one of
// Field Types' families (#60), a column of the row itself (its number, when
// it was made and changed, and by whom), a link to another table's rows, or
// a value worked out on each read (a formula, a rollup), which the SQL can't
// see until A4 stores it.

import (
	"encoding/json"
	"fmt"
	"strings"

	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// family is how a field's cells compare and sort.
type family int

const (
	famText family = iota
	famNumber
	famDate
	famCheckbox
	famChoice
	famMulti
	famPeople
	famRefs
	famRowLink
	famComputed
)

// rowColumn is a field read from the row itself rather than its values.
type rowColumn string

const (
	colNone      rowColumn = ""
	colRowNo     rowColumn = "row_no"
	colCreatedAt rowColumn = "created_at"
	colUpdatedAt rowColumn = "updated_at"
	colCreatedBy rowColumn = "created_by"
	colUpdatedBy rowColumn = "updated_by"
)

// column is one field of the table as a query reads it.
type column struct {
	field  *model.Field
	key    string // its id: the key of its cell in values
	family family
	col    rowColumn
	// options are a choice's labels in their order (sorting by them), and
	// labelOf a label by option id; group the labels in each status group.
	options []string
	labelOf map[string]string
	group   map[string][]string
	// link is a link to a table's rows: the field whose links it reads, and
	// whether it reads them backwards (the other side shows them).
	linkField uuid.UUID
	reverse   bool
}

// columns indexes a table's fields as "field:<id>", and by id alone.
func columns(fields []*model.Field) map[string]*column {
	out := make(map[string]*column, 2*len(fields))
	for _, f := range fields {
		c := columnOf(f)
		out["field:"+c.key] = c
		out[c.key] = c
	}
	return out
}

func columnOf(f *model.Field) *column {
	c := &column{field: f, key: f.Id.String()}
	var cfg struct {
		Options []struct {
			ID    string `json:"id"`
			Label string `json:"label"`
			Group string `json:"group"`
		} `json:"options"`
		Target  string `json:"relation_target"`
		Inverse string `json:"inverse_of"`
	}
	_ = json.Unmarshal([]byte(f.Config), &cfg)
	switch f.Type {
	case model.FieldNumber, model.FieldDuration, model.FieldRating:
		c.family = famNumber
	case model.FieldAutonumber:
		c.family, c.col = famNumber, colRowNo
	case model.FieldDate:
		c.family = famDate
	case model.FieldCreatedTime:
		c.family, c.col = famDate, colCreatedAt
	case model.FieldModifiedTime:
		c.family, c.col = famDate, colUpdatedAt
	case model.FieldCheckbox:
		c.family = famCheckbox
	case model.FieldSelect, model.FieldStatus:
		c.family = famChoice
	case model.FieldMultiSelect:
		c.family = famMulti
	case model.FieldPeople:
		c.family = famPeople
	case model.FieldAttachment:
		c.family = famMulti // its files by name (labelsOf)
	case model.FieldCreatedBy:
		c.family, c.col = famPeople, colCreatedBy
	case model.FieldModifiedBy:
		c.family, c.col = famPeople, colUpdatedBy
	case model.FieldRelation:
		c.family = famRefs
		if cfg.Target == "table" {
			c.family, c.linkField = famRowLink, f.Id
			if id, err := uuid.Parse(cfg.Inverse); err == nil {
				c.linkField, c.reverse = id, true
			}
		}
	case model.FieldFormula, model.FieldRollup:
		c.family = famComputed
	default: // text, long text, url, email, phone, the old person field
		c.family = famText
	}
	if c.family == famChoice || c.family == famMulti {
		c.labelOf = map[string]string{}
		c.group = map[string][]string{}
		for _, o := range cfg.Options {
			c.options = append(c.options, o.Label)
			if o.ID != "" {
				c.labelOf[o.ID] = o.Label
			}
			if o.Group != "" {
				c.group[o.Group] = append(c.group[o.Group], o.Label)
			}
		}
	}
	return c
}

// errNoField is a spec naming a field the table hasn't got.
func errNoField(ref string) error {
	return fmt.Errorf("there's no field %q in this table", strings.TrimPrefix(ref, "field:"))
}
