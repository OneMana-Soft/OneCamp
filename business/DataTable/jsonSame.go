package business

import (
	"bytes"
	"encoding/json"
	"reflect"
)

// sameJSON is whether two JSON values are the same value, however they're
// written: spacing, key order and escapes aside, so a field's default or
// config written again in another form doesn't read as changed.
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
