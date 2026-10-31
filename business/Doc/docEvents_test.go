package businness

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Doc"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	followModels "github.com/akashc777/OneCamp/models/postgres/Follow"
)

const (
	sam  = "22222222-2222-4222-8222-222222222222"
	maya = "33333333-3333-4333-8333-333333333333"
	jo   = "44444444-4444-4444-8444-444444444444"
	d2   = "55555555-5555-4555-8555-555555555555"
	d3   = "66666666-6666-4666-8666-666666666666"
)

// Who a body mentions and which docs it links to, each once, whatever the
// chip's id looks like; a channel chip and a broken id are nobody.
func TestABodysMentionsAndLinksAreRead(t *testing.T) {
	body := `<p>Hi <span data-type="mention" data-id="` + sam + `@0x1f" data-label="Sam">@Sam</span>,
		<span data-label="Maya" data-type="mention" data-id="` + maya + `">@Maya</span> and again
		<span data-type="mention" data-id="` + sam + `@0x1f">@Sam</span>
		<span data-type="mention" data-id="nobody@0x2">@x</span>
		<span data-type="channelMention" data-id="` + jo + `">#eng</span>
		see <span data-type="entityMention" data-id="doc:` + d2 + `">Plan</span>,
		<a href="https://app.example.test/app/doc/` + d3 + `/comment">the RFC</a>, <a href="/app/doc/` + d2 + `">Plan</a>,
		<a href="https://example.com/other">elsewhere</a></p>`
	people, docs := bodyReferences(body)
	if !slices.Equal(people, []string{sam, maya}) {
		t.Errorf("people %v", people)
	}
	if !slices.Equal(docs, []string{d2, d3}) {
		t.Errorf("docs %v", docs)
	}
}

type emitted struct {
	mu     sync.Mutex
	events []resourceEventBusiness.Event
}

func (e *emitted) Emit(_ context.Context, ev resourceEventBusiness.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, ev)
}

func withEmitted(t *testing.T) *emitted {
	t.Helper()
	e := &emitted{}
	old := resourceEventBusiness.Bus
	resourceEventBusiness.Bus = e
	t.Cleanup(func() { resourceEventBusiness.Bus = old })
	return e
}

// A save tells the people it newly mentions, by whoever wrote it, never
// someone writing in it; a save that mentions nobody new tells nobody.
func TestASaveTellsOnlyThePeopleItNewlyMentions(t *testing.T) {
	ev := withEmitted(t)
	oldS, oldR, oldW := syncMentions, readDocLinks, writeDocLinks
	t.Cleanup(func() { syncMentions, readDocLinks, writeDocLinks = oldS, oldR, oldW })
	newly := []string{sam, maya}
	syncMentions = func(_ context.Context, _ string, people []string) (followModels.MentionSync, error) {
		return followModels.MentionSync{Added: newly}, nil
	}
	var linked []string
	readDocLinks = func(context.Context, string) (*dgraphStruct.DgraphDoc, error) {
		return &dgraphStruct.DgraphDoc{Uid: "0x1"}, nil
	}
	writeDocLinks = func(_ context.Context, _ *dgraphStruct.DgraphDoc, refs []string) (bool, error) {
		linked = refs
		return true, nil
	}

	at := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	body := `<span data-type="mention" data-id="` + sam + `@0x1">@Sam</span><a href="/app/doc/` + d2 + `">x</a>`
	mentionsSaved(context.Background(), Change{DocUUID: "doc", Body: body, Contributors: []string{maya}, SavedAt: at})
	if len(ev.events) != 1 {
		t.Fatalf("emitted %+v", ev.events)
	}
	e := ev.events[0]
	if e.Kind != resourceEventBusiness.DocMentioned || e.Actor.UserUUID != maya || !slices.Equal(e.Subjects, []string{sam}) || !e.At.Equal(at) {
		t.Errorf("the mention event %+v (maya wrote it, so isn't told)", e)
	}
	if !slices.Equal(linked, []string{d2}) {
		t.Errorf("links kept %v", linked)
	}

	// Two people in one save: nobody in particular wrote it.
	ev.events = nil
	newly = []string{jo}
	withJo := body + `<span data-type="mention" data-id="` + jo + `@0x4">@Jo</span>`
	mentionsSaved(context.Background(), Change{DocUUID: "doc", Body: withJo, Contributors: []string{maya, sam}, SavedAt: at})
	if e := ev.events[0]; e.Actor.UserUUID != "" || e.Data["contributors"] != maya+","+sam {
		t.Errorf("a shared save %+v", e)
	}

	ev.events = nil
	newly = nil
	mentionsSaved(context.Background(), Change{DocUUID: "doc", Body: body + "<p>more</p>", Contributors: []string{maya}, SavedAt: at})
	if len(ev.events) != 0 {
		t.Errorf("a save with nobody newly mentioned emitted %+v", ev.events)
	}

	// A save that mentions and links to what the last one did touches no store.
	synced := 0
	syncMentions = func(context.Context, string, []string) (followModels.MentionSync, error) {
		synced++
		return followModels.MentionSync{}, nil
	}
	mentionsSaved(context.Background(), Change{DocUUID: "doc", Body: body + "<p>typing</p>", SavedAt: at})
	if synced != 0 {
		t.Errorf("an unchanged save synced its mentions %d times", synced)
	}
	mentionsSaved(context.Background(), Change{DocUUID: "doc", Body: "<p>no one</p>", SavedAt: at})
	if synced != 1 {
		t.Errorf("a save that dropped its mentions synced %d times", synced)
	}
}

