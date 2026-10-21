package domain

import (
	"errors"
	"strings"
	"testing"
)

// The sinks' read of a DM or group message (chat A2): one that mentions
// people arrives with its Mention nodes as a list (chat_mentions is [uid] in
// the graph), and is read whole, its mentions gathered into one Mention.
func TestASinksMessageReadsItsMentionsAsTheGraphListsThem(t *testing.T) {
	raw := []byte(`{"chatInfo":[{
		"uid":"0x1","chat_uuid":"c-1","chat_body_text":"<p>hi @sam</p>",
		"chat_from":{"uid":"0xa","user_uuid":"u-ada","user_name":"ada"},
		"chat_to":{"uid":"0xb","user_uuid":"u-sam","user_name":"sam"},
		"chat_dm":{"uid":"0xd","dm_grouping_id":"u-ada u-sam","dm_participants":[{"uid":"0xa","user_uuid":"u-ada"},{"uid":"0xb","user_uuid":"u-sam"}]},
		"chat_reply_to":{"uid":"0xq","chat_uuid":"c-0","chat_body_text":"<p>when?</p>"},
		"chat_mentioned":[{"mention_users":[{"uid":"0xb","user_uuid":"u-sam"}]}]
	}]}`)
	c, err := parseSinkChat(raw)
	if err != nil {
		t.Fatal(err)
	}
	if c.Uuid != "c-1" || c.From == nil || c.From.Uuid != "u-ada" || c.To == nil || c.To.Uuid != "u-sam" || c.DM == nil || c.DM.GroupingId != "u-ada u-sam" || len(c.DM.Participants) != 2 || c.ReplyToChat == nil || c.ReplyToChat.Uuid != "c-0" {
		t.Errorf("message %+v", c)
	}
	if c.Mentions == nil || len(c.Mentions.Mentions) != 1 || c.Mentions.Mentions[0].Uuid != "u-sam" {
		t.Errorf("mentions %+v", c.Mentions)
	}
	if !strings.Contains(sinkChatQuery, "chat_mentioned: chat_mentions") {
		t.Error("the query reads chat_mentions under its own name again: a list DgraphChat can't hold")
	}
}

func TestASinksMessageThatIsntThereIsGone(t *testing.T) {
	if _, err := parseSinkChat([]byte(`{"chatInfo":[]}`)); !errors.Is(err, ErrChatGone) {
		t.Errorf("%v, want ErrChatGone", err)
	}
}
