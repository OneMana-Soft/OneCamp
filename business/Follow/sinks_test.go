package business

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	followModels "github.com/akashc777/OneCamp/models/postgres/Follow"
	"github.com/google/uuid"
)

type auditRow_ struct {
	actor                           *uuid.UUID
	kind, action, category, summary string
	meta                            map[string]interface{}
}

// Every audited doc event writes one row, under "docs", saying who acted (a
// person, an agent or the server) and never a private page's title.
func TestEveryAuditedDocEventWritesOneRow(t *testing.T) {
	var mu sync.Mutex
	var rows []auditRow_
	oldR, oldE, oldP := auditRecord, actorEmail, docPeople
	t.Cleanup(func() { auditRecord, actorEmail, docPeople = oldR, oldE, oldP })
	auditRecord = func(_ context.Context, actor *uuid.UUID, _ string, kind, action, category, summary string, meta map[string]interface{}) error {
		mu.Lock()
		defer mu.Unlock()
		rows = append(rows, auditRow_{actor, kind, action, category, summary, meta})
		return nil
	}
	actorEmail = func(context.Context, uuid.UUID) string { return "akash@example.test" }
	docPeople = func(context.Context, string) (*dgraphStruct.DgraphDoc, error) { return privateDoc(), nil }

	ctx := context.Background()
	for kind := range AuditedKinds {
		e := resourceEventBusiness.Event{Kind: kind, ResourceKind: "doc", ResourceUUID: docID, Actor: resourceEventBusiness.Actor{UserUUID: ownerID}, At: noon}
		if !isAudited(e) {
			t.Errorf("%s isn't audited", kind)
		}
		record(ctx, e)
	}
	if len(rows) != len(AuditedKinds) {
		t.Fatalf("%d events wrote %d rows", len(AuditedKinds), len(rows))
	}
	for _, r := range rows {
		if r.category != auditBusiness.CategoryDocs || r.kind != auditBusiness.ActorHuman || r.actor == nil || r.actor.String() != ownerID {
			t.Errorf("%s: %+v", r.action, r)
		}
		if strings.Contains(r.summary, "Launch notes") || !strings.Contains(r.summary, "a private page") && r.action != "doc.purged" {
			t.Errorf("%s names the private page: %q", r.action, r.summary)
		}
	}

	// An agent's change says so, beside the person who asked it; the server's
	// own is the system's.
	rows = nil
	record(ctx, resourceEventBusiness.Event{Kind: resourceEventBusiness.DocDeleted, ResourceKind: "doc", ResourceUUID: docID,
		Actor: resourceEventBusiness.Actor{UserUUID: ownerID, AgentUUID: "00000000-0000-4000-8000-0000000000b7"}, At: noon})
	record(ctx, resourceEventBusiness.Event{Kind: resourceEventBusiness.DocPurged, ResourceKind: "doc", ResourceUUID: docID,
		Actor: resourceEventBusiness.Actor{System: true}, At: noon})
	if rows[0].kind != auditBusiness.ActorAgent || rows[0].action != "agent.doc.deleted" || rows[0].meta["agent_uuid"] == nil ||
		rows[1].kind != auditBusiness.ActorSystem || rows[1].actor != nil || rows[1].action != "doc.purged" {
		t.Errorf("an agent's and the server's rows: %+v", rows)
	}
	if rows[1].summary != "Deleted a page for good" {
		t.Errorf("a purge names a doc that's gone by id alone: %q", rows[1].summary)
	}
	if isAudited(resourceEventBusiness.Event{Kind: resourceEventBusiness.DocUpdated, ResourceKind: "doc"}) {
		t.Error("every save would be a row")
	}
}

// A shared doc's row says with how many people, and an open doc is named.
func TestAnAuditRowNamesAnOpenDocAndHowManyItWasSharedWith(t *testing.T) {
	no := false
	doc := privateDoc()
	doc.IsPrivate = &no
	summary, meta := auditRow(resourceEventBusiness.Event{Kind: resourceEventBusiness.DocShared, ResourceKind: "doc", ResourceUUID: docID,
		Subjects: []string{readID, talkID}, Data: map[string]string{"level": "edit", "title": "Launch notes"}}, doc)
	if summary != "Shared ‘Launch notes’ with 2 people who can edit" || meta["people"] != readID+","+talkID || meta["title"] != "Launch notes" {
		t.Errorf("%q %v", summary, meta)
	}
	_, meta = auditRow(resourceEventBusiness.Event{Kind: resourceEventBusiness.DocRenamed, ResourceKind: "doc", ResourceUUID: docID,
		Data: map[string]string{"title": "Secret plan"}}, privateDoc())
	if meta["title"] != nil {
		t.Error("a private page's title went into the log's metadata")
	}
}

