package scene

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// yjsState is one of testdata/yjs_states.json: a state real Yjs wrote
// (yjs_states.mjs) and the root maps a Y.Doc that applied it shows, or
// refuse for an update that isn't a whole state.
type yjsState struct {
	Name   string                    `json:"name"`
	State  string                    `json:"state"`
	Want   map[string]map[string]any `json:"want"`
	Refuse bool                      `json:"refuse"`
	bytes  []byte
}

func yjsStates(t *testing.T) []yjsState {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "yjs_states.json"))
	if err != nil {
		t.Fatal(err)
	}
	var states []yjsState
	if err := json.Unmarshal(raw, &states); err != nil {
		t.Fatal(err)
	}
	for i := range states {
		if states[i].bytes, err = base64.StdEncoding.DecodeString(states[i].State); err != nil {
			t.Fatal(err)
		}
	}
	return states
}

// sameJSON compares as JSON: Yjs's numbers and Maps's float64 alike.
func sameJSON(t *testing.T, a, b any) bool {
	t.Helper()
	norm := func(v any) any {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var out any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	return reflect.DeepEqual(norm(a), norm(b))
}

func TestMapsShowsWhatAYDocShows(t *testing.T) {
	read := 0
	for _, s := range yjsStates(t) {
		if s.Refuse {
			continue
		}
		got, err := Maps(s.bytes)
		if err != nil {
			t.Fatalf("%s: %v", s.Name, err)
		}
		for root, want := range s.Want {
			if len(want) == 0 && len(got[root]) == 0 {
				continue
			}
			if !sameJSON(t, got[root], want) {
				g, _ := json.Marshal(got[root])
				w, _ := json.Marshal(want)
				t.Errorf("%s, %s:\n got %.600s\nwant %.600s", s.Name, root, g, w)
			}
		}
		read++
	}
	if read < 6 {
		t.Fatalf("read %d of the states, wanted every one", read)
	}
}

func TestMapsRefusesWhatIsntAWholeState(t *testing.T) {
	var small []byte
	for _, s := range yjsStates(t) {
		if s.Refuse {
			if _, err := Maps(s.bytes); err == nil {
				t.Errorf("%s was read", s.Name)
			}
		}
		if s.Name == "values" {
			small = s.bytes
		}
	}
	for i := 0; i < len(small); i++ {
		if _, err := Maps(small[:i]); err == nil {
			t.Fatalf("a state cut at byte %d of %d was read", i, len(small))
		}
	}
	if _, err := Maps(append(append([]byte{}, small...), 0)); err == nil {
		t.Fatal("bytes after the delete set were read")
	}
}

func TestMapsReadsWhatMapsUpdateWrites(t *testing.T) {
	b, err := MapsUpdate(42, Root{Name: "elements", Entries: []Entry{{"a", map[string]any{"x": 1.5, "tags": []any{"q"}}}, {"b", "two"}}},
		Root{Name: "files", Entries: []Entry{{"f", map[string]any{"objectUuid": "u"}}}})
	if err != nil {
		t.Fatal(err)
	}
	strict, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	general, err := Maps(b)
	if err != nil {
		t.Fatal(err)
	}
	if !sameJSON(t, strict, general) || len(general["elements"]) != 2 || len(general["files"]) != 1 {
		t.Fatalf("Decode %v, Maps %v", strict, general)
	}
}

// Decode stays the check of what MapsUpdate writes: a state screens wrote,
// with several clients or deletions, is refused.
func TestDecodeRefusesAStateScreensWrote(t *testing.T) {
	for _, s := range yjsStates(t) {
		if s.Name != "three screens at once" && s.Name != "overwrites, collected" {
			continue
		}
		if _, err := Decode(s.bytes); err == nil {
			t.Errorf("Decode read %s", s.Name)
		}
	}
}
