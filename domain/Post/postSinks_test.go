package domain

import (
	"errors"
	"strings"
	"testing"
)

// The sinks' read of a post (chat A2): a post that mentions people arrives
// with its Mention nodes as a list (post_mentions is [uid] in the graph), and
// is read whole, its mentions gathered into the one Mention its writer holds.
func TestASinksPostReadsItsMentionsAsTheGraphListsThem(t *testing.T) {
	raw := []byte(`{"postInfo":[{
		"uid":"0x1","post_uuid":"p-1","post_text":"<p>launch at 10 @sam</p>",
		"post_by":{"uid":"0xa","user_uuid":"u-ada","user_name":"ada"},
		"post_channel":{"uid":"0xc","ch_uuid":"c-launch","ch_name":"launch"},
		"post_reply_to":{"uid":"0xq","post_uuid":"p-0","post_text":"<p>when?</p>"},
		"post_mentioned":[{"mention_users":[{"uid":"0xb","user_uuid":"u-sam"}]},{"mention_users":[{"uid":"0xd","user_uuid":"u-jonas"}]}]
	}]}`)
	p, err := parseSinkPost(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.Uuid != "p-1" || p.PostBy == nil || p.PostBy.Uuid != "u-ada" || p.Channel == nil || p.Channel.Uuid != "c-launch" || p.ReplyToPost == nil || p.ReplyToPost.Uuid != "p-0" {
		t.Errorf("post %+v", p)
	}
	if p.Mentions == nil || len(p.Mentions.Mentions) != 2 || p.Mentions.Mentions[0].Uuid != "u-sam" || p.Mentions.Mentions[1].Uid != "0xd" {
		t.Errorf("mentions %+v", p.Mentions)
	}
	if !strings.Contains(sinkPostQuery, "post_mentioned: post_mentions") {
		t.Error("the query reads post_mentions under its own name again: a list DgraphPost can't hold")
	}
}

func TestASinksPostThatIsntThereIsGone(t *testing.T) {
	if _, err := parseSinkPost([]byte(`{"postInfo":[]}`)); !errors.Is(err, ErrPostGone) {
		t.Errorf("%v, want ErrPostGone", err)
	}
	if p, err := parseSinkPost([]byte(`{"postInfo":[{"post_uuid":"p-2","post_text":"<p>hi</p>"}]}`)); err != nil || p.Mentions != nil {
		t.Errorf("a post that mentions nobody: %+v %v", p, err)
	}
}

func TestASearchEntryIsWrittenAsAnIndexSoARetryIsTheSameEntry(t *testing.T) {
	body := "{ \"create\" : { \"_index\" : \"posts\", \"_id\" : \"p-1\" } }\n{}\n{ \"create\" : { \"_index\" : \"attachments\", \"_id\" : \"f-1\" } }\n{}\n"
	got := asIndexOps(body)
	if strings.Contains(got, `"create"`) || strings.Count(got, `{ "index" : {`) != 2 {
		t.Errorf("%q", got)
	}
}
