package helpers

import (
	"encoding/json"
	"reflect"
)

// EmptyListsInResponse is data with every nil list in its envelopes answered as
// an empty one: a nil slice as [] and a nil map as {}, never null (V-52).
//
// A client reads null for "not read yet": V-51's project with no milestones drew
// its loading rows for ever, because Milestone.List answered a nil slice. About a
// hundred answers build their lists the same way (rows appended to a nil slice, a
// reader's "nothing" as nil). WriteJSON runs every answer through this, so the
// class is closed for every handler at once, including ones not yet written.
//
// Like RedactErrorsInResponse it walks map envelopes only: Envolope and
// map[string]interface{}, nested ones too. Left as they are: a struct's own fields
// (it marshals itself), []byte and json.RawMessage, nil pointers, and an untyped
// nil, which an answer uses for "none" of one thing ("data": nil). The input is
// never changed: a map is copied only when one of its values is.
func EmptyListsInResponse(data interface{}) interface{} {
	switch typed := data.(type) {
	case Envolope:
		if out, changed := emptyListsInMap(typed); changed {
			return Envolope(out)
		}
	case map[string]interface{}:
		if out, changed := emptyListsInMap(typed); changed {
			return out
		}
	}
	return data
}

var rawMessageType = reflect.TypeOf(json.RawMessage(nil))

// emptyListsInMap is m with its nil lists made empty, and whether any was; m
// itself when none was.
func emptyListsInMap(m map[string]interface{}) (map[string]interface{}, bool) {
	var out map[string]interface{}
	for key, value := range m {
		next, changed := emptyList(value)
		if !changed {
			continue
		}
		if out == nil {
			out = make(map[string]interface{}, len(m))
			for k, v := range m {
				out[k] = v
			}
		}
		out[key] = next
	}
	if out == nil {
		return m, false
	}
	return out, true
}

// emptyList is v as it should go on the wire, and whether that differs from v.
func emptyList(v interface{}) (interface{}, bool) {
	switch inner := v.(type) {
	case nil:
		return v, false
	case Envolope:
		if out, changed := emptyListsInMap(inner); changed {
			return Envolope(out), true
		}
		return v, false
	case map[string]interface{}:
		if out, changed := emptyListsInMap(inner); changed {
			return out, true
		}
		return v, false
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice:
		if !rv.IsNil() || rv.Type() == rawMessageType || rv.Type().Elem().Kind() == reflect.Uint8 {
			return v, false
		}
		return reflect.MakeSlice(rv.Type(), 0, 0).Interface(), true
	case reflect.Map:
		if !rv.IsNil() {
			return v, false
		}
		return reflect.MakeMap(rv.Type()).Interface(), true
	}
	return v, false
}

// NonNil is s, or an empty slice of its type when s is nil: a list that goes on
// the wire or into a Postgres array as [] or '{}', never null.
func NonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
