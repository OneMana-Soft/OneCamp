package Activity

import (
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A mention in a comment shows only when the comment's own parent passed the
// feed's filters: a reply in a private channel the reader isn't in comes back
// with its text and no post, and used to be listed.
func TestAMentionShowsOnlyWithSomethingToShow(t *testing.T) {
	comment := func(c dgraphStruct.DgraphComment) *dgraphStruct.DgraphMentions {
		c.Text = "<p>@jonas can you check these numbers?</p>"
		return &dgraphStruct.DgraphMentions{Comment: &c}
	}
	for name, tc := range map[string]struct {
		m    *dgraphStruct.DgraphMentions
		want bool
	}{
		"in a post":                         {&dgraphStruct.DgraphMentions{Post: &dgraphStruct.DgraphPost{Uuid: "p"}}, true},
		"in a chat":                         {&dgraphStruct.DgraphMentions{Chat: &dgraphStruct.DgraphChat{Uuid: "c"}}, true},
		"in a reply in a channel":           {comment(dgraphStruct.DgraphComment{Post: &dgraphStruct.DgraphPost{Uuid: "p"}}), true},
		"in a reply in a group chat":        {comment(dgraphStruct.DgraphComment{Chat: &dgraphStruct.DgraphChat{Uuid: "c"}}), true},
		"in a task's comment":               {comment(dgraphStruct.DgraphComment{Task: &dgraphStruct.DgraphTask{Uuid: "t"}}), true},
		"in a doc's comment":                {comment(dgraphStruct.DgraphComment{Doc: &dgraphStruct.DgraphDoc{Uuid: "d"}}), true},
		"in a board's comment":              {comment(dgraphStruct.DgraphComment{Board: &dgraphStruct.DgraphBoard{Uuid: "b"}}), true},
		"in a reply the reader can't reach": {comment(dgraphStruct.DgraphComment{}), false},
		"nothing left":                      {&dgraphStruct.DgraphMentions{}, false},
		"none":                              {nil, false},
	} {
		if got := mentionShown(tc.m); got != tc.want {
			t.Errorf("%s: mentionShown = %v, want %v", name, got, tc.want)
		}
	}
}
