package asana

// Asana's custom fields become the project's own fields: text, number
// (money when it's formatted as a currency), single- and multi-select,
// date and people. A project lists its fields in custom_field_settings;
// each task carries its values in custom_fields. A field called Priority
// is the task's priority (buildSourceTask), not a field of its own;
// formulas and custom ids, worked out by Asana, aren't brought across.

import (
	"context"
	"fmt"
	"strings"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

type asanaEnumOption struct {
	GID     string `json:"gid"`
	Name    string `json:"name"`
	Color   string `json:"color"`
	Enabled *bool  `json:"enabled"`
}

// asanaFieldDef is a custom field as a project's settings list it.
type asanaFieldDef struct {
	GID             string            `json:"gid"`
	Name            string            `json:"name"`
	Type            string            `json:"type"`
	ResourceSubtype string            `json:"resource_subtype"`
	Format          string            `json:"format"`
	CurrencyCode    string            `json:"currency_code"`
	IsFormula       bool              `json:"is_formula_field"`
	EnumOptions     []asanaEnumOption `json:"enum_options"`
}

// fieldOptFields asks a project's settings for what fieldOf needs.
const fieldOptFields = "custom_field.gid,custom_field.name,custom_field.type,custom_field.resource_subtype,custom_field.format," +
	"custom_field.currency_code,custom_field.is_formula_field,custom_field.enum_options.gid,custom_field.enum_options.name," +
	"custom_field.enum_options.color,custom_field.enum_options.enabled"

// valueOptFields asks each task for its fields' values.
const valueOptFields = "custom_fields.gid,custom_fields.name,custom_fields.type,custom_fields.resource_subtype,custom_fields.format," +
	"custom_fields.currency_code,custom_fields.is_formula_field,custom_fields.display_value,custom_fields.text_value,custom_fields.number_value,custom_fields.date_value.date," +
	"custom_fields.enum_value.gid,custom_fields.enum_value.name,custom_fields.enum_value.color," +
	"custom_fields.multi_enum_values.gid,custom_fields.multi_enum_values.name,custom_fields.multi_enum_values.color,custom_fields.people_value.gid"

func isPriority(name string) bool { return strings.EqualFold(strings.TrimSpace(name), "priority") }

// fieldOf is an Asana field as a OneCamp one, if it can be one.
func fieldOf(d asanaFieldDef) (importProvider.SourceField, bool) {
	f := importProvider.SourceField{SourceID: d.GID, Name: strings.TrimSpace(d.Name)}
	// A formula is worked out by Asana from other fields; kept here it would
	// be a number that never changes again.
	if f.SourceID == "" || f.Name == "" || isPriority(f.Name) || d.IsFormula {
		return f, false
	}
	kind := d.ResourceSubtype
	if kind == "" {
		kind = d.Type
	}
	switch kind {
	case "text":
		f.Type = importProvider.FieldText
	case "number":
		f.Type = importProvider.FieldNumber
		if cur := importProvider.CurrencyCode(d.CurrencyCode); d.Format == "currency" && cur != "" {
			f.Type, f.Currency = importProvider.FieldMoney, cur
		}
	case "enum", "multi_enum":
		f.Type = importProvider.FieldSelect
		if kind == "multi_enum" {
			f.Type = importProvider.FieldMultiSelect
		}
		for _, o := range d.EnumOptions {
			if o.GID != "" && (o.Enabled == nil || *o.Enabled) {
				f.Options = append(f.Options, option(o))
			}
		}
	case "date":
		f.Type = importProvider.FieldDate
	case "people":
		f.Type = importProvider.FieldPerson
	default:
		return f, false
	}
	return f, true
}

func option(o asanaEnumOption) importProvider.SourceOption {
	return importProvider.SourceOption{SourceID: o.GID, Label: o.Name, Color: importProvider.PaletteColor(o.Color)}
}

// fieldValues is a task's values of its custom fields. A formula's value,
// worked out by Asana, comes as text for the task's description.
func fieldValues(cfs []asanaCustomField) []importProvider.SourceFieldValue {
	var out []importProvider.SourceFieldValue
	for _, cf := range cfs {
		def := asanaFieldDef{GID: cf.GID, Name: cf.Name, Type: cf.Type, ResourceSubtype: cf.ResourceSubtype, Format: cf.Format, CurrencyCode: cf.CurrencyCode, IsFormula: cf.IsFormula}
		f, ok := fieldOf(def)
		if !ok {
			if (cf.IsFormula || cf.ResourceSubtype == "formula") && strings.TrimSpace(cf.DisplayValue) != "" && !isPriority(cf.Name) {
				out = append(out, importProvider.SourceFieldValue{Field: importProvider.SourceField{SourceID: cf.GID, Name: strings.TrimSpace(cf.Name)}, Text: cf.DisplayValue})
			}
			continue
		}
		var v any
		switch f.Type {
		case importProvider.FieldText:
			if cf.TextValue != nil && strings.TrimSpace(*cf.TextValue) != "" {
				v = *cf.TextValue
			}
		case importProvider.FieldNumber, importProvider.FieldMoney:
			if cf.NumberValue != nil {
				v = *cf.NumberValue
			}
		case importProvider.FieldSelect:
			if cf.EnumValue != nil && cf.EnumValue.GID != "" {
				f.Options = []importProvider.SourceOption{option(*cf.EnumValue)}
				v = cf.EnumValue.GID
			}
		case importProvider.FieldMultiSelect:
			var ids []string
			for _, o := range cf.MultiEnumValues {
				if o.GID != "" {
					f.Options = append(f.Options, option(o))
					ids = append(ids, o.GID)
				}
			}
			if len(ids) > 0 {
				v = ids
			}
		case importProvider.FieldDate:
			if cf.DateValue != nil && len(cf.DateValue.Date) >= 10 {
				v = cf.DateValue.Date[:10]
			}
		case importProvider.FieldPerson:
			if len(cf.PeopleValue) > 0 && cf.PeopleValue[0].GID != "" {
				v = cf.PeopleValue[0].GID
			}
		}
		if v != nil {
			out = append(out, importProvider.SourceFieldValue{Field: f, Value: v, Text: cf.DisplayValue})
		}
	}
	return out
}

// listProjectFields is a project's custom fields, in the project's order.
func (p *Provider) listProjectFields(ctx context.Context, tok, projectGID string) ([]importProvider.SourceField, error) {
	var out []importProvider.SourceField
	path := fmt.Sprintf("/projects/%s/custom_field_settings?limit=100&opt_fields=%s", projectGID, fieldOptFields)
	type setting struct {
		CustomField *asanaFieldDef `json:"custom_field"`
	}
	type settingsResp struct {
		Data     []setting      `json:"data"`
		NextPage *asanaNextPage `json:"next_page"`
	}
	for page := 0; path != "" && page < maxPages; page++ {
		var resp settingsResp
		if err := p.getJSON(ctx, tok, path, &resp); err != nil {
			return out, err
		}
		for _, s := range resp.Data {
			if s.CustomField == nil {
				continue
			}
			if f, ok := fieldOf(*s.CustomField); ok {
				out = append(out, f)
			}
		}
		path = nextPagePath(resp.NextPage)
	}
	return out, nil
}
