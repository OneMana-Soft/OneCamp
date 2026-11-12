package business

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	tableAccess "github.com/akashc777/OneCamp/business/TableAccess"
	threadBusiness "github.com/akashc777/OneCamp/business/Thread"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// mentionWorld is a comments world whose members are cy (0x0c, "Cy") and,
// once named, anyone in its people; the lookups it was asked are kept.
type mentionWorld struct {
	*commentWorld
	people map[string]*dgraphStruct.DgraphUser
	asked  [][]string
	err    error
}

const (
	cyUUID = "0c0c0c0c-0000-4000-8000-00000000000c"
	boUUID = "0b0b0b0b-0000-4000-8000-00000000000b"
)

func newMentionWorld(t *testing.T) *mentionWorld {
	t.Helper()
	key := "pics/cy.png"
	w := &mentionWorld{commentWorld: newCommentWorld(t), people: map[string]*dgraphStruct.DgraphUser{
		cyUUID: {Uid: "0x0c", Uuid: cyUUID, UserName: "Cy", ProfileKey: &key},
	}}
	old := mentionable
	t.Cleanup(func() { mentionable = old })
	mentionable = func(_ context.Context, ids []string) (map[string]*dgraphStruct.DgraphUser, error) {
		w.asked = append(w.asked, ids)
		out := map[string]*dgraphStruct.DgraphUser{}
		for _, id := range ids {
			if p := w.people[id]; p != nil {
				out[id] = p
			}
		}
		return out, w.err
	}
	return w
}

// A comment's @[member uuid] is posted as the member's chip, as the app's
// composer writes it, and answered as it was written, the member named; the
// people are read once, and a comment with no mention reads no one.
func TestACommentsMentionIsPostedAsTheMembersChipAndAnsweredAsWritten(t *testing.T) {
	w := newMentionWorld(t)
	got, err := w.serve(t, "tables.row.comment.create", map[string]any{"text": "Can @[" + strings.ToUpper(cyUUID) + "] check <this>?"})
	if err != nil {
		t.Fatal(err)
	}
	body := w.threads.bodies[0].HTML
	if !strings.Contains(body, `<span data-type="mention" data-id="`+cyUUID+`@0x0c" data-label="Cy" data-mention-suggestion-char="@">@Cy</span>`) || !strings.Contains(body, "check &lt;this&gt;?") {
		t.Fatalf("posted %q, want Cy's chip in the escaped text", body)
	}
	c, ok := got.(apiComment)
	if !ok || c.ID != "c-first" || c.Text != "<p>Can @["+cyUUID+"] check &lt;this&gt;?</p>" {
		t.Fatalf("answered %#v, want the comment as written", got)
	}
	if p := c.Mentioned[cyUUID]; len(c.Mentioned) != 1 || p.UUID != cyUUID || p.Name != "Cy" || p.ProfileKey != "pics/cy.png" {
		t.Fatalf("mentioned %+v, want Cy", c.Mentioned)
	}
	if !slices.Equal(w.asked[0], []string{cyUUID}) {
		t.Fatalf("asked for %v", w.asked)
	}

	// A reply and an edit say it the same way.
	w.threads.got = threadBusiness.View{Thread: threadBusiness.Thread{ID: "t1", ResourceKind: tableAccess.KindTableRow, ResourceUUID: w.row.String()}}
	got, err = w.serve(t, "tables.row.comment.create", map[string]any{"text": "@[" + cyUUID + "] done", "thread": "t1"})
	if c, ok := got.(apiComment); err != nil || !ok || c.Text != "<p>@["+cyUUID+"] done</p>" || c.Mentioned[cyUUID].Name != "Cy" {
		t.Fatalf("the reply answered %#v (%v)", got, err)
	}
	asked := len(w.asked)
	if _, err := w.serve(t, "tables.row.comment.create", map[string]any{"text": "No one"}); err != nil || len(w.asked) != asked {
		t.Fatalf("a comment with no mention asked for people %v (%v)", w.asked[asked:], err)
	}
}

