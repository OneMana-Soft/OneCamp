package business

import (
	"context"
	"errors"
	"net/http"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// readsAs points the guard's graph read at found (or at err), and counts
// the reads.
func readsAs(t *testing.T, found *dgraphStruct.DgraphReaction, err error) *int {
	t.Helper()
	reads := 0
	was := reactionOnParent
	reactionOnParent = func(context.Context, string, string, string) (*dgraphStruct.DgraphReaction, error) {
		reads++
		return found, err
	}
	t.Cleanup(func() { reactionOnParent = was })
	return &reads
}

const (
	comment = "0x2a"
	mine    = "0x7"
	theirs  = "0x8"
)

// Only a reaction, and only the caller's own, on the parent the request
// names may be changed or removed. Anything else the uid names is no
// reaction at all as far as the caller learns (404); someone else's
// reaction on that parent is theirs (403).
func TestMayChangeOnlyTheCallersOwnReaction(t *testing.T) {
	reaction := func(types []string, addedBy *dgraphStruct.DgraphUser) *dgraphStruct.DgraphReaction {
		return &dgraphStruct.DgraphReaction{Uid: "0x99", DType: types, EmojiUuid: "e", AddedBy: addedBy}
	}
	cases := []struct {
		name   string
		found  *dgraphStruct.DgraphReaction
		err    error
		caller string
		want   int // 0: allowed
	}{
		{"their own reaction", reaction([]string{"Reaction"}, &dgraphStruct.DgraphUser{Uid: mine}), nil, mine, 0},
		{"someone else's reaction", reaction([]string{"Reaction"}, &dgraphStruct.DgraphUser{Uid: theirs}), nil, mine, http.StatusForbidden},
		{"a reaction nobody added", reaction([]string{"Reaction"}, nil), nil, mine, http.StatusForbidden},
		{"a caller the graph doesn't know", reaction([]string{"Reaction"}, &dgraphStruct.DgraphUser{}), nil, "", http.StatusForbidden},
		{"no reaction on that parent", nil, nil, mine, http.StatusNotFound},
		{"a doc stamped as their reaction", reaction([]string{"Doc", "Reaction"}, &dgraphStruct.DgraphUser{Uid: mine}), nil, mine, http.StatusNotFound},
		{"a node of another type", reaction([]string{"Doc"}, &dgraphStruct.DgraphUser{Uid: mine}), nil, mine, http.StatusNotFound},
		{"a node with no type", reaction(nil, &dgraphStruct.DgraphUser{Uid: mine}), nil, mine, http.StatusNotFound},
		{"a graph that doesn't answer", nil, errors.New("deadline exceeded"), mine, http.StatusInternalServerError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reads := readsAs(t, c.found, c.err)
			got, refusal := MayChange(context.Background(), OnComment, comment, "0x99", c.caller)
			if *reads != 1 {
				t.Fatalf("read the graph %d times, want once", *reads)
			}
			if c.want == 0 {
				if refusal != nil || got != c.found {
					t.Fatalf("refused (%+v), want their own reaction allowed and returned", refusal)
				}
				return
			}
			if refusal == nil {
				t.Fatalf("allowed, want %d", c.want)
			}
			if refusal.Status != c.want || refusal.Msg == "" || got != nil {
				t.Errorf("answered %d %q with %+v, want %d and no reaction", refusal.Status, refusal.Msg, got, c.want)
			}
		})
	}
}

// What isn't a uid is nothing to look for: refused without reading, so it
// never reaches a query.
func TestMayChangeRefusesWhatIsNotAUidWithoutReading(t *testing.T) {
	for _, uid := range []string{"", " ", "0x", "0x0", "0x0000", "abc", "42", "0xg1", "0x1 ", " 0x1", "0x1) OR has(doc_title",
		`0x1", "doc_title": "x`, "0x12345678901234567"} {
		reads := readsAs(t, &dgraphStruct.DgraphReaction{Uid: uid, DType: []string{"Reaction"}, AddedBy: &dgraphStruct.DgraphUser{Uid: mine}}, nil)
		got, refusal := MayChange(context.Background(), OnPost, comment, uid, mine)
		if refusal == nil || refusal.Status != http.StatusNotFound || got != nil {
			t.Errorf("reaction uid %q: %+v %+v, want 404", uid, got, refusal)
		}
		if *reads != 0 {
			t.Errorf("reaction uid %q was read from the graph", uid)
		}
	}
	reads := readsAs(t, nil, nil)
	if _, refusal := MayChange(context.Background(), OnPost, "", "0x99", mine); refusal == nil || refusal.Status != http.StatusNotFound || *reads != 0 {
		t.Errorf("a parent with no uid: %+v after %d reads, want 404 without reading", refusal, *reads)
	}
}

// The read is made through the edge the parent holds its reactions by, for
// the parent and the reaction the request names.
func TestMayChangeReadsThroughTheParentsEdge(t *testing.T) {
	for on, edge := range map[On]string{OnComment: "comment_reactions", OnPost: "post_reactions", OnChat: "chat_reactions"} {
		var asked [3]string
		was := reactionOnParent
		reactionOnParent = func(_ context.Context, e, parent, reaction string) (*dgraphStruct.DgraphReaction, error) {
			asked = [3]string{e, parent, reaction}
			return nil, nil
		}
		MayChange(context.Background(), on, "0xa1", "0xB2", mine)
		reactionOnParent = was
		if asked != [3]string{edge, "0xa1", "0xB2"} {
			t.Errorf("%s read %q, want %s of 0xa1 filtered to 0xB2", on, asked, edge)
		}
	}
}
