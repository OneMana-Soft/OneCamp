package business

import (
	"testing"

	userDomain "github.com/akashc777/OneCamp/domain/User"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
)

func TestActorKind(t *testing.T) {
	cases := []struct {
		name string
		user *dgraphStruct.DgraphUser
		want string
	}{
		{"nobody", nil, ActorPerson},
		{"a member", &dgraphStruct.DgraphUser{EmailID: "maya@acme.com"}, ActorPerson},
		{"an agent", &dgraphStruct.DgraphUser{IsBot: true, EmailID: userDomain.AgentBotPrefix + "x" + userDomain.BotEmailDomain}, ActorAgent},
		{"a Slack person", &dgraphStruct.DgraphUser{IsBot: true, EmailID: userDomain.SlackBridgeBotEmail}, ActorPerson},
		{"a channel guest", &dgraphStruct.DgraphUser{IsBot: true, EmailID: userDomain.ChannelGuestBotEmail}, ActorPerson},
		{"an unknown bot", &dgraphStruct.DgraphUser{IsBot: true, EmailID: "webhook" + userDomain.BotEmailDomain}, ActorApp},
	}
	for _, c := range cases {
		if got := ActorKind(c.user); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
		if c.user != nil && c.user.EmailID != "" {
			t.Errorf("%s: the actor's email was left in the response", c.name)
		}
	}
}

func TestActorOf(t *testing.T) {
	maya := &dgraphStruct.DgraphUser{UserName: "maya"}
	items := []dgraphModels.UnifiedActivityItem{
		{Mention: &dgraphStruct.DgraphMentions{Post: &dgraphStruct.DgraphPost{PostBy: maya}}},
		{Mention: &dgraphStruct.DgraphMentions{Comment: &dgraphStruct.DgraphComment{CommentBy: maya}}},
		{Mention: &dgraphStruct.DgraphMentions{Chat: &dgraphStruct.DgraphChat{From: maya}}},
		{Comment: &dgraphStruct.DgraphComment{CommentBy: maya}},
		{Reaction: &dgraphModels.ReactionsActivity{DgraphReaction: dgraphStruct.DgraphReaction{AddedBy: maya}}},
	}
	for i := range items {
		if actorOf(&items[i]) != maya {
			t.Errorf("item %d: the actor wasn't found", i)
		}
	}
}
