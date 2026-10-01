package ai

import (
	"encoding/json"
	"strings"
	"testing"
)

func patch(t *testing.T, doc, p string) (string, error) {
	t.Helper()
	out, err := ApplyJSONPatch(json.RawMessage(doc), json.RawMessage(p))
	return string(out), err
}

// The six operations, each doing what RFC 6902 says.
func TestJSONPatchAppliesEveryOperation(t *testing.T) {
	for _, tc := range []struct{ name, doc, patch, want string }{
		{"add a member", `{"a":1}`, `[{"op":"add","path":"/b","value":2}]`, `{"a":1,"b":2}`},
		{"add replaces an existing member", `{"a":1}`, `[{"op":"add","path":"/a","value":9}]`, `{"a":9}`},
		{"replace a member", `{"a":1}`, `[{"op":"replace","path":"/a","value":2}]`, `{"a":2}`},
		{"remove a member", `{"a":1,"b":2}`, `[{"op":"remove","path":"/a"}]`, `{"b":2}`},
		{"move", `{"a":1,"b":{}}`, `[{"op":"move","from":"/a","path":"/b/c"}]`, `{"b":{"c":1}}`},
		{"copy", `{"a":1}`, `[{"op":"copy","from":"/a","path":"/b"}]`, `{"a":1,"b":1}`},
		{"test passes and changes nothing", `{"a":[1,2]}`, `[{"op":"test","path":"/a","value":[1,2]}]`, `{"a":[1,2]}`},
		{"the whole document", `{"a":1}`, `[{"op":"replace","path":"","value":{"b":2}}]`, `{"b":2}`},
		{"several in order", `{"n":0}`, `[{"op":"replace","path":"/n","value":1},{"op":"add","path":"/m","value":2}]`, `{"m":2,"n":1}`},
		{"an empty document is an empty object", ``, `[{"op":"add","path":"/a","value":1}]`, `{"a":1}`},
		{"a null value is a value", `{}`, `[{"op":"add","path":"/a","value":null}]`, `{"a":null}`},
		{"an omitted value is null", `{}`, `[{"op":"add","path":"/a"}]`, `{"a":null}`},
		{"no operations", `{"a":1}`, `[]`, `{"a":1}`},
	} {
		got, err := patch(t, tc.doc, tc.patch)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

// Arrays are where a patch implementation usually goes wrong: an add splices
// rather than overwrites, "-" means the end, and a rebuilt slice has to be put
// back where it came from or the change is silently lost.
func TestJSONPatchSplicesArraysAndPutsThemBack(t *testing.T) {
	for _, tc := range []struct{ name, doc, patch, want string }{
		{"add at an index splices", `{"a":[1,3]}`, `[{"op":"add","path":"/a/1","value":2}]`, `{"a":[1,2,3]}`},
		{"add at the end", `{"a":[1]}`, `[{"op":"add","path":"/a/-","value":2}]`, `{"a":[1,2]}`},
		{"add at the length appends", `{"a":[1]}`, `[{"op":"add","path":"/a/1","value":2}]`, `{"a":[1,2]}`},
		{"replace at an index overwrites", `{"a":[1,2]}`, `[{"op":"replace","path":"/a/0","value":9}]`, `{"a":[9,2]}`},
		{"remove closes the gap", `{"a":[1,2,3]}`, `[{"op":"remove","path":"/a/1"}]`, `{"a":[1,3]}`},
		{"nested deep", `{"a":{"b":[{"c":[1]}]}}`, `[{"op":"add","path":"/a/b/0/c/-","value":2}]`, `{"a":{"b":[{"c":[1,2]}]}}`},
		{"a top-level array", `[1,3]`, `[{"op":"add","path":"/1","value":2}]`, `[1,2,3]`},
		{"remove the last element of a top-level array", `[1,2]`, `[{"op":"remove","path":"/1"}]`, `[1]`},
	} {
		got, err := patch(t, tc.doc, tc.patch)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

// An escaped pointer addresses a key that contains a slash or a tilde. Decoded
// in the wrong order, "~01" turns into "/" instead of "~1".
func TestJSONPatchDecodesEscapedPointers(t *testing.T) {
	for _, tc := range []struct{ name, doc, patch, want string }{
		{"a slash in a key", `{"a/b":1}`, `[{"op":"replace","path":"/a~1b","value":2}]`, `{"a/b":2}`},
		{"a tilde in a key", `{"a~b":1}`, `[{"op":"replace","path":"/a~0b","value":2}]`, `{"a~b":2}`},
		{"a tilde followed by a one", `{"a~1b":1}`, `[{"op":"replace","path":"/a~01b","value":2}]`, `{"a~1b":2}`},
		{"an empty key", `{"":1}`, `[{"op":"replace","path":"/","value":2}]`, `{"":2}`},
	} {
		got, err := patch(t, tc.doc, tc.patch)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

// Strict on purpose: the document is remote-controlled and the result goes
// back to that same remote, so an operation this cannot apply exactly is an
// error, never a guess.
func TestJSONPatchRefusesWhatItCannotApplyExactly(t *testing.T) {
	for _, tc := range []struct{ name, doc, patch, mustSay string }{
		{"an unknown operation", `{}`, `[{"op":"increment","path":"/a","value":1}]`, "unknown operation"},
		{"replacing what is not there", `{}`, `[{"op":"replace","path":"/a","value":1}]`, "does not exist"},
		{"removing what is not there", `{}`, `[{"op":"remove","path":"/a"}]`, "does not exist"},
		{"a missing parent", `{}`, `[{"op":"add","path":"/a/b","value":1}]`, "parent does not exist"},
		{"a path that is not a pointer", `{}`, `[{"op":"add","path":"a","value":1}]`, "start with /"},
		{"an index past the end", `{"a":[1]}`, `[{"op":"replace","path":"/a/5","value":1}]`, "past the end"},
		{"a non-numeric index", `{"a":[1]}`, `[{"op":"replace","path":"/a/x","value":1}]`, "not an array index"},
		{"a leading zero index", `{"a":[1,2]}`, `[{"op":"replace","path":"/a/01","value":1}]`, "not an array index"},
		{"- where nothing is added", `{"a":[1]}`, `[{"op":"remove","path":"/a/-"}]`, "can only be added to"},
		{"a failing test", `{"a":1}`, `[{"op":"test","path":"/a","value":2}]`, "does not match"},
		{"moving a value into itself", `{"a":{"b":1}}`, `[{"op":"move","from":"/a","path":"/a/c"}]`, "into itself"},
		{"a from that is not there", `{}`, `[{"op":"copy","from":"/a","path":"/b"}]`, "does not exist"},
		{"removing the whole document", `{}`, `[{"op":"remove","path":""}]`, "cannot be removed"},
		{"a patch that is not an array", `{}`, `{"op":"add"}`, "not a JSON Patch array"},
		{"a document that is not JSON", `{`, `[]`, "not JSON"},
	} {
		got, err := patch(t, tc.doc, tc.patch)
		if err == nil {
			t.Errorf("%s: applied instead of failing, giving %s", tc.name, got)
			continue
		}
		if !strings.Contains(err.Error(), tc.mustSay) {
			t.Errorf("%s: error %q does not say %q", tc.name, err, tc.mustSay)
		}
	}
}

// An operation that fails must not leave half a change behind: the caller
// drops the state on an error, and a document mutated on the way to that error
// would be a third thing, neither the old state nor the new one.
func TestJSONPatchLeavesTheDocumentAloneWhenItFails(t *testing.T) {
	doc := json.RawMessage(`{"a":1,"list":[1,2]}`)
	_, err := ApplyJSONPatch(doc, json.RawMessage(`[{"op":"add","path":"/b","value":2},{"op":"remove","path":"/nope"}]`))
	if err == nil {
		t.Fatal("expected the second operation to fail")
	}
	if string(doc) != `{"a":1,"list":[1,2]}` {
		t.Errorf("the caller's document was mutated: %s", doc)
	}
}

// A peer cannot make this work without limit.
func TestJSONPatchBoundsTheNumberOfOperations(t *testing.T) {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i <= maxPatchOps; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"op":"add","path":"/a","value":1}`)
	}
	b.WriteString("]")
	if _, err := ApplyJSONPatch(json.RawMessage(`{}`), json.RawMessage(b.String())); err == nil ||
		!strings.Contains(err.Error(), "more than the") {
		t.Errorf("want a bound error, got %v", err)
	}
}

// The error names which operation failed, because a patch is a list and "it
// did not apply" is not enough to fix one.
func TestJSONPatchErrorsNameTheOperation(t *testing.T) {
	_, err := ApplyJSONPatch(json.RawMessage(`{}`), json.RawMessage(`[{"op":"add","path":"/a","value":1},{"op":"replace","path":"/missing","value":2}]`))
	if err == nil || !strings.Contains(err.Error(), "operation 1 (replace /missing)") {
		t.Errorf("error = %v", err)
	}
}