// A quiet save (a copy's body, an import's) keeps whom its body mentions and
// the docs it links to, and tells nobody, though the same save made loudly in
// a new doc tells everyone it mentions.
func TestAQuietSaveKeepsWhoItMentionsAndTellsNobody(t *testing.T) {
	ev := withEmitted(t)
	oldS, oldR, oldW := syncMentions, readDocLinks, writeDocLinks
	t.Cleanup(func() { syncMentions, readDocLinks, writeDocLinks = oldS, oldR, oldW })
	since := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	made := since.Add(time.Hour)
	kept := map[string][]string{}
	syncMentions = func(_ context.Context, docUUID string, people []string) (followModels.MentionSync, error) {
		kept[docUUID] = people
		// The doc's first keeping: everyone it mentions is new.
		return followModels.MentionSync{Added: people, First: true, Since: since}, nil
	}
	readDocLinks = func(context.Context, string) (*dgraphStruct.DgraphDoc, error) {
		return &dgraphStruct.DgraphDoc{Uid: "0x1", CreatedAt: &made}, nil
	}
	linked := map[string][]string{}
	writeDocLinks = func(_ context.Context, doc *dgraphStruct.DgraphDoc, refs []string) (bool, error) {
		linked[doc.Uid] = refs
		return true, nil
	}

	body := `<p><span data-type="mention" data-id="` + maya + `@0x3">@Maya</span> owns <a href="/app/doc/` + d2 + `">the plan</a>.</p>`
	const quietCopy, loudDoc = "77777777-7777-4777-8777-777777777777", "88888888-8888-4888-8888-888888888888"
	mentionsSaved(context.Background(), Change{DocUUID: quietCopy, Body: body, Contributors: []string{sam}, SavedAt: made, Quiet: true})
	if len(ev.events) != 0 {
		t.Fatalf("a quiet save told %+v", ev.events)
	}
	if !slices.Equal(kept[quietCopy], []string{maya}) || !slices.Equal(linked["0x1"], []string{d2}) {
		t.Fatalf("a quiet save kept mentions %v and links %v, want [maya] and [d2]", kept[quietCopy], linked["0x1"])
	}

	mentionsSaved(context.Background(), Change{DocUUID: loudDoc, Body: body, Contributors: []string{sam}, SavedAt: made})
	if len(ev.events) != 1 || !slices.Equal(ev.events[0].Subjects, []string{maya}) {
		t.Fatalf("the same save made loudly told %+v, want Maya", ev.events)
	}
}

