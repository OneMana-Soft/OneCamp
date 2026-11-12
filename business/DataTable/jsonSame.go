package business

// Comparing JSON as values, for any writer: a row's cells before and after a
// write, a field's default or config before and after a schema change. Kept
// out of the AI columns' file because both editions use it (V-548).

import (
	"bytes"
	"encoding/json"
	"reflect"

	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
)

// changedCells is the cells, by field id, that differ between a row's values
// as they were (was) and are (is); the AI columns' marks (AIKeptKey) aren't a
// cell.
func changedCells(was, is string) map[string]bool {
	var before, after map[string]json.RawMessage
	_ = json.Unmarshal([]byte(was), &before)
	_ = json.Unmarshal([]byte(is), &after)
	changed := map[string]bool{}
	for id, v := range after {
		if !sameJSON(before[id], v) {
			changed[id] = true
		}
	}
	for id := range before {
		if _, kept := after[id]; !kept {
			changed[id] = true
		}
	}
	delete(changed, model.AIKeptKey)
	return changed
}

// sameJSON is whether two JSON values are the same value, however they're
// written: spacing, key order and escapes aside. A row with a person's AI
// cells has its values written again as its marks are lifted out
// (model.liftAIKept), with "&" as "\u0026": compared as written, every such
// cell read as changed, and asked the row's AI columns again.
func sameJSON(a, b json.RawMessage) bool {
	if a == nil || b == nil {
		return bytes.Equal(a, b)
	}
	x, xerr := decodeJSON(a)
	y, yerr := decodeJSON(b)
	if xerr != nil || yerr != nil {
		return bytes.Equal(a, b)
	}
	return reflect.DeepEqual(x, y)
}

// decodeJSON is a JSON value decoded, its numbers as written.
func decodeJSON(b json.RawMessage) (interface{}, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v interface{}
	err := d.Decode(&v)
	return v, err
}
