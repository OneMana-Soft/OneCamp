package businness

import (
	"reflect"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// The docs lists and the docs home's search name the pages above a row one
// way: at most three, the top first, each named only where the person may
// open it; and a row's owner is its owner, or whoever made it.
func TestAListRowNamesThePagesAboveItOneWay(t *testing.T) {
	top := &dgraphStruct.DgraphDoc{Uuid: "top", Title: "Top", Icon: "T"}
	mid := &dgraphStruct.DgraphDoc{Uuid: "mid", Title: "Mid", Parent: top}
	low := &dgraphStruct.DgraphDoc{Uuid: "low", Title: "Low", Parent: mid}
	under := &dgraphStruct.DgraphDoc{Uuid: "under", Title: "Under", Parent: low}
	row := &dgraphStruct.DgraphDoc{Uuid: "row", Parent: under}

	path := PathAbove(row)
	if len(path) != 3 || path[0] != mid || path[2] != under {
		t.Fatalf("the three nearest pages, the top first: %+v", path)
	}
	open := map[string]bool{"mid": true, "under": true}
	got := CrumbsOf(row, func(id string) bool { return open[id] })
	want := []Crumb{{UUID: "mid", Title: "Mid"}, {Redacted: true}, {UUID: "under", Title: "Under"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("crumbs: got %+v, want %+v", got, want)
	}
	if c := CrumbsOf(top, func(string) bool { return true }); c == nil || len(c) != 0 {
		t.Errorf("a row with nothing above it: %#v, want an empty list", c)
	}

	ana, ben := &dgraphStruct.DgraphUser{Uuid: "ana"}, &dgraphStruct.DgraphUser{Uuid: "ben"}
	if p := OwnerOf(&dgraphStruct.DgraphDoc{Owner: ana, CreatedBy: ben}); p == nil || p.UUID != "ana" {
		t.Errorf("an owned doc names its owner: %+v", p)
	}
	if p := OwnerOf(&dgraphStruct.DgraphDoc{Owner: &dgraphStruct.DgraphUser{}, CreatedBy: ben}); p == nil || p.UUID != "ben" {
		t.Errorf("a doc with no owner recorded names who made it: %+v", p)
	}
}
