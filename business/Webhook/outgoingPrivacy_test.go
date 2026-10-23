package business

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

var (
	pubChannel  = uuid.MustParse("c0ffee00-0000-4000-8000-000000000001")
	privChannel = uuid.MustParse("c0ffee00-0000-4000-8000-000000000002")
	otherChan   = uuid.MustParse("c0ffee00-0000-4000-8000-000000000003")
	project     = uuid.MustParse("b0a4d000-0000-4000-8000-000000000001")
	otherProj   = uuid.MustParse("b0a4d000-0000-4000-8000-000000000002")
)

// Every scope a webhook can be set up for, against every kind of event: a
// public channel's, a private channel's, a project's, the workspace's, and a
// DM's or a group chat's (BE-BUG-13, SA-BUG-10).
func TestWhatEachWebhookIsSentOfEachEvent(t *testing.T) {
	type webhook struct {
		scope  string
		entity *uuid.UUID
	}
	webhooks := map[string]webhook{
		"workspace":         {ScopeOrg, nil},
		"workspace (old)":   {"", nil},
		"#public's":         {ScopeChannel, &pubChannel},
		"#private's":        {ScopeChannel, &privChannel},
		"another channel's": {ScopeChannel, &otherChan},
		"the project's":     {ScopeProject, &project},
		"another project's": {ScopeProject, &otherProj},
		"an unknown scope":  {"dm", nil},
	}
	events := []struct {
		name   string
		kind   string
		data   map[string]interface{}
		public bool
		want   map[string]delivery // anything not named: sendNothing
	}{
		{"a message in a public channel", "post.created",
			map[string]interface{}{"post_id": "p1", "channel_id": pubChannel.String(), "text": "launch at 9"}, true,
			map[string]delivery{"workspace": sendAll, "workspace (old)": sendAll, "#public's": sendAll}},
		{"a message in a private channel", "post.created",
			map[string]interface{}{"post_id": "p2", "channel_id": privChannel.String(), "text": "pricing is"}, false,
			map[string]delivery{"workspace": sendIDs, "workspace (old)": sendIDs, "#private's": sendAll}},
		{"someone joining a private channel", "user.joined",
			map[string]interface{}{"user_id": "u1", "channel_id": privChannel.String()}, false,
			map[string]delivery{"workspace": sendIDs, "workspace (old)": sendIDs, "#private's": sendAll}},
		{"a task in a project", "task.created",
			map[string]interface{}{"task_id": "t1", "project_id": project.String(), "name": "Ship it"}, false,
			map[string]delivery{"workspace": sendAll, "workspace (old)": sendAll, "the project's": sendAll}},
		{"a workspace event", "github.issue.opened",
			map[string]interface{}{"issue": "42"}, false,
			map[string]delivery{"workspace": sendAll, "workspace (old)": sendAll}},
		{"a doc's event", "doc.created",
			map[string]interface{}{"doc_uuid": "d1", "doc_private": true}, false,
			map[string]delivery{"workspace": sendAll, "workspace (old)": sendAll}},
		{"someone joining the workspace", "user.joined",
			map[string]interface{}{"user_id": "u1"}, false,
			map[string]delivery{"workspace": sendAll, "workspace (old)": sendAll}},
		// Default-deny: a workspace event not on wholeAtOrg goes as ids, a
		// private doc's table's name with it (BE-11-G-5).
		{"a row in a private doc's table", "table.row.created",
			map[string]interface{}{"table_id": "t1", "table_name": "Salaries", "row_id": "r1"}, false,
			map[string]delivery{"workspace": sendIDs, "workspace (old)": sendIDs}},
		{"a kind nobody has decided on yet", "invoice.paid",
			map[string]interface{}{"invoice_id": "i1", "client": "Acme"}, false,
			map[string]delivery{"workspace": sendIDs, "workspace (old)": sendIDs}},
		{"a kind that only starts like a whole one", "github_secret.created",
			map[string]interface{}{"token": "x"}, false,
			map[string]delivery{"workspace": sendIDs, "workspace (old)": sendIDs}},
		{"a DM", "chat.created",
			map[string]interface{}{"message_id": "m1", "group_id": "u1 u2", "text": "my salary is"}, false, nil},
		{"a reply in a group chat", "chat.comment.created",
			map[string]interface{}{"message_id": "m1", "group_id": "g1", "text": "thread secret"}, false, nil},
		{"an agent's message in a DM", "agent.message",
			map[string]interface{}{"channel_id": "", "surface_kind": "dm", "text": "the numbers are"}, false, nil},
		{"an agent's message in a public channel", "agent.message",
			map[string]interface{}{"channel_id": pubChannel.String(), "text": "done"}, true,
			map[string]delivery{"workspace": sendAll, "workspace (old)": sendAll, "#public's": sendAll}},
		{"a channel event whose channel doesn't read", "post.created",
			map[string]interface{}{"post_id": "p3", "channel_id": "not-a-uuid", "text": "?"}, false,
			map[string]delivery{"workspace": sendIDs, "workspace (old)": sendIDs}},
	}
	for _, ev := range events {
		place := placeOf(ev.kind, ev.data)
		for name, w := range webhooks {
			got := deliveryTo(w.scope, w.entity, place, ev.public)
			if want := ev.want[name]; got != want {
				t.Errorf("%s, to %s webhook: got %v, want %v", ev.name, name, got, want)
			}
		}
	}
}

