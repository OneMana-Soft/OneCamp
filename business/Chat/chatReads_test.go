package business

import (
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

func TestADeletedDirectMessagesThreadShowsNothingOfIt(t *testing.T) {
	sent := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	deleted := sent.Add(time.Hour)
	two := uint64(2)
	chat := func(deletedAt *time.Time, replies int) *dgraphStruct.DgraphChat {
		c := &dgraphStruct.DgraphChat{
			Uid: "0x1", Uuid: "c", Body: "<p>my password is hunter2</p>", CreatedAt: &sent, DeletedAt: deletedAt,
			From:             &dgraphStruct.DgraphUser{Uuid: "sam"},
			To:               &dgraphStruct.DgraphUser{Uuid: "alex"},
			DM:               &dgraphStruct.DgraphDm{GroupingId: "g"},
			MediaObj:         []*dgraphStruct.DgraphAttachment{{ObjectKey: "keys.txt"}},
			Reactions:        []*dgraphStruct.DgraphReaction{{EmojiUuid: "+1"}},
			FwdMsgPost:       &dgraphStruct.DgraphPost{Uuid: "p"},
			ReplyToChat:      &dgraphStruct.DgraphChat{Uuid: "r"},
			Mentions:         &dgraphStruct.DgraphMentions{Uid: "0x9"},
			ChatCommentCount: &two,
		}
		for i := 0; i < replies; i++ {
			c.Comments = append(c.Comments, &dgraphStruct.DgraphComment{Uuid: "reply"})
		}
		return c
	}
	live := chat(nil, 2)
	if got, ok := ChatThreadForMember(live); !ok || got != live {
		t.Fatalf("a live message's thread: %+v, %v", got, ok)
	}
	if got, ok := ChatThreadForMember(chat(&deleted, 0)); ok || got != nil {
		t.Errorf("a deleted message with no replies opened as %+v", got)
	}
	got, ok := ChatThreadForMember(chat(&deleted, 2))
	if !ok || got.Body != DeletedChatText || got.From != nil || got.To != nil || got.MediaObj != nil || got.Reactions != nil ||
		got.FwdMsgPost != nil || got.ReplyToChat != nil || got.Mentions != nil {
		t.Errorf("the placeholder still carries the message: %+v", got)
	}
	if len(got.Comments) != 2 || got.DM == nil || got.DM.GroupingId != "g" || got.ChatCommentCount == nil || *got.ChatCommentCount != 2 {
		t.Errorf("the placeholder lost the thread: %+v", got)
	}
	if got, ok := ChatThreadForMember(nil); ok || got != nil {
		t.Errorf("no message: %+v, %v", got, ok)
	}
}