// Who a save's mentions tell. The people it newly mentions, but not its own
// editors. Nobody for a save holding nobody's edit (a conversion as a doc
// loads). Nobody for a doc's first keeping when the doc was made before docs'
// mentions were kept, or isn't known to have been made since: its body
// mentioned them before anyone could be told (an upgrade's first save). A doc
// made since is told from its first save, and an agent's save tells. Nobody
// for a quiet save (a copy's body, an import's), a doc made since included:
// nobody wrote its mentions just now.
func TestWhoASavesMentionsTell(t *testing.T) {
	since := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	before, after := since.Add(-24*time.Hour), since.Add(time.Minute)
	made := func(at time.Time) *dgraphStruct.DgraphDoc { return &dgraphStruct.DgraphDoc{Uid: "0x1", CreatedAt: &at} }
	byMaya := Change{Contributors: []string{maya}}
	for _, c := range []struct {
		name string
		save Change
		kept followModels.MentionSync
		doc  *dgraphStruct.DgraphDoc
		want []string
	}{
		{"a later save", byMaya, followModels.MentionSync{Added: []string{sam, maya}, Since: since}, made(before), []string{sam}},
		{"a conversion, nobody's edit", Change{}, followModels.MentionSync{Added: []string{sam}, Since: since}, made(after), nil},
		{"an agent's save", Change{Agents: []AgentContributor{{AgentUUID: jo, AskerUUID: maya}}}, followModels.MentionSync{Added: []string{sam}}, made(before), []string{sam}},
		{"the first keeping of a doc made before", byMaya, followModels.MentionSync{Added: []string{sam, jo}, First: true, Since: since}, made(before), nil},
		{"the first keeping of a doc made since", byMaya, followModels.MentionSync{Added: []string{sam, maya}, First: true, Since: since}, made(after), []string{sam}},
		{"the first keeping of a doc made at the start", byMaya, followModels.MentionSync{Added: []string{sam}, First: true, Since: since}, made(since), []string{sam}},
		{"the first keeping of a doc not read", byMaya, followModels.MentionSync{Added: []string{sam}, First: true, Since: since}, nil, nil},
		{"the first keeping of a doc with no time", byMaya, followModels.MentionSync{Added: []string{sam}, First: true, Since: since}, &dgraphStruct.DgraphDoc{Uid: "0x1"}, nil},
		{"the first keeping when the start isn't known", byMaya, followModels.MentionSync{Added: []string{sam}, First: true}, made(after), nil},
		{"a quiet save", Change{Contributors: []string{maya}, Quiet: true}, followModels.MentionSync{Added: []string{sam, jo}, Since: since}, made(before), nil},
		{"the quiet first keeping of a doc made since", Change{Contributors: []string{maya}, Quiet: true}, followModels.MentionSync{Added: []string{sam}, First: true, Since: since}, made(after), nil},
		{"an agent's quiet save", Change{Agents: []AgentContributor{{AgentUUID: jo, AskerUUID: maya}}, Quiet: true}, followModels.MentionSync{Added: []string{sam}}, made(before), nil},
	} {
		if got := whoToTell(c.save, c.kept, c.doc); !slices.Equal(got, c.want) {
			t.Errorf("%s: tells %v, want %v", c.name, got, c.want)
		}
	}
}

// A rename, or a change to who may open or comment, is one event; an update
// that changes nothing of them is none.
func TestAnUpdateIsToldAsWhatItChanged(t *testing.T) {
	yes, no := true, false
	before := &dgraphStruct.DgraphDoc{Uuid: "doc", Title: "Launch notes", IsPrivate: &yes}
	title, same := "Launch plan", "Launch notes"
	got := updateEvents(before, &adapter.InputUpdateDoc{DocId: "doc", Title: &title, IsPrivate: &no})
	if len(got) != 2 || got[0].Kind != resourceEventBusiness.DocRenamed || got[0].Data["title"] != "Launch plan" ||
		got[1].Kind != resourceEventBusiness.DocAccessChanged || got[1].Data["private"] != "false" {
		t.Errorf("rename and open: %+v", got)
	}
	if got := updateEvents(before, &adapter.InputUpdateDoc{DocId: "doc", Title: &same, IsPrivate: &yes}); len(got) != 0 {
		t.Errorf("nothing changed, yet %+v", got)
	}
}

// Sharing tells who gained access, by level; a level changed or taken away is
// an access change; the owner and anyone re-added as they were are neither.
func TestASharingChangeIsToldAsWhatItDid(t *testing.T) {
	before := &dgraphStruct.DgraphDoc{
		EditingUser: []*dgraphStruct.DgraphUser{{Uuid: "owner"}},
		ReadingUser: []*dgraphStruct.DgraphUser{{Uuid: maya}, {Uuid: jo}},
	}
	got := sharingEvents(before, adapter.InputUpdateDocPermissions{DocId: "doc",
		AddEditors: []string{sam, maya, "owner"}, AddViewers: []string{jo}, RemoveViewers: []string{d2}}, "owner")
	if len(got) != 2 {
		t.Fatalf("events %+v", got)
	}
	if got[0].Kind != resourceEventBusiness.DocShared || got[0].Data["level"] != "edit" || !slices.Equal(got[0].Subjects, []string{sam}) {
		t.Errorf("shared %+v", got[0])
	}
	if got[1].Kind != resourceEventBusiness.DocAccessChanged || !slices.Equal(got[1].Subjects, []string{maya}) {
		t.Errorf("maya's level changed: %+v", got[1])
	}
	got = sharingEvents(before, adapter.InputUpdateDocPermissions{DocId: "doc", RemoveViewers: []string{jo}}, "owner")
	if len(got) != 1 || got[0].Kind != resourceEventBusiness.DocAccessChanged || !slices.Equal(got[0].Subjects, []string{jo}) {
		t.Errorf("jo removed: %+v", got)
	}
}
