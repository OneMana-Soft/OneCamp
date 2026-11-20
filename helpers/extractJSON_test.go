package helpers

import (
	"encoding/json"
	"testing"
)

// extractJSONCases are the replies a model gives when it is asked for JSON:
// the value alone, fenced, with prose around it, with a second value after it,
// or with none at all. want is the first complete value of the kind asked for.
var extractJSONCases = []struct {
	name  string
	reply string
	kind  byte
	want  string
}{
	{"the object alone", `{"name":"Ops"}`, '{', `{"name":"Ops"}`},
	{"two objects", `{"name":"Ops"} and another: {"name":"Sales"}`, '{', `{"name":"Ops"}`},
	{"prose with braces after the object", "Here it is:\n{\"name\":\"Ops\"}\nUse {name} to refer to it.", '{', `{"name":"Ops"}`},
	{"braces inside a string", `{"note":"use } and { freely","n":1}`, '{', `{"note":"use } and { freely","n":1}`},
	{"an escaped quote", `{"say":"she said \"} hi\"","n":2} trailing }`, '{', `{"say":"she said \"} hi\"","n":2}`},
	{"a fenced block", "```json\n{\"tasks\":[{\"title\":\"Ship\"}]}\n```", '{', `{"tasks":[{"title":"Ship"}]}`},
	{"an array", `Facts: [{"fact":"likes tea"},{"fact":"in Pune"}] done [x]`, '[', `[{"fact":"likes tea"},{"fact":"in Pune"}]`},
	{"brackets in prose before the array", `Items [see below]: ["a","b"]`, '[', `["a","b"]`},
	{"no JSON at all", "I could not find anything to add.", '{', ""},
	{"an object never closed", `{"name":"Ops"`, '{', ""},
	{"an empty reply", "", '[', ""},
	{"a reply cut off at max tokens", `{"name":"Ops","steps":[{"type":"post","text":"hi"},{"type":"wait"`, '{', ""},
	{"a value inside one that stops being JSON", `{"steps":[{"type":"post"}, oops`, '{', ""},
	{"a malformed tool call", `create_task{"title": "Fix "login" bug", "project": {"id": "p1"}}>`, '{', ""},
	{"single-quoted keys around an object", `{'query': 'x', 'filter': {"k": "v"}}`, '{', ""},
	{"a prose brace that never closes", `{ is a brace. Here: {"a":1}`, '{', `{"a":1}`},
}

// extractJSONUnderTest is the extraction the table runs against.
var extractJSONUnderTest = ExtractJSON

func TestExtractJSONTakesTheFirstCompleteValue(t *testing.T) {
	for _, c := range extractJSONCases {
		got := extractJSONUnderTest(c.reply, c.kind)
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
			continue
		}
		if got != "" && !json.Valid([]byte(got)) {
			t.Errorf("%s: %q is not valid JSON", c.name, got)
		}
	}
}
