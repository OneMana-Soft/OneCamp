package business

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	docDomain "github.com/akashc777/OneCamp/domain/Doc"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// ai-search-io.md P1-9: a doc made through the API is titled by the heading
// it starts with when the call names no title, and seeded with the rest.
func TestTheAPIMakesADocFromMarkdown(t *testing.T) {
	w := newWorld(t)
	made, _, err := CreateFromMarkdown(context.Background(), tester, "", "# Launch plan\n\nShip it on **Friday**.", "")
	if err != nil {
		t.Fatal(err)
	}
	if made.Title != "Launch plan" || w.docTitled("Launch plan").parent != "" {
		t.Fatalf("made %+v", made)
	}
	body := w.seeded[made.UUID]
	if !strings.Contains(body, "<strong>Friday</strong>") || strings.Contains(body, "Launch plan") {
		t.Fatalf("seeded %q", body)
	}
	named, _, err := CreateFromMarkdown(context.Background(), tester, "Notes", "# Not the title", "")
	if err != nil || named.Title != "Notes" {
		t.Fatalf("a named doc: %+v, %v", named, err)
	}
}

// A doc the document service couldn't fill is not handed back as made.
func TestTheAPISaysWhenTheDocumentServiceIsDown(t *testing.T) {
	w := newWorld(t)
	w.seedErr = errors.New("collab down")
	if _, _, err := CreateFromMarkdown(context.Background(), tester, "Notes", "Text", ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("with the document service down: %v", err)
	}
	if len(w.deleted) != 1 {
		t.Fatalf("the empty doc stayed: %v", w.deleted)
	}
}

// Only someone who can edit a doc adds to it, as one live append credited to
// them; an agent's addition is marked as its own, its version kept first.
func TestTheAPIAddsToADocAsALiveEditForItsEditors(t *testing.T) {
	w := newWorld(t)
	doc, readOnly, hidden := uuid.NewString(), uuid.NewString(), uuid.NewString()
	w.levels[doc], w.levels[readOnly], w.levels[hidden] = accessBusiness.LevelEdit, accessBusiness.LevelView, accessBusiness.LevelNone

	if _, _, err := AppendMarkdown(context.Background(), tester, readOnly, "Done"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a reader added to a doc: %v", err)
	}
	for _, id := range []string{hidden, uuid.NewString(), "not-a-doc"} {
		if _, _, err := AppendMarkdown(context.Background(), tester, id, "Done"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: %v", id, err)
		}
	}
	if _, _, err := AppendMarkdown(context.Background(), tester, doc, "  \n"); !errors.Is(err, ErrEmpty) {
		t.Fatalf("nothing to add: %v", err)
	}
	if _, _, err := AppendMarkdown(context.Background(), tester, doc, "- [ ] Tag the release"); err != nil {
		t.Fatal(err)
	}
	agent := tester
	agent.AgentUUID = uuid.NewString()
	if _, _, err := AppendMarkdown(context.Background(), agent, doc, "Done"); err != nil {
		t.Fatal(err)
	}
	if len(w.appended) != 2 {
		t.Fatalf("appended %+v", w.appended)
	}
	person, byAgent := w.appended[0], w.appended[1]
	if person.doc != doc || !strings.Contains(person.html, `data-type="taskList"`) || person.opts.AgentEdit || person.opts.By.UserUUID != tester.UserUUID {
		t.Fatalf("a person's addition %+v", person)
	}
	if !byAgent.opts.AgentEdit || !byAgent.opts.SnapshotFirst || byAgent.opts.By.AgentUUID != agent.AgentUUID || byAgent.opts.By.UserUUID != tester.UserUUID {
		t.Fatalf("an agent's addition %+v", byAgent.opts)
	}
}

