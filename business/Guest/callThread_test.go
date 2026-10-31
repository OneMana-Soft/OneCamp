package business

import (
	"strings"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A call's thread, read by someone outside its conversation, is only what was
// written while the call went on (calls CALLS-3b, CHAT-LEAD (c)): nothing
// from before it, after it or deleted, oldest first, as plain text with each
// writer's name.
func TestACallsThreadIsItsOwnSpan(t *testing.T) {
	from := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	until := from.Add(30 * time.Minute)
	at := func(m int) *time.Time { v := from.Add(time.Duration(m) * time.Minute); return &v }
	gone := at(5)
	maya := &dgraphStruct.DgraphUser{Uuid: "u1", UserName: "Maya Chen"}
	got := callSpan([]*dgraphStruct.DgraphComment{
		{Uuid: "later", Text: "<p>after</p>", CommentBy: maya, CreatedAt: at(31)},
		{Uuid: "second", Text: "<p>second</p>", CommentBy: maya, CreatedAt: at(10)},
		{Uuid: "before", Text: "<p>before</p>", CommentBy: maya, CreatedAt: at(-1)},
		{Uuid: "first", Text: "<p>first <b>one</b></p>", CommentBy: maya, CreatedAt: at(1)},
		{Uuid: "deleted", Text: "<p>gone</p>", CommentBy: maya, CreatedAt: at(2), DeletedAt: gone},
		{Uuid: "undated", Text: "<p>?</p>", CommentBy: maya},
	}, from, until, "post-1", "guest-bot")
	if len(got) != 2 || got[0].ID != "first" || got[1].ID != "second" {
		t.Fatalf("the span: %+v", got)
	}
	if got[0].Text != "first one" || got[0].Author != "Maya Chen" {
		t.Errorf("a reply: %+v", got[0])
	}
}

// The call's chat runs one person's replies together by a key that means
// nothing outside the thread (CODE-REVIEWER, CALLS-3b): never by name, since
// two guests may both be "Guest". A guest's reply carries the name they gave
// and says it's a guest's; a relay's replies are nobody's in particular.
func TestACallsRepliesSayWhoWroteThemWithoutAnId(t *testing.T) {
	from := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	at := func(m int) *time.Time { v := from.Add(time.Duration(m) * time.Minute); return &v }
	maya := &dgraphStruct.DgraphUser{Uuid: "u-maya", UserName: "Maya Chen"}
	kai := &dgraphStruct.DgraphUser{Uuid: "u-kai", UserName: "Kai Lin"}
	guests := &dgraphStruct.DgraphUser{Uuid: "guest-bot", UserName: "Guests", IsBot: true}
	slack := &dgraphStruct.DgraphUser{Uuid: "slack-bot", UserName: "Slack", IsBot: true}
	gia, err := GuestMessageHTML("Gia", "joining from the client")
	if err != nil {
		t.Fatal(err)
	}
	nameless, err := GuestMessageHTML("Guest", "me too")
	if err != nil {
		t.Fatal(err)
	}
	replies := []*dgraphStruct.DgraphComment{
		{Uuid: "1", Text: "<p>hello</p>", CommentBy: maya, CreatedAt: at(1)},
		{Uuid: "2", Text: "<p>again</p>", CommentBy: maya, CreatedAt: at(2)},
		{Uuid: "3", Text: "<p>hi</p>", CommentBy: kai, CreatedAt: at(3)},
		{Uuid: "4", Text: gia, CommentBy: guests, CreatedAt: at(4)},
		{Uuid: "5", Text: nameless, CommentBy: guests, CreatedAt: at(5)},
		{Uuid: "6", Text: "<p>[Ana (Slack)] from Slack</p>", CommentBy: slack, CreatedAt: at(6)},
	}
	got := callSpan(replies, from, from.Add(time.Hour), "post-1", "guest-bot")
	if len(got) != 6 {
		t.Fatalf("the span: %+v", got)
	}
	if got[0].AuthorKey == "" || got[0].AuthorKey != got[1].AuthorKey || got[2].AuthorKey == got[0].AuthorKey {
		t.Errorf("members' keys: %q %q %q", got[0].AuthorKey, got[1].AuthorKey, got[2].AuthorKey)
	}
	for _, m := range got {
		if strings.Contains(m.AuthorKey, "u-maya") || strings.Contains(m.AuthorKey, "u-kai") {
			t.Errorf("a key gives an id away: %q", m.AuthorKey)
		}
	}
	if other := callSpan(replies[:1], from, from.Add(time.Hour), "post-2", "guest-bot"); other[0].AuthorKey == got[0].AuthorKey {
		t.Error("Maya's key is the same in another call's thread")
	}
	if g := got[3]; g.Author != "Gia" || !g.Guest || g.AuthorKey != "" || g.Text != "joining from the client" {
		t.Errorf("a guest's reply: %+v", g)
	}
	if g := got[4]; g.Author != "Guest" || !g.Guest || g.AuthorKey != "" {
		t.Errorf("a nameless guest's reply: %+v", g)
	}
	if r := got[5]; r.Guest || r.AuthorKey != "" || r.Author != "Ana (Slack)" {
		t.Errorf("a relay's reply: %+v", r)
	}
}
