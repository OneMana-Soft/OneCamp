package business

import (
	"testing"
	"time"

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

func TestAPostIsReadOnlyInItsOwnChannel(t *testing.T) {
	public, private := false, true
	live, gone := time.Time{}, time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	dgraphZero := time.Unix(0, 0).UTC() // the graph's own zero value is live too
	in := func(ch string, deletedAt *time.Time) *dgraphStruct.DgraphPost {
		return &dgraphStruct.DgraphPost{Uuid: "p", Channel: &dgraphStruct.DgraphChannel{Uuid: ch}, DeletedAt: deletedAt}
	}
	general := &dgraphStruct.DgraphChannel{Uuid: "general", IsPrivate: &public}
	mine := &dgraphStruct.DgraphChannel{Uuid: "mine", IsPrivate: &private, IsMember: 1}
	theirs := &dgraphStruct.DgraphChannel{Uuid: "theirs", IsPrivate: &private}
	for name, tc := range map[string]struct {
		ch   *dgraphStruct.DgraphChannel
		post *dgraphStruct.DgraphPost
		want bool
	}{
		"in a private channel they're in":       {mine, in("mine", &live), true},
		"in a public channel":                   {general, in("general", nil), true},
		"live by the graph's zero":              {mine, in("mine", &dgraphZero), true},
		"another channel's post, asked in mine": {mine, in("theirs", &live), false},
		"in a private channel they aren't in":   {theirs, in("theirs", &live), false},
		"deleted":                               {mine, in("mine", &gone), false},
		"a post whose channel didn't load":      {mine, &dgraphStruct.DgraphPost{Uuid: "p"}, false},
		"no post":                               {mine, nil, false},
		"no channel":                            {nil, in("mine", &live), false},
	} {
		if got := PostReadableIn(tc.ch, tc.post); got != tc.want {
			t.Errorf("%s: PostReadableIn = %v, want %v", name, got, tc.want)
		}
	}
}
