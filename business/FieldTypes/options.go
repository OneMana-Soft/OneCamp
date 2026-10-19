package business

// A select's or a multi-select's options, as its config keeps them: each with
// an id it keeps through renames and moves, a label (unique in the field) and
// a colour. A cell holds its option's label. An option stored without an id
// (every option saved before ids) is known by its label as "label:<label>",
// here and on the web (services/tableService.ts legacyOptionId); the one-time
// job in business/DataTable stores those ids, which never change again.

import (
	"encoding/json"
	"strings"

	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// StoredOptionID is how an option stored without an id is known. Pure.
func StoredOptionID(label string) string { return "label:" + label }

// NewOptionID is an id for an option that has none and whose label's id is
// taken, made as the web makes one ("o_" and ten characters).
func NewOptionID() string {
	return "o_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
}

// Options is a field config's options, each as a map, in order: one given as a
// bare string is the option of that label. A typed list, as Go callers build
// one (the AI draft's), is read as JSON reads it. Pure.
func Options(cfg map[string]any) []map[string]any {
	list, ok := cfg["options"].([]any)
	if !ok && cfg["options"] != nil {
		if b, err := json.Marshal(cfg["options"]); err == nil {
			_ = json.Unmarshal(b, &list)
		}
	}
	out := make([]map[string]any, 0, len(list))
	for _, it := range list {
		switch o := it.(type) {
		case map[string]any:
			out = append(out, o)
		case string:
			out = append(out, map[string]any{"label": o})
		}
	}
	return out
}

// OptionLabel is an option's label. Pure.
func OptionLabel(o map[string]any) string {
	s, _ := o["label"].(string)
	return s
}

// OptionID is the id an option was stored or sent with ("" for none). Pure.
func OptionID(o map[string]any) string {
	s, _ := o["id"].(string)
	return strings.TrimSpace(s)
}

// Option is one of a field's options, as a cell is checked against it.
type Option struct {
	ID    string
	Label string
}

// OptionsOf is a select field's options, each with its id, the derived one
// for an option stored without one.
func OptionsOf(f *model.Field) []Option {
	cfg := map[string]any{}
	_ = json.Unmarshal([]byte(f.Config), &cfg)
	opts := Options(cfg)
	out := make([]Option, 0, len(opts))
	for _, o := range opts {
		label := OptionLabel(o)
		id := OptionID(o)
		if id == "" {
			id = StoredOptionID(label)
		}
		out = append(out, Option{ID: id, Label: label})
	}
	return out
}
