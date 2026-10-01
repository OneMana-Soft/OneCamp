package business

import (
	"testing"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
)

func TestBuildTriggererActivity(t *testing.T) {
	agent := &model.AiAgent{Name: "Standup Bot"}
	bot := &userBusiness.BotIdentity{UUID: "bot-uuid", DgraphUID: "0xbot", Name: "Standup Bot"}

	t.Run("channel post → mention item with post + channel", func(t *testing.T) {
		item := buildTriggererActivity(agent, bot, Surface{Kind: SurfaceChannelPost, ChannelID: "ch1", PostID: "p1"}, "I'm blocked")
		if item == nil || item.ActivityType != mqttStruct.MESSAGE_ACTIVITY_MENTION {
			t.Fatalf("expected a MENTION item, got %+v", item)
		}
		if item.Mention == nil || item.Mention.PostUuid != "p1" {
			t.Fatalf("expected PostUuid p1, got %+v", item.Mention)
		}
		if item.Mention.Post == nil || item.Mention.Post.Channel == nil || item.Mention.Post.Channel.Uuid != "ch1" {
			t.Fatalf("expected channel ch1, got %+v", item.Mention.Post)
		}
		if item.Mention.Comment == nil || item.Mention.Comment.CommentBy == nil || item.Mention.Comment.CommentBy.UserName != "Standup Bot" {
			t.Fatalf("expected CommentBy the agent, got %+v", item.Mention.Comment)
		}
	})

	t.Run("group chat → mention item with chat + grouping", func(t *testing.T) {
		item := buildTriggererActivity(agent, bot, Surface{Kind: SurfaceGroupChat, GroupID: "g1", MessageID: "m1"}, "Done")
		if item == nil || item.Mention == nil || item.Mention.ChatUuid != "m1" {
			t.Fatalf("expected ChatUuid m1, got %+v", item)
		}
		if item.Mention.Chat == nil || item.Mention.Chat.DM == nil || item.Mention.Chat.DM.GroupingId != "g1" {
			t.Fatalf("expected grouping g1, got %+v", item.Mention.Chat)
		}
	})

	t.Run("task surface → nil (own path)", func(t *testing.T) {
		if item := buildTriggererActivity(agent, bot, Surface{Kind: SurfaceTask}, "Done"); item != nil {
			t.Fatalf("task surface should not build a mention item, got %+v", item)
		}
	})

	t.Run("missing ids → nil", func(t *testing.T) {
		if item := buildTriggererActivity(agent, bot, Surface{Kind: SurfaceChannelPost}, "x"); item != nil {
			t.Fatalf("channel_post with no post id should be nil, got %+v", item)
		}
	})
}