// What a workspace webhook is sent of a private channel's event: ids, never
// words or names.
func TestIDsOnlyCarryNoWords(t *testing.T) {
	data := map[string]interface{}{
		"post_id": "p1", "channel_id": privChannel.String(), "channel_name": "launch-pricing",
		"author_id": "u1", "author_name": "Maya Chen", "text": "the price is 49", "mention_ids": []string{"0x9"},
		"comment_uuid": "c1", "source": "user",
	}
	b, _ := json.Marshal(idsOnly(data))
	for _, words := range []string{"price", "Maya", "launch-pricing"} {
		if strings.Contains(string(b), words) {
			t.Errorf("carries %q: %s", words, b)
		}
	}
	for _, key := range []string{"post_id", "channel_id", "author_id", "mention_ids", "comment_uuid", "source"} {
		if !strings.Contains(string(b), `"`+key+`"`) {
			t.Errorf("lost %s: %s", key, b)
		}
	}
	if _, isText := data["text"]; !isText {
		t.Fatal("cut the event's own data, not a copy")
	}
}

// The scope an admin chooses is checked before anything is written.
func TestAWebhooksScopeIsChecked(t *testing.T) {
	ctx := context.Background()
	admin := uuid.New()
	if scope, id, err := webhookScope(ctx, "incoming", ScopeChannel, &pubChannel, admin, admin); err != nil || scope != ScopeOrg || id != nil {
		t.Errorf("an incoming webhook (sent no events) got %q %v %v", scope, id, err)
	}
	for _, s := range []string{"", ScopeOrg} {
		if scope, id, err := webhookScope(ctx, "outgoing", s, &pubChannel, admin, admin); err != nil || scope != ScopeOrg || id != nil {
			t.Errorf("%q: got %q %v %v, want the workspace with no entity", s, scope, id, err)
		}
	}
	for _, s := range []string{"dm", "group_chat", "everything"} {
		if _, _, err := webhookScope(ctx, "outgoing", s, &pubChannel, admin, admin); err == nil {
			t.Errorf("%q was taken", s)
		}
	}
	nilID := uuid.Nil
	for _, s := range []string{ScopeChannel, ScopeProject} {
		for _, id := range []*uuid.UUID{nil, &nilID} {
			_, _, err := webhookScope(ctx, "outgoing", s, id, admin, admin)
			if _, said := err.(ScopeError); !said {
				t.Errorf("%s with no %s: %v, want the admin told to choose one", s, s, err)
			}
		}
	}
}
