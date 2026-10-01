package business

import (
	"reflect"
	"testing"
)

// coerceArg must parse array/object-typed arguments (which cross the tool
// boundary as JSON strings) back into real JSON, so an MCP server receives a
// proper structure — e.g. github push_files' files:[{path,content}] — instead
// of a quoted string. Scalars keep their existing coercion; invalid JSON falls
// back to the raw string so the server validates and the model can retry.
func TestCoerceArg(t *testing.T) {
	filesJSON := `[{"path":"README.md","content":"# Hi"},{"path":"a.txt","content":"x"}]`

	cases := []struct {
		name     string
		val      string
		jsonType string
		want     interface{}
	}{
		{
			name:     "array of objects parsed to real JSON",
			val:      filesJSON,
			jsonType: "array",
			want: []interface{}{
				map[string]interface{}{"path": "README.md", "content": "# Hi"},
				map[string]interface{}{"path": "a.txt", "content": "x"},
			},
		},
		{
			name:     "object parsed to real JSON",
			val:      `{"path":"README.md","content":"hello"}`,
			jsonType: "object",
			want:     map[string]interface{}{"path": "README.md", "content": "hello"},
		},
		{
			name:     "array of strings parsed",
			val:      `["main","dev"]`,
			jsonType: "array",
			want:     []interface{}{"main", "dev"},
		},
		{name: "invalid JSON array falls back to raw string", val: "not json", jsonType: "array", want: "not json"},
		{name: "empty array arg falls back to raw string", val: "", jsonType: "array", want: ""},
		{name: "boolean coerced", val: "true", jsonType: "boolean", want: true},
		{name: "integer coerced", val: "42", jsonType: "integer", want: int64(42)},
		{name: "number coerced", val: "3.14", jsonType: "number", want: 3.14},
		{name: "string passthrough", val: "owner/repo", jsonType: "string", want: "owner/repo"},
		{name: "string type never parses JSON-looking text", val: `["x"]`, jsonType: "string", want: `["x"]`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := coerceArg(tc.val, tc.jsonType)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("coerceArg(%q, %q) = %#v, want %#v", tc.val, tc.jsonType, got, tc.want)
			}
		})
	}
}
