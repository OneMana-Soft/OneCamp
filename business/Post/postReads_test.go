package business

import (
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

func TestADeletedMessagesThreadShowsNothingOfIt(t *testing.T) {
	posted := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	deleted := posted.Add(time.Hour)
	live := time.Time{}
	two := uint64(2)
	thread := func(deletedAt *time.Time, replies int) *dgraphStruct.DgraphPost {
		p := &dgraphStruct.DgraphPost{
			Uid: "0x1", Uuid: "p", Text: "<p>the launch price is 40</p>", CreatedAt: &posted, DeletedAt: deletedAt,
			PostBy:        &dgraphStruct.DgraphUser{Uuid: "sam", UserName: "sam"},
			MediaObj:      []*dgraphStruct.DgraphAttachment{{ObjectKey: "pricing.pdf"}},
			Reactions:     []*dgraphStruct.DgraphReaction{{EmojiUuid: "+1"}},
			ForwaredPost:  &dgraphStruct.DgraphPost{Uuid: "f", Text: "<p>forwarded</p>"},
			ReplyToPost:   &dgraphStruct.DgraphPost{Uuid: "r", Text: "<p>quoted</p>"},
			Channel:       &dgraphStruct.DgraphChannel{Uuid: "c", IsAdmin: 1},
			Mentions:      &dgraphStruct.DgraphMentions{Uid: "0x9"},
			ReactionCount: []dgraphStruct.ReactionCount{{}},
			CommentCount:  &two,
			Comments:      nil,
		}
		for i := 0; i < replies; i++ {
			p.Comments = append(p.Comments, &dgraphStruct.DgraphComment{Uuid: "reply", Text: "<p>ok</p>"})
		}
		return p
	}

	// A live post is its thread, as read.
	livePost := thread(&live, 2)
	if got, ok := ThreadForMember(livePost); !ok || got != livePost {
		t.Fatalf("a live post's thread: %+v, %v", got, ok)
	}
	if got, ok := ThreadForMember(thread(nil, 0)); !ok || got.Text == DeletedPostText {
		t.Fatalf("a live post with no deletion time read: %+v, %v", got, ok)
	}

	// Deleted, with nobody's replies under it: no thread.
	if got, ok := ThreadForMember(thread(&deleted, 0)); ok || got != nil {
		t.Errorf("a deleted post with no replies opened as %+v", got)
	}

	// Deleted, with replies: a placeholder holding them and nothing of the message.
	got, ok := ThreadForMember(thread(&deleted, 2))
	if !ok || got == nil {
		t.Fatal("a deleted post with replies has no thread")
	}
	if got.Text != DeletedPostText {
		t.Errorf("its text is %q", got.Text)
	}
	if got.PostBy != nil || got.MediaObj != nil || got.Reactions != nil || got.ForwaredPost != nil ||
		got.ReplyToPost != nil || got.Mentions != nil || got.ReactionCount != nil {
		t.Errorf("the placeholder still carries the message: %+v", got)
	}
	if len(got.Comments) != 2 || got.CommentCount == nil || *got.CommentCount != 2 || got.Channel == nil ||
		got.Channel.IsAdmin != 1 || got.Uuid != "p" || got.DeletedAt == nil || !got.DeletedAt.Equal(deleted) {
		t.Errorf("the placeholder lost the thread: %+v", got)
	}

	if got, ok := ThreadForMember(nil); ok || got != nil {
		t.Errorf("no post: %+v, %v", got, ok)
	}
}
