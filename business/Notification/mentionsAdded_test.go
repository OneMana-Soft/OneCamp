package notification

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

const ben = "44444444-4444-4444-8444-444444444444"

// A save tells whom it names now that it didn't before, each once, never its
// actor; a re-save, a reorder and a removal tell nobody.
func TestASaveAddsOnlyWhomItNewlyNames(t *testing.T) {
	for _, c := range []struct {
		name          string
		before, after []string
		want          []string
	}{
		{"a first save", nil, []string{sam, maya}, []string{sam, maya}},
		{"an edit adding one", []string{sam}, []string{sam, maya}, []string{maya}},
		{"a re-save", []string{sam, maya}, []string{sam, maya}, nil},
		{"a reorder", []string{sam, maya}, []string{maya, sam}, nil},
		{"a removal", []string{sam, maya}, []string{sam}, nil},
		{"the actor and a repeat", nil, []string{akash, maya, maya, ""}, []string{maya}},
	} {
		if got := MentionsAdded(c.before, c.after, akash); !slices.Equal(got, c.want) {
			t.Errorf("%s: added %v, want %v", c.name, got, c.want)
		}
	}
}

// Whom a save adds is told once ever, keyed by the thing and the person, of
// those who hear of it; a remove and re-add, raced or not, tells nobody twice.
// Someone a broadcast reached is pushed but not emailed.
func TestAnAddedMentionIsToldOnceToWhoHearsOfIt(t *testing.T) {
	w := &toldWays{}
	withTellSeams(t, w)
	n := Notice{Kind: MessageMentioned, ResourceKind: "channel", ResourceUUID: "c1", Actor: &dgraphStruct.DgraphUser{Uuid: akash},
		Title: "Akash mentioned you in #launch", Path: "/app/channel/c1/p1", Email: EventChannelMention}
	var asked []string
	save := MentionSave{Kind: "post", ID: "p1", Before: []string{sam}, After: []string{sam, maya, ben},
		Hear: func(_ context.Context, added []string) ([]string, error) {
			asked = added
			return []string{maya, ben, sam}, nil // sam, named before, is never told
		},
		Named: func(id string) bool { return id == maya },
	}
	told := TellMentionsAdded(context.Background(), n, save)
	if !slices.Equal(asked, []string{maya, ben}) || !slices.Equal(told, []string{maya, ben}) {
		t.Fatalf("asked who hears of %v and told %v; want maya and ben", asked, told)
	}
	if !w.keys["mention|post|p1|"+maya] || !w.keys["mention|post|p1|"+ben] {
		t.Errorf("notices keyed %v; want mention|post|p1|<person>", w.keys)
	}
	if !slices.Equal(w.delivered, []string{maya, ben}) || !slices.Equal(w.emailed, []string{maya}) {
		t.Errorf("pushed %v and emailed %v; want both pushed and only maya, whom the words name, emailed", w.delivered, w.emailed)
	}

	// Ben taken out, then put back: told nobody again.
	save.Before, save.After = []string{sam, maya}, []string{sam, maya, ben}
	if again := TellMentionsAdded(context.Background(), n, save); len(again) != 0 || len(w.delivered) != 2 {
		t.Errorf("a re-add told %v and pushed %v; want nobody again", again, w.delivered)
	}
}

// A mention that went through business/Notify's door (an edit of a chat
// message, V-843: the door chose who hears of it and pushed them) is noted in
// the inbox of each the door told, emailed to those it emails whom the words
// name, and never pushed here; once ever, as TellMentionsAdded tells it.
func TestADoorsMentionIsNotedAndEmailedNeverPushed(t *testing.T) {
	w := &toldWays{}
	withTellSeams(t, w)
	n := Notice{Kind: MessageMentioned, ResourceKind: "channel", ResourceUUID: "c1", Actor: &dgraphStruct.DgraphUser{Uuid: akash},
		Title: "Akash mentioned you in #launch", Path: "/app/channel/c1/p1", Email: EventChannelMention}
	save := MentionSave{Kind: "post", ID: "p1", After: []string{maya, ben, sam},
		Hear: func(context.Context, []string) ([]string, error) {
			t.Error("asked who hears of it: the door chose")
			return nil, nil
		},
		Named: func(id string) bool { return id != ben }, // ben was reached by a broadcast
	}
	// The door told all three in the inbox; it emails maya and ben (sam's
	// email it held).
	told := NoteMentionsAdded(context.Background(), n, save, []string{maya, ben, sam}, []string{maya, ben})
	if !slices.Equal(told, []string{maya, ben, sam}) || !w.keys["mention|post|p1|"+maya] || !w.keys["mention|post|p1|"+sam] {
		t.Fatalf("told %v keyed %v; want all three, keyed mention|post|p1|<person>", told, w.keys)
	}
	if len(w.delivered) != 0 || len(w.through) != 0 || !slices.Equal(w.emailed, []string{maya}) {
		t.Errorf("pushed %v %v and emailed %v; want no push (the door's) and only maya emailed", w.delivered, w.through, w.emailed)
	}
	if again := NoteMentionsAdded(context.Background(), n, save, []string{maya}, []string{maya}); len(again) != 0 || len(w.emailed) != 1 {
		t.Errorf("noted again %v, emailed %v; want nobody twice", again, w.emailed)
	}
}

