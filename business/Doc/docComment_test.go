package businness

import (
	"testing"
	"time"

	"github.com/google/uuid"

	adapter "github.com/akashc777/OneCamp/adapter/Doc"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A public doc that takes everyone's comments can have no creator, and
// everyone who can read it may comment on it. The comment named the doc's
// creator as whose content it was on, read through a nil pointer, which took
// the request down. It now names nobody, and still names the creator of a doc
// that has one.
func TestACommentOnADocWithNoCreatorNamesNobody(t *testing.T) {
	at := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	id := uuid.New()
	input := &adapter.CreateOrUpdateDocCommentInput{CommentBody: "<p>noted</p>"}
	mentions := []*dgraphStruct.DgraphUser{{Uid: "0xb2", Uuid: "someone"}}

	c := newDocComment(&dgraphStruct.DgraphDoc{Uuid: uuid.NewString()}, "0xa1", input, mentions, id, at)
	if c.ContentAddedBy != nil {
		t.Errorf("a comment on a doc with no creator is on %+v's content, want nobody's", c.ContentAddedBy)
	}
	if c.CommentBy == nil || c.CommentBy.Uid != "0xa1" || c.Uuid != id.String() || c.Text != "<p>noted</p>" {
		t.Errorf("the comment: %+v", c)
	}
	if c.Mentions == nil || len(c.Mentions.Mentions) != 1 || c.Mentions.Mentions[0].Uid != "0xb2" {
		t.Errorf("its mentions: %+v", c.Mentions)
	}

	// A creator read without a node to point at names nobody either: an empty
	// reference in a mutation is a new, empty node.
	blank := &dgraphStruct.DgraphDoc{Uuid: uuid.NewString(), CreatedBy: &dgraphStruct.DgraphUser{}}
	if c := newDocComment(blank, "0xa1", input, nil, id, at); c.ContentAddedBy != nil {
		t.Errorf("a comment on a doc whose creator has no uid is on %+v's content, want nobody's", c.ContentAddedBy)
	}

	made := &dgraphStruct.DgraphDoc{Uuid: uuid.NewString(), CreatedBy: &dgraphStruct.DgraphUser{Uid: "0xc3", Uuid: "owner"}}
	if c := newDocComment(made, "0xa1", input, nil, id, at); c.ContentAddedBy == nil || c.ContentAddedBy.Uid != "0xc3" {
		t.Errorf("a comment on a doc its creator made is on %+v's content, want 0xc3's", c.ContentAddedBy)
	}
}
