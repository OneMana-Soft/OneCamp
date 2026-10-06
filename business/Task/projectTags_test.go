package business

import (
	"reflect"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

func TestCountTags(t *testing.T) {
	l := func(s string) *dgraphStruct.DgraphTask { return &dgraphStruct.DgraphTask{Label: &s} }
	got := CountTags([]*dgraphStruct.DgraphTask{l("Bug, frontend"), l("bug"), l(""), {}, l("backend, Frontend")})
	want := []TagCount{{"Bug", 2}, {"frontend", 2}, {"backend", 1}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