// Who hears of it unread tells nobody; nobody added asks nobody.
func TestAnAddedMentionTellsNobodyWithoutWhoHearsOfIt(t *testing.T) {
	w := &toldWays{}
	withTellSeams(t, w)
	n := Notice{Kind: TaskMentioned, ResourceKind: "task", ResourceUUID: "t1", Actor: &dgraphStruct.DgraphUser{Uuid: akash}}
	failed := MentionSave{Kind: "task", ID: "t1", After: []string{sam},
		Hear: func(context.Context, []string) ([]string, error) { return nil, errors.New("down") }}
	if told := TellMentionsAdded(context.Background(), n, failed); len(told) != 0 || w.writes != 0 {
		t.Errorf("told %v (%d writes) when who hears of it couldn't be read", told, w.writes)
	}
	none := MentionSave{Kind: "task", ID: "t1", Before: []string{sam}, After: []string{sam},
		Hear: func(context.Context, []string) ([]string, error) {
			t.Error("asked who hears of nobody")
			return nil, nil
		}}
	TellMentionsAdded(context.Background(), n, none)
}

// A save reads who its thing named before it only when it names someone, and
// never for an import's save; one that couldn't be read tells nobody.
func TestASaveReadsWhoItNamedOnlyWhenItCanTell(t *testing.T) {
	old := mentionedNow
	t.Cleanup(func() { mentionedNow = old })
	reads := 0
	var fail error
	mentionedNow = func(_ context.Context, kind, id string) ([]string, error) {
		reads++
		if kind != "comment" || id != "c1" {
			t.Errorf("read the mentions of %s %s", kind, id)
		}
		return []string{sam}, fail
	}
	after := []*dgraphStruct.DgraphUser{{Uuid: sam}, {Uuid: maya}, nil}
	if s, ok := SaveOfMentions(context.Background(), "comment", "c1", after); !ok || !slices.Equal(s.Before, []string{sam}) || !slices.Equal(s.After, []string{sam, maya}) {
		t.Errorf("save %+v ok %v; want before [sam], after [sam maya]", s, ok)
	}
	if _, ok := SaveOfMentions(context.Background(), "comment", "c1", nil); ok || reads != 1 {
		t.Errorf("a save naming nobody: ok %v, %d reads; want no telling and no read", ok, reads)
	}
	if _, ok := SaveOfMentions(context.WithValue(context.Background(), helpers.BulkImportContextKey, true), "comment", "c1", after); ok || reads != 1 {
		t.Errorf("an import's save: ok %v, %d reads; want no telling and no read", ok, reads)
	}
	fail = errors.New("down")
	if _, ok := SaveOfMentions(context.Background(), "comment", "c1", after); ok {
		t.Error("a save whose mentions couldn't be read can tell")
	}
}

// A deleted message takes back the notice each person was told of its
// mentions, by the key Tell wrote it under, whatever holds the mention (V-843).
func TestRetractMentionNoticesTakesBackEachPersonsKey(t *testing.T) {
	var got []string
	old := retractNotices
	retractNotices = func(_ context.Context, keys []string) (int64, error) {
		got = append(got, keys...)
		return int64(len(keys)), nil
	}
	t.Cleanup(func() { retractNotices = old })
	const msg = "55555555-5555-4555-8555-555555555555"
	if err := RetractMentionNotices(context.Background(), msg, []string{ben}); err != nil {
		t.Fatal(err)
	}
	want := []string{"mention|post|" + msg + "|" + ben, "mention|chat|" + msg + "|" + ben, "mention|comment|" + msg + "|" + ben}
	if !slices.Equal(got, want) {
		t.Errorf("keys taken back %v, want %v", got, want)
	}
}
