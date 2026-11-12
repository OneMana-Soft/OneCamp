package models

import (
	"encoding/json"
	"testing"
)

// UX-PROJECTS-H1-BE: one way to take predicates off a task (a bulk change's
// and a single write's alike). A delete naming only the node would take the
// node itself, so one that names no predicate is no delete at all.
func TestClearingTakesPredicatesNeverTheNode(t *testing.T) {
	b, err := json.Marshal(Clearing("0x2a", []string{"task_priority"}))
	if err != nil || string(b) != `{"task_priority":null,"uid":"0x2a"}` {
		t.Fatalf("%s %v", b, err)
	}
	for _, none := range [][]string{nil, {}, {""}} {
		if d := Clearing("uid(task)", none); d != nil {
			t.Errorf("%q: %v, want no delete", none, d)
		}
	}
}
