package business

import (
	"encoding/json"
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

func TestApplyChannelMembership(t *testing.T) {
	cases := []struct {
		name       string
		ids        []string
		channel    string
		enabled    bool
		wantIDs    []string
		wantChange bool
	}{
		{"add to empty", nil, "c1", true, []string{"c1"}, true},
		{"add new", []string{"c1"}, "c2", true, []string{"c1", "c2"}, true},
		{"add existing is no-op", []string{"c1"}, "c1", true, []string{"c1"}, false},
		{"remove present", []string{"c1", "c2"}, "c1", false, []string{"c2"}, true},
		{"remove absent is no-op", []string{"c2"}, "c1", false, []string{"c2"}, false},
		{"remove last", []string{"c1"}, "c1", false, []string{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, changed := applyChannelMembership(c.ids, c.channel, c.enabled)
			if changed != c.wantChange {
				t.Fatalf("changed = %v, want %v", changed, c.wantChange)
			}
			if len(got) != len(c.wantIDs) {
				t.Fatalf("ids = %v, want %v", got, c.wantIDs)
			}
			for i := range got {
				if got[i] != c.wantIDs[i] {
					t.Fatalf("ids = %v, want %v", got, c.wantIDs)
				}
			}
		})
	}
}

// marshalScope must preserve other scope keys (e.g. project_ids) and drop
// channel_ids entirely when empty (so an empty scope reads as "everywhere").
func TestMarshalScopePreservesOtherKeys(t *testing.T) {
	out, err := marshalScope(`{"project_ids":["p1"],"channel_ids":["old"]}`, []string{"c1", "c2"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	var m map[string]interface{}
	if uerr := json.Unmarshal([]byte(out), &m); uerr != nil {
		t.Fatalf("unmarshal: %v", uerr)
	}
	if _, ok := m["project_ids"]; !ok {
		t.Fatal("project_ids was dropped")
	}
	ch, _ := m["channel_ids"].([]interface{})
	if len(ch) != 2 {
		t.Fatalf("channel_ids = %v, want 2 entries", m["channel_ids"])
	}
}

func TestIdInList(t *testing.T) {
	ids := []string{"0x1", "0x2", "uuid-3"}
	if !idInList(ids, "0x2") {
		t.Fatal("expected match for present id")
	}
	if idInList(ids, "0x9") {
		t.Fatal("did not expect match for absent id")
	}
	if idInList(ids, "") {
		t.Fatal("empty target must never match")
	}
}

func TestMentionIDsFromEvent(t *testing.T) {
	// Native []string passes through.
	if got := mentionIDsFromEvent([]string{"a", "b"}); len(got) != 2 {
		t.Fatalf("expected 2 ids, got %v", got)
	}
	// Generic []interface{} (as it arrives through the event map) is coerced,
	// dropping non-strings and blanks.
	got := mentionIDsFromEvent([]interface{}{"a", "", 5, "b"})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("unexpected coercion result: %v", got)
	}
	// Unknown shapes yield nil.
	if mentionIDsFromEvent(42) != nil {
		t.Fatal("expected nil for unsupported type")
	}
}

// Who may put an agent in a channel or take it out: its owner or a workspace
// admin, either way; a channel's admins may take it out of their channel
// unless that leaves it in none (and so answering everywhere); anyone else
// in the channel, neither.
func TestMayPlaceInChannel(t *testing.T) {
	owner, someone := uuid.New(), uuid.New()
	agent := &model.AiAgent{Id: uuid.New(), CreatedBy: owner}
	for _, c := range []struct {
		name         string
		actor        Actor
		channelAdmin bool
		adding       bool
		channelsLeft int
		want         error
	}{
		{"its owner adds it", Actor{UserID: owner}, false, true, 1, nil},
		{"its owner takes it out of its last channel", Actor{UserID: owner}, false, false, 0, nil},
		{"a workspace admin adds it", Actor{UserID: someone, IsAdmin: true}, false, true, 1, nil},
		{"a workspace admin takes it out of its last channel", Actor{UserID: someone, IsAdmin: true}, false, false, 0, nil},
		{"a member adds it", Actor{UserID: someone}, false, true, 1, errForbidden},
		{"a member takes it out", Actor{UserID: someone}, false, false, 1, errForbidden},
		{"a channel admin adds it", Actor{UserID: someone}, true, true, 1, errForbidden},
		{"a channel admin takes it out", Actor{UserID: someone}, true, false, 1, nil},
		{"a channel admin takes it out of its only channel", Actor{UserID: someone}, true, false, 0, errOnlyChannel},
	} {
		if got := mayPlaceInChannel(c.actor, agent, c.channelAdmin, c.adding, c.channelsLeft); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	if !IsOnlyChannel(errOnlyChannel) || IsOnlyChannel(errForbidden) || !IsForbidden(errForbidden) {
		t.Error("the refusals aren't told apart")
	}
}
