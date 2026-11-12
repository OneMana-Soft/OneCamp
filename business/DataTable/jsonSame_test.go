package business

import (
	"reflect"
	"testing"
)

func TestChangedCells(t *testing.T) {
	was := `{"a": "x", "b": [1, 2], "c": {"k": 1}, "gone": true, "_ai_kept": ["z"]}`
	is := `{"a":"x","b":[1,2],"c":{"k":2},"new":null}`
	got := changedCells(was, is)
	want := map[string]bool{"c": true, "gone": true, "new": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("changed %v, want %v", got, want)
	}
	if got := changedCells("{}", `{"a": "x"}`); !reflect.DeepEqual(got, map[string]bool{"a": true}) {
		t.Errorf("a new row's cells: %v", got)
	}
	// A row with a person's AI cell is written again as its mark is lifted
	// out, with "&" escaped: the same cells, not changed ones.
	if got := changedCells(`{"url": "https://x.io/?a=1&b=2", "n": 1.50, "_ai_kept": ["f"]}`, `{"n":1.50,"url":"https://x.io/?a=1\u0026b=2"}`); len(got) != 0 {
		t.Errorf("the same cells written again read as changed: %v", got)
	}
}
