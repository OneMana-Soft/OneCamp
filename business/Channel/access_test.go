package business

import (
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

func TestWhoCanReadAChannel(t *testing.T) {
	public, private := false, true
	for name, tc := range map[string]struct {
		ch   *dgraphStruct.DgraphChannel
		want bool
	}{
		"public, not a member":    {&dgraphStruct.DgraphChannel{Uuid: "c", IsPrivate: &public}, true},
		"private, a member":       {&dgraphStruct.DgraphChannel{Uuid: "c", IsPrivate: &private, IsMember: 1}, true},
		"private, a moderator":    {&dgraphStruct.DgraphChannel{Uuid: "c", IsPrivate: &private, IsAdmin: 1}, true},
		"private, not a member":   {&dgraphStruct.DgraphChannel{Uuid: "c", IsPrivate: &private}, false},
		"privacy unknown":         {&dgraphStruct.DgraphChannel{Uuid: "c"}, false},
		"privacy unknown, member": {&dgraphStruct.DgraphChannel{Uuid: "c", IsMember: 1}, true},
		"nothing loaded":          {&dgraphStruct.DgraphChannel{IsPrivate: &public}, false},
		"missing":                 {nil, false},
	} {
		if got := CanRead(tc.ch); got != tc.want {
			t.Errorf("%s: CanRead = %v, want %v", name, got, tc.want)
		}
	}
}