// A share's row says what the people it reached can do, in words, not the
// level's code (DESIGN-BUILD-8, 23 Oct 2026).
func TestAShareRowSaysWhatItsPeopleCanDo(t *testing.T) {
	for _, c := range []struct {
		level  string
		people []string
		want   string
	}{
		{"view", []string{readID}, "Shared a private page with 1 person who can view"},
		{"comment", []string{readID, talkID}, "Shared a private page with 2 people who can comment"},
		{"full", []string{readID}, "Shared a private page with 1 person who has full access"},
		{"full", []string{readID, talkID}, "Shared a private page with 2 people who have full access"},
	} {
		summary, _ := auditRow(resourceEventBusiness.Event{Kind: resourceEventBusiness.DocShared, ResourceKind: "doc", ResourceUUID: docID,
			Subjects: c.people, Data: map[string]string{"level": c.level}}, privateDoc())
		if summary != c.want {
			t.Errorf("%s with %d: %q, want %q", c.level, len(c.people), summary, c.want)
		}
	}
}

// Open tabs hear what they need of a change, and nothing about who.
func TestADocsMetaEventCarriesWhatATabActsOn(t *testing.T) {
	m := docMeta(resourceEventBusiness.Event{Kind: resourceEventBusiness.DocRenamed, ResourceKind: "doc", ResourceUUID: docID,
		Actor: resourceEventBusiness.Actor{UserUUID: ownerID}, At: noon, Subjects: []string{readID}, Data: map[string]string{"title": "Launch plan", "secret": "x"}})
	if m.Kind != "doc.renamed" || m.DocUuid != docID || m.ActorUuid != ownerID || m.Data["title"] != "Launch plan" || len(m.Data) != 1 {
		t.Errorf("rename meta %+v", m)
	}
	m = docMeta(resourceEventBusiness.Event{Kind: resourceEventBusiness.DocShared, ResourceKind: "doc", ResourceUUID: docID, Subjects: []string{readID},
		Data: map[string]string{"level": "edit"}})
	if m.Data != nil {
		t.Errorf("a share's meta says who or how: %+v", m.Data)
	}
	if isMeta(resourceEventBusiness.Event{Kind: resourceEventBusiness.DocUpdated, ResourceKind: "doc"}) {
		t.Error("body saves reach open editors through the collaboration service, not as meta")
	}
}

// Thirty saves in a minute send one doc.updated, when the minute closes; a
// save after it opens the next.
func TestThirtySavesInAMinuteSendOneWebhook(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	oldD, oldC, oldU, oldP := dispatchWebhook, clock, updates, docPeople
	t.Cleanup(func() { dispatchWebhook, clock, updates, docPeople = oldD, oldC, oldU, oldP })
	updates = &pendingUpdates{due: map[string]time.Time{}}
	dispatchWebhook = func(_ context.Context, kind string, data map[string]interface{}) {
		mu.Lock()
		defer mu.Unlock()
		sent = append(sent, kind+" "+data["doc_uuid"].(string))
	}
	docPeople = func(context.Context, string) (*dgraphStruct.DgraphDoc, error) { return privateDoc(), nil }
	now := noon
	clock = func() time.Time { return now }

	ctx := context.Background()
	for i := 0; i < 30; i++ {
		now = noon.Add(time.Duration(i) * 2 * time.Second)
		sendWebhook(ctx, resourceEventBusiness.Event{Kind: resourceEventBusiness.DocUpdated, ResourceKind: "doc", ResourceUUID: docID, At: now})
		_ = flushUpdates(ctx) // the sweep runs meanwhile, and the minute is still open
	}
	if len(sent) != 0 {
		t.Fatalf("sent %v while the minute was open", sent)
	}
	now = noon.Add(time.Minute)
	_ = flushUpdates(ctx)
	_ = flushUpdates(ctx)
	if len(sent) != 1 || sent[0] != "doc.updated "+docID {
		t.Fatalf("thirty saves sent %v", sent)
	}
	sendWebhook(ctx, resourceEventBusiness.Event{Kind: resourceEventBusiness.DocUpdated, ResourceKind: "doc", ResourceUUID: docID, At: now})
	now = now.Add(time.Minute)
	_ = flushUpdates(ctx)
	if len(sent) != 2 {
		t.Errorf("the next minute's save sent %v", sent)
	}
}

// A webhook's payload never names a private doc, and names an open one.
func TestAWebhookNamesOnlyAnOpenDoc(t *testing.T) {
	e := resourceEventBusiness.Event{Kind: resourceEventBusiness.DocShared, ResourceKind: "doc", ResourceUUID: docID,
		Actor: resourceEventBusiness.Actor{UserUUID: ownerID}, At: noon, Subjects: []string{readID}, Data: map[string]string{"level": "view"}}
	p := webhookPayload(e, privateDoc())
	if _, named := p["doc_title"]; named || p["doc_private"] != true || p["level"] != "view" {
		t.Errorf("private doc payload %v", p)
	}
	no := false
	open := privateDoc()
	open.IsPrivate = &no
	if p := webhookPayload(e, open); p["doc_title"] != "Launch notes" {
		t.Errorf("open doc payload %v", p)
	}
	for _, k := range []resourceEventBusiness.Kind{"doc.created", "doc.updated", "doc.deleted", "doc.shared", "doc.comment.created"} {
		if !isWebhook(resourceEventBusiness.Event{Kind: k, ResourceKind: "doc"}) {
			t.Errorf("%s is no webhook", k)
		}
	}
}

