package level

import (
	"reflect"
	"testing"
)

// Whoever a change adds to one of editors, commenters and viewers is taken
// off the other two, each removal list once each, in order, without "".
func TestOneRoleEach(t *testing.T) {
	edit, comment, view := OneRoleEach([]string{"e"}, []string{"c", "c"}, []string{"v"}, []string{"x", "", "x"}, []string{"y"}, nil)
	for _, c := range []struct {
		name      string
		got, want []string
	}{
		{"off edit", edit, []string{"x", "v", "c"}},
		{"off comment", comment, []string{"y", "e", "v"}},
		{"off view", view, []string{"e", "c"}},
	} {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, c.got, c.want)
		}
	}
	if got := Unique(nil); got == nil || len(got) != 0 {
		t.Errorf("Unique(nil) = %#v, want an empty list", got)
	}
}