// Someone who isn't a member is refused by name, and when the people can't
// be read the server says so: either way nothing is posted.
func TestAMentionOfNoMemberIsRefusedAndNothingIsPosted(t *testing.T) {
	w := newMentionWorld(t)
	_, err := w.serve(t, "tables.row.comment.create", map[string]any{"text": "@[" + cyUUID + "] and @[" + boUUID + "]"})
	refusedAs(t, err, http.StatusBadRequest, "@["+boUUID+"] is no member of this workspace")
	if e := asAPIError(t, err); e.Code != "invalid_argument" {
		t.Fatalf("code %q, want invalid_argument", e.Code)
	}
	w.err = errors.New("graph down")
	_, err = w.serve(t, "tables.row.comment.create", map[string]any{"text": "@[" + cyUUID + "]"})
	refusedAs(t, err, http.StatusServiceUnavailable, "the people mentioned couldn't be read")
	if len(w.threads.calls) != 0 {
		t.Fatalf("threads asked %v", w.threads.calls)
	}
}

// The list writes every chip back as @[uuid] and names each person once for
// all its threads: by their name now, or, for someone gone or unread, by the
// name their chip kept. A read never fails for a name.
func TestTheListWritesEveryMentionAsWrittenAndNamesThem(t *testing.T) {
	w := newMentionWorld(t)
	chip := func(id, uid, name string) string {
		return `<span data-type="mention" data-id="` + id + `@` + uid + `" data-label="` + name + `" data-mention-suggestion-char="@">@` + name + `</span>`
	}
	w.threads.listed = []threadBusiness.View{
		{Thread: threadBusiness.Thread{ID: "t1"}, Comments: []threadBusiness.Comment{
			{ID: "c1", Text: "<p>Hi " + chip(cyUUID, "0x0c", "Cyrus") + "</p>"},
			{ID: "c2", Text: "<p>plain</p>"},
		}},
		{Thread: threadBusiness.Thread{ID: "t2"}, Comments: []threadBusiness.Comment{
			{ID: "c3", Text: "<p>" + chip(boUUID, "0x0b", "Bo") + " and " + chip(cyUUID, "0x0c", "Cy") + "</p>"},
		}},
	}
	got, err := w.serve(t, "tables.row.comments.list", nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := got.(rowComments).Threads
	c1, c2, c3 := ts[0].Comments[0], ts[0].Comments[1], ts[1].Comments[0]
	if c1.Text != "<p>Hi @["+cyUUID+"]</p>" || c1.Mentioned[cyUUID].Name != "Cy" || c2.Text != "<p>plain</p>" || c2.Mentioned != nil {
		t.Fatalf("thread 1 reads %+v / %+v", c1, c2)
	}
	if c3.Text != "<p>@["+boUUID+"] and @["+cyUUID+"]</p>" || c3.Mentioned[boUUID].Name != "Bo" || c3.Mentioned[cyUUID].Name != "Cy" {
		t.Fatalf("thread 2 reads %+v", c3)
	}
	if len(w.asked) != 1 {
		t.Fatalf("people read %d times, want once: %v", len(w.asked), w.asked)
	}
	if s := jsonOf(t, got); strings.Contains(s, "0x0c") || strings.Contains(s, "data-type") || !strings.Contains(s, `"comment_mentioned":{"`+cyUUID+`":{"user_uuid":"`+cyUUID+`","user_name":"Cy"`) {
		t.Fatalf("answered %s", s)
	}

	w.err = errors.New("graph down")
	got, err = w.serve(t, "tables.row.comments.list", nil)
	if err != nil {
		t.Fatalf("a name unread failed the read: %v", err)
	}
	if c := got.(rowComments).Threads[0].Comments[0]; c.Text != "<p>Hi @["+cyUUID+"]</p>" || c.Mentioned[cyUUID].Name != "Cyrus" {
		t.Fatalf("with no names read, it reads %+v, want the name the chip kept", c)
	}
}