// The inbox shows a notice's doc by title only to a reader who can open it,
// and never anyone's email.
func TestTheInboxNamesOnlyWhatItsReaderCanOpen(t *testing.T) {
	rows := []*followModels.Notice{
		{ID: uuid.New(), Kind: "doc.mentioned", ResourceKind: "doc", ResourceUUID: docID, ActorUUID: ownerID, Reason: "mention", CreatedAt: noon},
	}
	titles := map[string]string{docID: "Launch notes"}
	actor := user(ownerID)
	actor.EmailID = "owner@example.test"
	byID := map[string]*dgraphStruct.DgraphUser{ownerID: actor}

	// The resolver says the reader may open it.
	items := noticeItems(rows, map[string]bool{docID: true}, titles, byID)
	n := items[0].Notice
	if items[0].ActivityType != "NOTICE" || !n.CanOpen || n.Title != "Launch notes" || n.Actor == nil || n.Actor.EmailID != "" {
		t.Errorf("a reader's item %+v (actor %+v)", n, n.Actor)
	}
	if actor.EmailID == "" {
		t.Error("reading the item changed the person it read")
	}
	// It says they may not, or couldn't say: never named, never opened.
	for _, readable := range []map[string]bool{{docID: false}, nil} {
		items = noticeItems(rows, readable, titles, byID)
		if n := items[0].Notice; n.CanOpen || n.Title != "" {
			t.Errorf("someone who can't open the doc sees %+v", n)
		}
	}
}

// A proposal's events reach open tabs with its id, its status and who made
// it, never its text, so an open doc shows a suggestion made elsewhere at once.
func TestProposalEventsReachOpenTabsWithIdsOnly(t *testing.T) {
	for _, k := range []resourceEventBusiness.Kind{resourceEventBusiness.DocProposalCreated, resourceEventBusiness.DocProposalAccepted,
		resourceEventBusiness.DocProposalDeclined, resourceEventBusiness.DocProposalWithdrawn} {
		e := resourceEventBusiness.Event{Kind: k, ResourceKind: "doc", ResourceUUID: docID,
			Data: map[string]string{"proposal_id": "p1", "status": "open", "author_kind": "person", "quote": "never", "hunks": "never"}}
		if !isMeta(e) {
			t.Errorf("%s doesn't reach open tabs", k)
		}
		m := docMeta(e)
		if m.Kind != string(k) || m.Data["proposal_id"] != "p1" || m.Data["status"] != "open" || m.Data["author_kind"] != "person" || len(m.Data) != 3 {
			t.Errorf("%s meta %+v", k, m.Data)
		}
	}
	// A proposal on anything but a doc isn't a doc's meta event.
	if isMeta(resourceEventBusiness.Event{Kind: resourceEventBusiness.DocProposalCreated, ResourceKind: "table"}) {
		t.Error("a table's proposal reached a doc's topic")
	}
}

// A thread's events reach open tabs with its ids, and the log; an export says
// what it made and where.
func TestThreadEventsReachOpenTabsAndTheLog(t *testing.T) {
	m := docMeta(resourceEventBusiness.Event{Kind: resourceEventBusiness.ThreadReanchored, ResourceKind: "doc", ResourceUUID: docID,
		Data: map[string]string{"thread_id": "t1", "anchor_kind": "yrange", "status": "open", "body": "never"}})
	if m.Data["thread_id"] != "t1" || m.Data["anchor_kind"] != "yrange" || m.Data["body"] != "" {
		t.Errorf("thread meta %+v", m.Data)
	}
	for _, k := range []resourceEventBusiness.Kind{"thread.created", "thread.replied", "thread.resolved", "thread.reopened", "thread.reanchored"} {
		e := resourceEventBusiness.Event{Kind: k, ResourceKind: "doc"}
		if !isMeta(e) || !isAudited(e) {
			t.Errorf("%s: meta %v, audited %v", k, isMeta(e), isAudited(e))
		}
	}
	no := false
	open := privateDoc()
	open.IsPrivate = &no
	summary, _ := auditRow(resourceEventBusiness.Event{Kind: resourceEventBusiness.DocExported, ResourceKind: "doc", ResourceUUID: docID,
		Data: map[string]string{"format": "markdown", "via": "browser"}}, open)
	if summary != "Exported ‘Launch notes’ as markdown, in the browser" {
		t.Errorf("export reads %q", summary)
	}
}
