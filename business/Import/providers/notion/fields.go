package notion

// A database's properties become the project's custom fields when they
// aren't one of the task's own (title, status, priority, assignee and due
// date: schemaOf) and OneCamp has a kind of field for them: select, status,
// multi-select, number (money for a currency format), date, person,
// checkbox, URL, email, phone and text. A multi-select called Tags or
// Labels is the task's tags instead. Formulas, rollups, relations and
// files, which Notion works out or links elsewhere, aren't brought across.

import (
	"sort"
	"strings"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

type notionOption struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

// numberFormats are Notion's currency number formats.
var numberFormats = map[string]string{
	"dollar": "USD", "us_dollar": "USD", "euro": "EUR", "pound": "GBP", "yen": "JPY", "rupee": "INR",
	"won": "KRW", "yuan": "CNY", "real": "BRL", "lira": "TRY", "rupiah": "IDR", "franc": "CHF",
	"hong_kong_dollar": "HKD", "new_zealand_dollar": "NZD", "krona": "SEK", "norwegian_krone": "NOK",
	"mexican_peso": "MXN", "rand": "ZAR", "new_taiwan_dollar": "TWD", "danish_krone": "DKK", "zloty": "PLN",
	"baht": "THB", "forint": "HUF", "koruna": "CZK", "shekel": "ILS", "chilean_peso": "CLP",
	"philippine_peso": "PHP", "dirham": "AED", "colombian_peso": "COP", "riyal": "SAR", "ringgit": "MYR",
	"leu": "RON", "argentine_peso": "ARS", "uruguayan_peso": "UYU", "singapore_dollar": "SGD",
	"canadian_dollar": "CAD", "australian_dollar": "AUD", "peruvian_sol": "PEN", "ruble": "RUB",
}

// isTagsProp is a multi-select that holds the task's tags.
func isTagsProp(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "tags", "tag", "labels", "label":
		return true
	}
	return false
}

// picked reports whether a property is one of the task's own.
func (s *notionDBSchema) picked(name string) bool {
	return name == s.titleProp || name == s.statusProp || name == s.priorityProp || name == s.assigneeProp || name == s.dueProp
}

// buildFields is the database's properties as custom fields, sorted by
// name, and keyed by property name.
func (s *notionDBSchema) buildFields() ([]importProvider.SourceField, map[string]importProvider.SourceField) {
	names := make([]string, 0, len(s.props))
	for n := range s.props {
		names = append(names, n)
	}
	sort.Strings(names)
	var list []importProvider.SourceField
	byName := map[string]importProvider.SourceField{}
	for _, n := range names {
		if s.picked(n) {
			continue
		}
		if f, ok := propField(n, s.props[n]); ok {
			list = append(list, f)
			byName[n] = f
		}
	}
	return list, byName
}

// propField is one property as a custom field, if it can be one.
func propField(name string, p notionDBProp) (importProvider.SourceField, bool) {
	id := p.ID
	if id == "" {
		id = name
	}
	f := importProvider.SourceField{SourceID: id, Name: strings.TrimSpace(name)}
	if f.Name == "" {
		return f, false
	}
	options := func(opts []notionOption) {
		for _, o := range opts {
			f.Options = append(f.Options, importProvider.SourceOption{SourceID: optionKey(o), Label: o.Name, Color: importProvider.PaletteColor(o.Color)})
		}
	}
	switch p.Type {
	case "select":
		f.Type = importProvider.FieldSelect
		if p.Select != nil {
			options(p.Select.Options)
		}
	case "status":
		f.Type = importProvider.FieldSelect
		if p.Status != nil {
			options(p.Status.Options)
		}
	case "multi_select":
		if isTagsProp(name) {
			return f, false
		}
		f.Type = importProvider.FieldMultiSelect
		if p.MultiSelect != nil {
			options(p.MultiSelect.Options)
		}
	case "number":
		f.Type = importProvider.FieldNumber
		if p.Number != nil {
			if cur := numberFormats[p.Number.Format]; cur != "" {
				f.Type, f.Currency = importProvider.FieldMoney, cur
			}
		}
	case "date":
		f.Type = importProvider.FieldDate
	case "people":
		f.Type = importProvider.FieldPerson
	case "checkbox":
		f.Type = importProvider.FieldCheckbox
	case "url":
		f.Type = importProvider.FieldURL
	case "email", "phone_number", "rich_text":
		f.Type = importProvider.FieldText
	default:
		return f, false
	}
	return f, true
}

// optionKey is an option's id, or its name on a schema without ids.
func optionKey(o notionOption) string {
	if o.ID != "" {
		return o.ID
	}
	return strings.ToLower(strings.TrimSpace(o.Name))
}

// fieldValues is a page's values of its database's fields.
func fieldValues(props map[string]notionPageProperty, byName map[string]importProvider.SourceField) []importProvider.SourceFieldValue {
	names := make([]string, 0, len(props))
	for n := range props {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []importProvider.SourceFieldValue
	for _, n := range names {
		f, ok := byName[n]
		if !ok {
			continue
		}
		if v := propValue(props[n]); v != nil {
			out = append(out, importProvider.SourceFieldValue{Field: f, Value: v, Text: peopleNames(props[n])})
		}
	}
	return out
}

// peopleNames is the people a people property names, for when they
// themselves can't come across.
func peopleNames(p notionPageProperty) string {
	var names []string
	for _, person := range p.People {
		if person.Name != "" {
			names = append(names, person.Name)
		}
	}
	return strings.Join(names, ", ")
}

func propValue(p notionPageProperty) any {
	switch p.Type {
	case "select":
		if p.Select != nil && p.Select.Name != "" {
			return optionKey(*p.Select)
		}
	case "status":
		if p.Status != nil && p.Status.Name != "" {
			return optionKey(*p.Status)
		}
	case "multi_select":
		var ids []string
		for _, o := range p.MultiSelect {
			ids = append(ids, optionKey(o))
		}
		if len(ids) > 0 {
			return ids
		}
	case "number":
		if p.Number != nil {
			return *p.Number
		}
	case "date":
		if p.Date != nil && len(p.Date.Start) >= 10 {
			// "2026-10-08" or "2026-10-08T09:00:00.000+05:30": the day is
			// the page's own, before the time.
			return p.Date.Start[:10]
		}
	case "people":
		if len(p.People) > 0 && p.People[0].ID != "" {
			return p.People[0].ID
		}
	case "checkbox":
		if p.Checkbox {
			return true
		}
	case "url":
		if p.URL != nil && strings.TrimSpace(*p.URL) != "" {
			return *p.URL
		}
	case "email":
		if p.Email != nil && strings.TrimSpace(*p.Email) != "" {
			return *p.Email
		}
	case "phone_number":
		if p.Phone != nil && strings.TrimSpace(*p.Phone) != "" {
			return *p.Phone
		}
	case "rich_text":
		if t := strings.TrimSpace(renderRichTextPlain(p.RichText)); t != "" {
			return t
		}
	}
	return nil
}