// A picture written into the Markdown is uploaded to the doc, as an import's
// is, and one the API can't reach (a file beside it) is said to be left out,
// for a new doc and for an addition alike.
func TestTheAPIUploadsAPictureItIsGivenAndSaysWhichItLeftOut(t *testing.T) {
	w := newWorld(t)
	picture := "![Chart](data:image/png;base64," + base64.StdEncoding.EncodeToString(png1x1) + ")"
	made, skipped, err := CreateFromMarkdown(context.Background(), tester, "Report", picture+"\n\n![Map](map.png)", "")
	if err != nil || len(skipped) != 1 || skipped[0].What != "markdown: map.png" {
		t.Fatalf("made %+v, skipped %+v, %v", made, skipped, err)
	}
	if body := w.seeded[made.UUID]; !strings.Contains(body, "/doc/getDocAttachment/"+made.UUID+"/") || strings.Contains(body, "base64") {
		t.Fatalf("seeded %q", body)
	}

	doc := uuid.NewString()
	w.levels[doc] = accessBusiness.LevelEdit
	_, skipped, err = AppendMarkdown(context.Background(), tester, doc, picture+"\n\n![Map](map.png)")
	if err != nil || len(skipped) != 1 || skipped[0].What != "markdown: map.png" {
		t.Fatalf("skipped %+v, %v", skipped, err)
	}
	if added := w.appended[0].html; !strings.Contains(added, "/doc/getDocAttachment/"+doc+"/") || strings.Contains(added, "base64") {
		t.Fatalf("appended %q", added)
	}
}

// ai-search-io.md P1-9: reading a doc through the API is the app's Markdown
// export of it, recorded as an export made through the API.
func TestAnAPIReadIsTheMarkdownExportRecordedAsMadeThroughTheAPI(t *testing.T) {
	w := newWorld(t)
	doc := uuid.NewString()
	w.levels[doc] = accessBusiness.LevelView
	w.bodies[doc] = [2]string{"Runbook", "<p>Restart the queue.</p>"}
	w.useWorkspace(&workspace{rows: []*dgraphStruct.DgraphDoc{row(doc, "Runbook", "", uuid.NewString(), false)}})
	listed, md, err := ReadForAPI(context.Background(), tester, doc)
	if err != nil {
		t.Fatal(err)
	}
	if md != "# Runbook\n\nRestart the queue.\n" || listed.Title != "Runbook" || listed.UUID != doc {
		t.Fatalf("read %+v %q", listed, md)
	}
	var via string
	for _, e := range w.events {
		if e.Kind == resourceEventBusiness.DocExported {
			via = e.Data["via"]
		}
	}
	if via != "api" {
		t.Fatalf("recorded via %q", via)
	}
	if _, _, err := ReadForAPI(context.Background(), tester, uuid.NewString()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a doc they can't open: %v", err)
	}
}

// The API lists the docs the Access Resolver lets the person open, and names
// the page above one only when they can open that page too.
func TestTheAPIListsOnlyDocsThePersonCanOpen(t *testing.T) {
	w := newWorld(t)
	open, closed, hidden, openParent, closedParent := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	w.levels[open], w.levels[closed], w.levels[openParent] = accessBusiness.LevelView, accessBusiness.LevelView, accessBusiness.LevelView
	saved := listDocs
	t.Cleanup(func() { listDocs = saved })
	var asked docDomain.DocListQuery
	listDocs = func(_ context.Context, q docDomain.DocListQuery) (*dgraphStruct.DgraphDocList, error) {
		asked = q
		return &dgraphStruct.DgraphDocList{Count: 30, Docs: []*dgraphStruct.DgraphDoc{
			row(open, "Handbook", openParent, tester.UserUUID, false),
			row(closed, "Notes", closedParent, tester.UserUUID, false),
			row(hidden, "Salaries", "", tester.UserUUID, true),
		}}, nil
	}
	docs, more, err := ListDocs(context.Background(), tester, docDomain.ScopeWorkspace, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if asked.ViewerUID != tester.UserUID || asked.First != 20 || asked.Scope != docDomain.ScopeWorkspace || !more {
		t.Fatalf("asked %+v, more %v", asked, more)
	}
	if len(docs) != 2 || docs[0].UUID != open || docs[0].ParentUUID != openParent || docs[1].UUID != closed || docs[1].ParentUUID != "" {
		t.Fatalf("listed %+v", docs)
	}
}
