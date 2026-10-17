package models

import (
	"encoding/json"
	"testing"
)

// A bot's typing message says it is a bot, and which kind, so the app can say
// an AI teammate "is working" rather than "is typing". A person's message
// reads exactly as it did before those fields existed, so every person's line,
// and an app that predates them, stay as they were.
func TestTypingSaysWhenABotTypes(t *testing.T) {
	cases := []struct {
		name string
		msg  any
		want string
	}{
		{
			"a person in a channel",
			MqttChannelTyping{UserUUID: "u1", UserName: "Maya Chen", ChannelUuid: "c1"},
			`{"user_uuid":"u1","user_name":"Maya Chen","channel_uuid":"c1"}`,
		},
		{
			"a person in a conversation",
			MqttChatTyping{UserUUID: "u1", UserName: "Maya Chen", ChatGrpId: "g1"},
			`{"user_uuid":"u1","user_name":"Maya Chen","chat_grp_id":"g1"}`,
		},
		{
			"an agent in a channel",
			MqttChannelTyping{UserUUID: "rc", UserName: "Release Captain", ChannelUuid: "c1", IsBot: true, BotKind: "agent"},
			`{"user_uuid":"rc","user_name":"Release Captain","channel_uuid":"c1","is_bot":true,"user_bot_kind":"agent"}`,
		},
		{
			"the assistant in a conversation",
			MqttChatTyping{UserUUID: "ai", UserName: "OneCamp AI", ChatGrpId: "g1", IsBot: true, BotKind: "assistant"},
			`{"user_uuid":"ai","user_name":"OneCamp AI","chat_grp_id":"g1","is_bot":true,"user_bot_kind":"assistant"}`,
		},
	}
	for _, c := range cases {
		got, err := json.Marshal(c.msg)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if string(got) != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}
