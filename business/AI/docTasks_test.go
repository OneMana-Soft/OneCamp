package business

import (
	"context"
	"errors"
	"strings"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

const meetingNotes = `<h1 data-block-id="t">Launch sync</h1>` +
	`<p data-block-id="p1">We agreed the launch is on the 20th.</p>` +
	`<ul data-block-id="l"><li data-block-id="li1"><p data-block-id="b-priya">Priya to send the deck by Friday</p></li>` +
	`<li data-block-id="li2"><p data-block-id="b-jonas">Jonas will book the venue</p></li></ul>` +
	`<ul data-type="taskList" data-block-id="tl"><li data-type="taskItem" data-checked="true" data-block-id="done"><p data-block-id="done-p">Draft the agenda</p></li></ul>`

// fakeTasksDoc makes the doc readable, the model answer with out, and Priya a member.
func fakeTasksDoc(t *testing.T, html, out string) *fakedExtraction {
	t.Helper()
	savedLive, savedSpace := liveDocHTML, docSpaceOf
	t.Cleanup(func() { liveDocHTML, docSpaceOf = savedLive, savedSpace })
	fakeRecord(t)
	fakeDecision(t, accessBusiness.Decision{Level: accessBusiness.LevelView}, nil)
	faked := fakeExtraction(t, out, func(system, user string) {
		if !strings.Contains(system, `{"tasks": [`) {
			t.Error("the prompt must ask for an object with a list in it: JSON mode refuses a bare list")
		}
		if !strings.Contains(user, "Document:\n[1] Launch sync") || !strings.Contains(user, "[3] Priya to send the deck by Friday") || strings.Contains(user, "Draft the agenda") {
			t.Errorf("the doc reached the model as %q", user)
		}
		if !strings.Contains(user, "Fri 2026-10-16") {
			t.Errorf("the coming days are not written out: %q", user)
		}
	})
	liveDocHTML = func(context.Context, string) (string, error) { return html, nil }
	docSpaceOf = func(context.Context, string, string) (string, string) { return "", "" }
	return faked
}

// "Priya to send the deck by Friday" is one task: Priya's, due the coming
// Friday, linked to its own line (ai-search-io P0-6).
func TestPriyaToSendTheDeckByFridayIsOneTask(t *testing.T) {
	fakeTasksDoc(t, meetingNotes, `{"tasks":[{"title":"Send the deck","assignee":"Priya","due":"2026-10-16","priority":"high","block":3}]}`)
	res, err := ExtractDocActionItems(context.Background(), member(), recDoc)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Enabled || len(res.Tasks) != 1 {
		t.Fatalf("got %+v", res)
	}
	got := res.Tasks[0]
	if got.Title != "Send the deck" || got.AssigneeUUID != "u-priya" || got.AssigneeName != "Priya Nair" || got.Due != "2026-10-16T17:00:00Z" || got.Priority != "high" {
		t.Errorf("task: %+v", got)
	}
	if got.BlockID != "b-priya" || got.Quote != "Priya to send the deck by Friday" {
		t.Errorf("its line: %q %q", got.BlockID, got.Quote)
	}
	if res.Lines != 4 {
		t.Errorf("lines read: %d (the done to-do is left out)", res.Lines)
	}
}

// A doc with one action item still gives one task, however the model answers
// it: JSON mode makes small models answer one item as the item alone.
func TestOneActionItemStillGivesOneTask(t *testing.T) {
	for _, out := range []string{
		`{"title":"Send the deck","assignee":"Priya","due":"2026-10-16","block":"[3]"}`,
		`{"tasks":[{"title":"Send the deck","assignee":"Priya","block":3}]}`,
		"```json\n[{\"title\":\"Send the deck\",\"block\":2}]\n```",
	} {
		fakeTasksDoc(t, meetingNotes, out)
		res, err := ExtractDocActionItems(context.Background(), member(), recDoc)
		if err != nil || len(res.Tasks) != 1 || res.Tasks[0].BlockID != "b-priya" {
			t.Errorf("%s: %+v %v", out, res, err)
		}
	}
	fakeTasksDoc(t, meetingNotes, `{"tasks":[]}`)
	if res, _ := ExtractDocActionItems(context.Background(), member(), recDoc); len(res.Tasks) != 0 || res.Note != "No clear action items in this doc." {
		t.Errorf("no items: %+v", res)
	}
}

// A model's line number that doesn't fit the task is replaced by the line whose
// words match best, and a task that matches no line well gets none.
func TestATaskFindsItsLine(t *testing.T) {
	lines, err := contentcodec.Blocks(meetingNotes, contentcodec.BlockOptions{Text: true, SkipDone: true})
	if err != nil {
		t.Fatal(err)
	}
	if l, ok := lineFor(lines, 4, "Book the venue"); !ok || l.ID != "b-jonas" {
		t.Errorf("the numbered line: %+v %v", l, ok)
	}
	// A small model's number off by one: the task's own words find the line.
	if l, ok := lineFor(lines, 2, "Send the deck"); !ok || l.ID != "b-priya" {
		t.Errorf("an off-by-one number: %+v %v", l, ok)
	}
	if l, ok := lineFor(lines, 1, "Book the venue for the launch"); !ok || l.ID != "b-jonas" {
		t.Errorf("a wrong number: %+v %v", l, ok)
	}
	if l, ok := lineFor(lines, 0, "Renew the domain"); ok {
		t.Errorf("a task from nowhere was put on %+v", l)
	}
}

// A doc the person can't read is refused before the model is asked for, so it
// spends none of their AI requests either.
func TestAPersonWhoCantReadTheDocGetsNoActionItems(t *testing.T) {
	faked := fakeTasksDoc(t, meetingNotes, `{"tasks":[]}`)
	fakeDecision(t, accessBusiness.Decision{Level: accessBusiness.LevelNone}, nil)
	if _, err := ExtractDocActionItems(context.Background(), member(), recDoc); !errors.Is(err, ErrDocAINotFound) || faked.models != 0 || faked.chats != 0 {
		t.Errorf("err=%v, model asked for %d times, asked %d times", err, faked.models, faked.chats)
	}
}

// fakeTaskMaking makes tasks in memory; the project answers as given.
func fakeTaskMaking(t *testing.T, project *dgraphStruct.DgraphProject) (*[]string, *[]string) {
	t.Helper()
	savedP, savedMake, savedLink, savedTitle := projectForTasks, makeDocTask, linkTaskToDoc, docTitle
	t.Cleanup(func() {
		projectForTasks, makeDocTask, linkTaskToDoc, docTitle = savedP, savedMake, savedLink, savedTitle
	})
	projectForTasks = func(context.Context, string, string) (*dgraphStruct.DgraphProject, error) { return project, nil }
	var made, links []string
	makeDocTask = func(_ context.Context, _ *userModels.UserInfo, _ uuid.UUID, _ *dgraphStruct.DgraphProject, task ProposedTask, description string) (uuid.UUID, error) {
		made = append(made, task.Title+"|"+description)
		return uuid.New(), nil
	}
	linkTaskToDoc = func(_ context.Context, task, doc string) error {
		links = append(links, doc)
		return nil
	}
	docTitle = func(context.Context, string, string) string { return `Launch <sync>` }
	return &made, &links
}

const billing = "9d4c3b2a-1f0e-4d5c-8b7a-6f5e4d3c2b1a"

// Each task made from a doc links back to the doc and its line, and the making
// is recorded.
func TestTasksFromADocLinkBackToTheirLine(t *testing.T) {
	rows := fakeRecord(t)
	fakeDecision(t, accessBusiness.Decision{Level: accessBusiness.LevelView}, nil)
	made, links := fakeTaskMaking(t, &dgraphStruct.DgraphProject{Uuid: billing, Name: "Launch", IsProjectAdmin: 1})
	res, err := CreateDocTasks(context.Background(), member(), recDoc, billing, []DocProposedTask{
		{ProposedTask: ProposedTask{Title: "Send the deck", Description: "For the board"}, BlockID: "b-priya"},
		{ProposedTask: ProposedTask{Title: "Book the venue"}},
	})
	if err != nil || res.Created != 2 || len(res.Tasks) != 2 || res.Tasks[0].BlockID != "b-priya" || res.Project.Name != "Launch" {
		t.Fatalf("%+v %v", res, err)
	}
	if want := `Send the deck|<p>From <a href="/app/doc/` + recDoc + `#block-b-priya">Launch &lt;sync&gt;</a></p><p>For the board</p>`; (*made)[0] != want {
		t.Errorf("the description is\n%s\nwant\n%s", (*made)[0], want)
	}
	if !strings.Contains((*made)[1], `href="/app/doc/`+recDoc+`"`) {
		t.Errorf("a task with no line links to the doc: %s", (*made)[1])
	}
	if len(*links) != 2 || (*links)[0] != recDoc {
		t.Errorf("the doc is not listed on the tasks: %v", *links)
	}
	if len(*rows) != 1 || (*rows)[0].action != ActionDocAITasksCreated || (*rows)[0].summary != "Made 2 tasks in Launch from a doc's action items" {
		t.Errorf("record: %+v", *rows)
	}
}

// Someone who can't add tasks to a project can't have them made there, even
// with a forged request, and the refusal is in their activity: a viewer is
// told the project's name, someone outside it isn't.
func TestTasksAreRefusedInAProjectThePersonCantAddTo(t *testing.T) {
	for _, c := range []struct {
		project *dgraphStruct.DgraphProject
		role    workAccess.Role
		want    string
	}{
		{&dgraphStruct.DgraphProject{Uuid: billing, Name: "Billing", IsProjectMember: 1}, workAccess.RoleViewer, "You can't add tasks to Billing, so none were made."},
		{&dgraphStruct.DgraphProject{Uuid: billing, Name: "Billing"}, workAccess.RoleNone, "You can't add tasks to that project, so none were made."},
		{nil, workAccess.RoleNone, "You can't add tasks to that project, so none were made."},
	} {
		rows := fakeRecord(t)
		fakeDecision(t, accessBusiness.Decision{Level: accessBusiness.LevelEdit}, nil)
		made, _ := fakeTaskMaking(t, c.project)
		saved := standingIn
		standingIn = func(context.Context, string, *dgraphStruct.DgraphProject) (workAccess.Standing, error) {
			return workAccess.Standing{ProjectUUID: billing, Role: c.role, EditWho: workAccess.EditMembers}, nil
		}
		t.Cleanup(func() { standingIn = saved })
		_, err := CreateDocTasks(context.Background(), member(), recDoc, billing, []DocProposedTask{{ProposedTask: ProposedTask{Title: "Send the deck"}}})
		var cant *CantAddTasksError
		if !errors.As(err, &cant) || err.Error() != c.want {
			t.Errorf("got %v, want %q", err, c.want)
		}
		if len(*made) != 0 {
			t.Errorf("tasks were made: %v", *made)
		}
		if len(*rows) != 1 || (*rows)[0].action != ActionDocAITasksRefused || (*rows)[0].meta["project_uuid"] != billing {
			t.Errorf("the refusal was not recorded: %+v", *rows)
		}
	}
}
