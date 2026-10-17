package entityLinkBusiness

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

const (
	openDoc   = "11111111-1111-4111-8111-111111111111"
	closedDoc = "22222222-2222-4222-8222-222222222222"
	goneDoc   = "33333333-3333-4333-8333-333333333333"
	wasNever  = "44444444-4444-4444-8444-444444444444"
	hereDoc   = "55555555-5555-4555-8555-555555555555"
	space     = "66666666-6666-4666-8666-666666666666"
	editDoc   = "77777777-7777-4777-8777-777777777777"
	talkDoc   = "88888888-8888-4888-8888-888888888888"
)

// reader is who reads in these tests, and maya someone else in the workspace.
var (
	reader = accessBusiness.Subject{UserUID: "0x1", UserUUID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}
	maya   = &dgraphStruct.DgraphUser{Uid: "0x2", Uuid: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", UserName: "Maya"}
)

// rules decides by a table: each person's level on each doc, and the docs in the trash.
type rules struct {
	accessBusiness.Resolver
	levels map[string]map[string]accessBusiness.Level // person uuid -> doc -> level
	gone   map[string]bool
}

func (r rules) Resolve(_ context.Context, who accessBusiness.Subject, res accessBusiness.Resource) (accessBusiness.Decision, error) {
	l, ok := r.levels[who.UserUUID][res.UUID]
	if !ok && !r.gone[res.UUID] {
		return accessBusiness.Decision{}, accessBusiness.ErrNotFound
	}
	if r.gone[res.UUID] {
		return accessBusiness.Decision{Level: accessBusiness.LevelNone, Deleted: true, Was: l}, nil
	}
	return accessBusiness.Decision{Level: l, Was: l}, nil
}

func withRules(t *testing.T) {
	saved, savedChips := accessBusiness.Rules, readDocChips
	t.Cleanup(func() { accessBusiness.Rules, readDocChips = saved, savedChips })
	accessBusiness.Rules = rules{
		levels: map[string]map[string]accessBusiness.Level{
			reader.UserUUID: {openDoc: accessBusiness.LevelView, hereDoc: accessBusiness.LevelFull, closedDoc: accessBusiness.LevelNone, goneDoc: accessBusiness.LevelEdit,
				editDoc: accessBusiness.LevelEdit, talkDoc: accessBusiness.LevelComment},
			maya.Uuid: {openDoc: accessBusiness.LevelView, hereDoc: accessBusiness.LevelNone, editDoc: accessBusiness.LevelNone, talkDoc: accessBusiness.LevelNone},
		},
		gone: map[string]bool{goneDoc: true, wasNever: true},
	}
	all := map[string]*dgraphStruct.DgraphDoc{
		openDoc:   {Uuid: openDoc, Title: "Launch plan", Icon: "🚀", Parent: &dgraphStruct.DgraphDoc{Uuid: hereDoc}, SpaceKind: "project", SpaceUUID: space},
		closedDoc: {Uuid: closedDoc, Title: "Salaries"},
		goneDoc:   {Uuid: goneDoc, Title: "Old notes"},
		hereDoc:   {Uuid: hereDoc, Title: "Here"},
	}
	readDocChips = func(_ context.Context, ids []string) (map[string]*dgraphStruct.DgraphDoc, error) {
		out := map[string]*dgraphStruct.DgraphDoc{}
		for _, id := range ids {
			if d := all[strings.ToLower(id)]; d != nil {
				out[strings.ToLower(id)] = d
			}
		}
		return out, nil
	}
}

func TestADocChipNeverShowsWhatItsReaderCantOpen(t *testing.T) {
	withRules(t)
	got, err := ReadEntities(context.Background(), reader, []Ref{
		{Kind: "doc", ID: strings.ToUpper(openDoc)}, {Kind: "doc", ID: closedDoc}, {Kind: "doc", ID: goneDoc},
		{Kind: "doc", ID: wasNever}, {Kind: "doc", ID: "not-a-doc"}, {Kind: "spaceship", ID: openDoc},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []Entity{
		{Kind: "doc", ID: strings.ToUpper(openDoc), Access: AccessOK, Title: "Launch plan", Topic: helpers.GetMqttTopicForDoc(openDoc), Data: map[string]any{
			"title": "Launch plan", "icon": "🚀", "parent_id": hereDoc, "space_kind": "project", "space_id": space,
		}},
		{Kind: "doc", ID: closedDoc, Access: AccessNone},
		{Kind: "doc", ID: goneDoc, Access: AccessGone},
		{Kind: "doc", ID: wasNever, Access: AccessNone},
		{Kind: "doc", ID: "not-a-doc", Access: AccessNone},
		{Kind: "spaceship", ID: openDoc, Access: AccessNone},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

// project is a project as one reader stands in it.
func project(uuid string, member, admin uint8) *dgraphStruct.DgraphProject {
	return &dgraphStruct.DgraphProject{Uuid: uuid, Name: "Launch", IsProjectMember: member, IsProjectAdmin: admin}
}

func TestATaskChipFollowsTheTaskViewsRule(t *testing.T) {
	const (
		member   = "a0000000-0000-4000-8000-000000000001"
		admin    = "a0000000-0000-4000-8000-000000000002"
		outside  = "a0000000-0000-4000-8000-000000000003"
		assigned = "a0000000-0000-4000-8000-000000000004"
		deleted  = "a0000000-0000-4000-8000-000000000005"
		archived = "a0000000-0000-4000-8000-000000000006"
		proj     = "b0000000-0000-4000-8000-000000000001"
	)
	saved := readTaskChips
	t.Cleanup(func() { readTaskChips = saved })
	due := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	gone := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	readTaskChips = func(_ context.Context, ids []string, readerUID string) ([]*dgraphStruct.DgraphTask, error) {
		if readerUID != reader.UserUID {
			t.Fatalf("read for %q", readerUID)
		}
		return []*dgraphStruct.DgraphTask{
			{Uuid: member, Name: "Write the post", Status: dgraphStruct.TASK_STATUS_INPROGRESS, DueDate: &due, Project: project(proj, 1, 0), Assignee: maya},
			{Uuid: admin, Name: "Ship it", Status: dgraphStruct.TASK_STATUS_DONE, Project: project(proj, 1, 1)},
			{Uuid: outside, Name: "Salaries", Status: dgraphStruct.TASK_STATUS_TODO, Project: project(proj, 0, 0)},
			{Uuid: assigned, Name: "Review", Status: dgraphStruct.TASK_STATUS_TODO, Project: project(proj, 0, 0), Assignee: &dgraphStruct.DgraphUser{Uid: reader.UserUID}},
			{Uuid: deleted, Name: "Old", Status: dgraphStruct.TASK_STATUS_TODO, Project: project(proj, 1, 1), DeletedAt: &gone},
			{Uuid: archived, Name: "In a deleted project", Status: dgraphStruct.TASK_STATUS_TODO, Project: &dgraphStruct.DgraphProject{Uuid: proj, IsProjectMember: 1, DeletedAt: &gone}},
		}, nil
	}
	got, err := ReadEntities(context.Background(), reader, []Ref{{"task", member}, {"task", admin}, {"task", outside}, {"task", assigned}, {"task", deleted}, {"task", archived}})
	if err != nil {
		t.Fatal(err)
	}
	topic := helpers.GetMqttTopicForProjectMessage(proj)
	byID := map[string]Entity{}
	for _, e := range got {
		byID[e.ID] = e
	}
	if e := byID[member]; e.Access != AccessOK || e.Topic != topic || e.Data["can_edit"] != false || e.Data["status"] != dgraphStruct.TASK_STATUS_INPROGRESS ||
		e.Data["due"] != "2026-10-20" || e.Data["assignee"].(map[string]any)["name"] != "Maya" || e.Data["project_id"] != proj {
		t.Errorf("a member's task: %+v", e)
	}
	if e := byID[admin]; e.Access != AccessOK || e.Data["can_edit"] != true || e.Data["done"] != true {
		t.Errorf("an admin's task: %+v", e)
	}
	if e := byID[outside]; e.Access != AccessNone || e.Data != nil {
		t.Errorf("a task in a project the reader isn't in told them: %+v", e)
	}
	// The assignee sees their task, but can't listen on a project they're not in.
	if e := byID[assigned]; e.Access != AccessOK || e.Topic != "" {
		t.Errorf("an assignee's task: %+v", e)
	}
	if e := byID[deleted]; e.Access != AccessGone || e.Data != nil {
		t.Errorf("a deleted task: %+v", e)
	}
	if e := byID[archived]; e.Access != AccessGone || e.Data != nil || e.Title != "" {
		t.Errorf("a task in a deleted project: %+v", e)
	}
}

func TestAProjectChipIsForItsMembers(t *testing.T) {
	const (
		in     = "c0000000-0000-4000-8000-000000000001"
		out    = "c0000000-0000-4000-8000-000000000002"
		closed = "c0000000-0000-4000-8000-000000000003"
	)
	saved := readProjectChips
	t.Cleanup(func() { readProjectChips = saved })
	gone := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	readProjectChips = func(context.Context, []string, string) ([]*dgraphStruct.DgraphProject, error) {
		p := project(closed, 1, 0)
		p.DeletedAt = &gone
		return []*dgraphStruct.DgraphProject{project(in, 0, 1), project(out, 0, 0), p}, nil
	}
	got, err := ReadEntities(context.Background(), reader, []Ref{{"project", in}, {"project", out}, {"project", closed}})
	if err != nil {
		t.Fatal(err)
	}
	want := []Entity{
		{Kind: "project", ID: in, Access: AccessOK, Title: "Launch", Data: map[string]any{"name": "Launch", "can_edit": true}, Topic: helpers.GetMqttTopicForProjectMessage(in)},
		{Kind: "project", ID: out, Access: AccessNone},
		{Kind: "project", ID: closed, Access: AccessGone},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestAPersonChipNamesThemAsTheyAreNamedEverywhere(t *testing.T) {
	saved := readPeople
	t.Cleanup(func() { readPeople = saved })
	readPeople = func(context.Context, []string) ([]*dgraphStruct.DgraphUser, error) {
		return []*dgraphStruct.DgraphUser{{Uid: "0x2", Uuid: maya.Uuid, UserName: "", UserFullName: "Maya Chen", IsBot: true}}, nil
	}
	got, err := ReadEntities(context.Background(), reader, []Ref{{"person", strings.ToUpper(maya.Uuid)}, {"person", wasNever}})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Access != AccessOK || got[0].Data["name"] != "Maya Chen" || got[0].Data["is_bot"] != true || got[1].Access != AccessNone {
		t.Fatalf("got %+v", got)
	}
}

func TestAReadTakesAtMostMaxRefs(t *testing.T) {
	if _, err := ReadEntities(context.Background(), accessBusiness.Subject{}, make([]Ref, MaxRefs+1)); err != ErrTooMany {
		t.Fatalf("got %v", err)
	}
}

func TestAKindIsRegisteredOnce(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a second reader for docs was taken")
		}
	}()
	RegisterKind("doc", readDocs)
}

func TestTheAtListLearnsWhoCantOpenTheDoc(t *testing.T) {
	withRules(t)
	got, err := MentionAccessFor(context.Background(), reader, hereDoc, []string{maya.Uuid, reader.UserUUID, maya.Uuid})
	if err != nil || !reflect.DeepEqual(got, MentionAccess{CanShare: true, CantOpen: []string{maya.Uuid}}) {
		t.Fatalf("got %+v %v", got, err)
	}
	got, err = MentionAccessFor(context.Background(), reader, openDoc, []string{maya.Uuid})
	if !errors.Is(err, ErrNoDoc) {
		t.Fatalf("a reader who can't edit the doc was told who can open it: %+v %v", got, err)
	}
	if _, err := MentionAccessFor(context.Background(), reader, hereDoc, make([]string, MaxMentionChecks+1)); !errors.Is(err, ErrTooManyPeople) {
		t.Fatalf("got %v", err)
	}
}

// Sharing with someone just mentioned is an invite, so the "@" list offers it
// by the invite's rule (MayInvite): to an editor, as the share dialog's Invite
// does, and to full access as before. A commenter writes in no doc's body, so
// the list asks nothing for them.
func TestTheAtListOffersSharingByTheInviteRule(t *testing.T) {
	withRules(t)
	for _, c := range []struct {
		name, doc string
		want      MentionAccess
		err       error
	}{
		{"full access", hereDoc, MentionAccess{CanShare: true, CantOpen: []string{maya.Uuid}}, nil},
		{"an editor", editDoc, MentionAccess{CanShare: true, CantOpen: []string{maya.Uuid}}, nil},
		{"a commenter", talkDoc, MentionAccess{}, ErrNoDoc},
	} {
		got, err := MentionAccessFor(context.Background(), reader, c.doc, []string{maya.Uuid})
		if !errors.Is(err, c.err) || (c.err == nil && !reflect.DeepEqual(got, c.want)) {
			t.Errorf("%s: %+v %v, want %+v %v", c.name, got, err, c.want, c.err)
		}
	}
}

// The titles anything names a thing by are the chips' reads: the same rule,
// for every reader asked about at once.
func TestTitlesAreReadAsChipsAre(t *testing.T) {
	withRules(t)
	mayaReads := accessBusiness.Subject{UserUID: maya.Uid, UserUUID: maya.Uuid}
	refs := []accessBusiness.ItemRef{{Kind: "doc", ID: openDoc}, {Kind: "doc", ID: hereDoc}, {Kind: "doc", ID: closedDoc}, {Kind: "doc", ID: goneDoc}}

	got, err := accessBusiness.ReadableTitles(context.Background(), []accessBusiness.Subject{reader}, refs)
	want := map[accessBusiness.ItemRef]string{{Kind: "doc", ID: openDoc}: "Launch plan", {Kind: "doc", ID: hereDoc}: "Here"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("for the reader: %v %v", got, err)
	}
	// Here is the reader's alone: asked for both of them, it isn't named.
	got, err = accessBusiness.ReadableTitles(context.Background(), []accessBusiness.Subject{reader, mayaReads}, refs)
	if err != nil || !reflect.DeepEqual(got, map[accessBusiness.ItemRef]string{{Kind: "doc", ID: openDoc}: "Launch plan"}) {
		t.Fatalf("for the reader and Maya: %v %v", got, err)
	}
}

// For every kind a chip draws, ReadableTitles names a thing exactly when its
// chip says the reader may open it, by the chip's title: one read, one rule.
func TestEveryKindsTitlesAreItsChips(t *testing.T) {
	withRules(t)
	const (
		task   = "d0000000-0000-4000-8000-000000000001"
		hidden = "d0000000-0000-4000-8000-000000000002"
		proj   = "d0000000-0000-4000-8000-000000000003"
	)
	savedTasks, savedProjects, savedPeople := readTaskChips, readProjectChips, readPeople
	t.Cleanup(func() { readTaskChips, readProjectChips, readPeople = savedTasks, savedProjects, savedPeople })
	readTaskChips = func(context.Context, []string, string) ([]*dgraphStruct.DgraphTask, error) {
		return []*dgraphStruct.DgraphTask{
			{Uuid: task, Name: "Write the post", Project: project(proj, 1, 0)},
			{Uuid: hidden, Name: "Salaries review", Project: project(proj, 0, 0)},
		}, nil
	}
	readProjectChips = func(context.Context, []string, string) ([]*dgraphStruct.DgraphProject, error) {
		return []*dgraphStruct.DgraphProject{project(proj, 0, 1)}, nil
	}
	readPeople = func(context.Context, []string) ([]*dgraphStruct.DgraphUser, error) {
		return []*dgraphStruct.DgraphUser{maya}, nil
	}

	refs := []Ref{{"doc", openDoc}, {"doc", closedDoc}, {"doc", goneDoc}, {"task", task}, {"task", hidden}, {"project", proj}, {"person", maya.Uuid}, {"person", wasNever}}
	chips, err := ReadEntities(context.Background(), reader, refs)
	if err != nil {
		t.Fatal(err)
	}
	items := make([]accessBusiness.ItemRef, len(refs))
	for i, r := range refs {
		items[i] = accessBusiness.ItemRef{Kind: r.Kind, ID: r.ID}
	}
	titles, err := accessBusiness.ReadableTitles(context.Background(), []accessBusiness.Subject{reader}, items)
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range chips {
		title, named := titles[items[i]]
		if named != (c.Access == AccessOK) || title != c.Title {
			t.Errorf("%s %s: the chip says %q (%q), the titles %v (%q)", c.Kind, c.ID, c.Access, c.Title, named, title)
		}
	}
	if len(titles) != 4 {
		t.Errorf("named %d of the 4 the reader may open: %v", len(titles), titles)
	}
}

// Linking a doc to a task asks whether the doc may be named to its reader.
func TestADocCanBeLinkedOnlyBySomeoneWhoMayOpenIt(t *testing.T) {
	withRules(t)
	for doc, want := range map[string]bool{openDoc: true, strings.ToUpper(hereDoc): true, closedDoc: false, goneDoc: false, wasNever: false} {
		got, err := CanReadRef(context.Background(), RefDoc, doc, reader.UserUID, reader.UserUUID)
		if err != nil || got != want {
			t.Errorf("%s: %v %v, want %v", doc, got, err, want)
		}
	}
}
