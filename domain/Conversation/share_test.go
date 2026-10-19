package domain

import (
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A share keeps whole messages, each with its replies, newest first, within
// its limit, and leaves the oldest out.
func TestAShareKeepsTheNewestWholeMessagesWithinItsLimit(t *testing.T) {
	at := func(h int) time.Time { return time.Date(2026, 10, 16, h, 0, 0, 0, time.UTC) }
	newestFirst := []CutoffMessage{{At: at(9), Replies: 1}, {At: at(8), Replies: 3}, {At: at(7)}, {At: at(6), Replies: 2}}
	for _, c := range []struct {
		limit int
		want  time.Time
	}{
		{100, time.Time{}}, // everything fits
		{7, at(7)},         // 2 + 4 + 1: the 06:00 message and its replies are left out
		{6, at(8)},         // 2 + 4: the 07:00 message would be the seventh
		{2, at(9)},         // the newest alone
		{1, at(9)},         // not even the newest whole: it goes alone, its replies cut
	} {
		if got := Cutoff(newestFirst, c.limit); !got.Equal(c.want) {
			t.Errorf("limit %d: cutoff %v, want %v", c.limit, got, c.want)
		}
	}
	if got := Cutoff(nil, 10); !got.IsZero() {
		t.Errorf("nothing to share: %v", got)
	}
}

func TestShareCountsAreReadPerChoice(t *testing.T) {
	raw := []byte(`{"g":[{"m0":3,"m1":10,"m2":40}],"s":[{"r0":1,"r1":4,"r2":12}]}`)
	got, err := parseShareCounts(raw, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []ShareCount{{3, 1}, {10, 4}, {40, 12}}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("choice %d: %+v, want %+v", i, got[i], want[i])
		}
	}
	if got[2].Total() != 52 {
		t.Errorf("total %d", got[2].Total())
	}
	// A conversation the graph doesn't have counts nothing.
	if none, err := parseShareCounts([]byte(`{"g":[],"s":[{}]}`), 1); err != nil || none[0].Total() != 0 {
		t.Errorf("an unknown conversation: %+v %v", none, err)
	}
}

// A page names each message's writer; a message nobody can be named for stays
// in the page, so the share's cursor passes it, with nothing to copy.
func TestAShareablePageKeepsItsOrderAndPassesTheUnattributable(t *testing.T) {
	raw := []byte(`{"g":[{"dm_chats":[
		{"chat_uuid":"a","chat_body_text":"<p>one</p>","chat_created_at":"2026-10-16T09:00:00Z",
		 "chat_from":{"uid":"0x1","user_uuid":"maya"},"chat_reply_to":{"chat_uuid":"z"},
		 "chat_attachments":[{"attachment_obj_key":"k1","attachment_file_name":"plan.pdf"}],
		 "chat_comments":[{"comment_uuid":"r1","comment_text":"<p>yes</p>","comment_created_at":"2026-10-16T09:01:00Z","comment_by":{"uid":"0x2","user_uuid":"jonas"}},
		                  {"comment_uuid":"r2","comment_text":"<p>?</p>","comment_created_at":"2026-10-16T09:02:00Z"}]},
		{"chat_uuid":"b","chat_body_text":"<p>half made</p>","chat_created_at":"2026-10-16T09:05:00Z"}
	]}]}`)
	got, err := parseSharePage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].UUID != "a" || got[1].UUID != "b" {
		t.Fatalf("page: %+v", got)
	}
	a := got[0]
	if a.By == nil || a.By.Uuid != "maya" || a.ReplyTo != "z" || len(a.Attachments) != 1 || len(a.Replies) != 1 || a.Replies[0].UUID != "r1" {
		t.Errorf("the first message: %+v", a)
	}
	if got[1].By != nil {
		t.Errorf("a message nobody wrote is attributed: %+v", got[1])
	}
}

func TestAHiddenConversationStaysHiddenUntilANewerMessage(t *testing.T) {
	hidAt := time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC)
	before, after := hidAt.Add(-time.Minute), hidAt.Add(time.Minute)
	hidden := map[string]time.Time{"g": hidAt}
	dm := func(at *time.Time) *dgraphStruct.DgraphDm {
		return &dgraphStruct.DgraphDm{GroupingId: "g", Chats: []*dgraphStruct.DgraphChat{{CreatedAt: at}}}
	}
	if !stillHidden(dm(&before), hidden) {
		t.Error("a conversation hidden after its newest message shows")
	}
	if stillHidden(dm(&after), hidden) {
		t.Error("a newer message didn't show it again")
	}
	if stillHidden(&dgraphStruct.DgraphDm{GroupingId: "other"}, hidden) {
		t.Error("a conversation nobody hid is hidden")
	}
}
