package business

import (
	"context"
	"slices"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A topic is encrypted, so the web app can't work out which conversation one
// is. The settings name each conversation's topics by the key the app names
// the conversation by (chat A1's subscriptions by conversation), and those
// topics are among the ones it's handed.
func TestTheSettingsNameEachConversationsTopics(t *testing.T) {
	t.Setenv("JWT_SECRET", "conversation-topics-test")
	person := "6f1c3b0e-0d6f-4c56-9a59-3a3f4e1c2b01"
	channel := "4a1d0c6e-7b8f-4e2d-9c1a-0f2e3d4c5b6a"
	dm := "2b0e7f1a-1c2d-4e3f-8a9b-0c1d2e3f4a5b 6f1c3b0e-0d6f-4c56-9a59-3a3f4e1c2b01"
	group := "9f86d081884c7d659a2feaa0c55ad015"
	prev := readPersonTopics
	readPersonTopics = func(context.Context, string) (*dgraphStruct.DgraphUser, error) {
		return &dgraphStruct.DgraphUser{
			Channels: []*dgraphStruct.DgraphChannel{{Uuid: channel}, {Uuid: ""}},
			DMs:      []*dgraphStruct.DgraphDm{{GroupingId: dm}, {GroupingId: group}, {GroupingId: ""}},
			Projects: []*dgraphStruct.DgraphProject{{Uuid: "5e4d3c2b-1a09-4f8e-9d7c-6b5a4f3e2d1c"}},
		}, nil
	}
	t.Cleanup(func() {
		readPersonTopics = prev
		memberTopicsCache.Delete(person)
	})

	cfg, err := GetMqttConfig(context.Background(), person, false)
	if err != nil {
		t.Fatal(err)
	}
	chMsg, chTyping := helpers.GetMqttTopicForChannel(channel)
	dmMsg, dmTyping := helpers.GetMqttTopicForDm(dm)
	grpMsg, grpTyping := helpers.GetMqttTopicForDm(group)
	want := map[string][]string{
		"channel:" + channel: {chMsg, chTyping},
		"dm:" + dm:           {dmMsg, dmTyping},
		"group:" + group:     {grpMsg, grpTyping},
	}
	if len(cfg.ConversationTopics) != len(want) {
		t.Fatalf("conversations named: %v, want %v", cfg.ConversationTopics, want)
	}
	for key, topics := range want {
		got := cfg.ConversationTopics[key]
		if !slices.Equal(got, topics) {
			t.Errorf("%s: %v, want %v", key, got, topics)
		}
		for _, topic := range got {
			if !slices.Contains(cfg.Topics, topic) {
				t.Errorf("%s names %s, which the settings don't hand out", key, topic)
			}
		}
	}
}
