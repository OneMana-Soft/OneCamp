package businness

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
)

// A new comment reaches its doc's maker and the people it mentions live only
// while they may read the doc, by the Access Resolver's rule, asked once: a
// maker the doc was taken from hears nothing, someone mentioned who reads it
// through a team hears of it, and with no answer nobody does.
func TestALiveCommentReachesOnlyThoseWhoMayReadTheDoc(t *testing.T) {
	type sent struct{ to, kind string }
	var got []sent
	var asked [][]string
	readers := map[string]bool{"teammate": true}
	var readersErr error
	oldR, oldP := commentActivityReaders, commentActivityPublish
	t.Cleanup(func() { commentActivityReaders, commentActivityPublish = oldR, oldP })
	commentActivityReaders = func(_ context.Context, _ string, ids []string) (map[string]bool, error) {
		asked = append(asked, ids)
		if readersErr != nil {
			return nil, readersErr
		}
		out := map[string]bool{}
		for _, id := range ids {
			out[id] = readers[id]
		}
		return out, nil
	}
	commentActivityPublish = func(to string, item *dgraphModels.UnifiedActivityItem) {
		got = append(got, sent{to, item.ActivityType})
	}
	doc := &dgraphStruct.DgraphDoc{Uuid: "doc", Title: "Plan", CreatedBy: &dgraphStruct.DgraphUser{Uuid: "maker"}}
	mentions := []*dgraphStruct.DgraphUser{{Uuid: "teammate"}, {Uuid: "stranger"}, nil}

	PublishDocCommentActivity("c1", "<p>hi</p>", time.Now(), doc, dgraphStruct.DgraphUser{Uuid: "author"}, mentions)
	if want := []sent{{"teammate", "MENTION"}}; !slices.Equal(got, want) {
		t.Errorf("told %v, want %v: the maker can't read it any more, the stranger never could", got, want)
	}
	if len(asked) != 1 || !slices.Equal(asked[0], []string{"maker", "teammate", "stranger"}) {
		t.Errorf("the rule was asked %v, want once about the maker and the mentioned", asked)
	}

	got, readers["maker"] = nil, true
	PublishDocCommentActivity("c2", "<p>hi</p>", time.Now(), doc, dgraphStruct.DgraphUser{Uuid: "author"}, nil)
	if want := []sent{{"maker", "COMMENT"}}; !slices.Equal(got, want) {
		t.Errorf("told %v, want the maker, who reads it", got)
	}

	got, readersErr = nil, errors.New("graph down")
	PublishDocCommentActivity("c3", "<p>hi</p>", time.Now(), doc, dgraphStruct.DgraphUser{Uuid: "author"}, mentions)
	if len(got) != 0 {
		t.Errorf("with the rule unanswered, told %v", got)
	}
}
