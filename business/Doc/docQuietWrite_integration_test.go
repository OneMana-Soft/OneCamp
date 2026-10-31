//go:build integration

package businness

// Quiet writes against real stores: Postgres 12, which keeps whom a doc's body
// mentions, and Dgraph, which holds the docs. A body saved quietly (a copy's,
// an import's) keeps whom it mentions and tells nobody, and the first save by a
// person after it tells only whom that save adds. Duplicate's copy tells nobody
// whom its source mentions, whether the collaboration service saves it (the
// save says quiet) or is down (the seed is written directly). A doc made from a
// template still tells whom the template mentions. Neither a seed nor a copy
// writes a body into a folder (docs F1).
//
// Run: PKGS='./business/Doc/' integ_rel.sh <worktree> <log> '^(TestAQuietSaveTellsNobodyAndKeepsWhoItMentions|TestACopyTellsNobodyWhomItsSourceMentions|TestACopyMadeWithTheServiceDownTellsNobody|TestADocFromATemplateTellsWhomItMentions|TestASeedIntoAFolderWritesNothing|TestACopyOfAFolderIsNotSeeded)$'

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	adapter "github.com/akashc777/OneCamp/adapter/Doc"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	domain "github.com/akashc777/OneCamp/domain/Doc"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/services/collab"
	"github.com/akashc777/OneCamp/tests/integration"
)

// realLive is the Live Doc Writer the package starts with: the client of the
// collaboration service's live port.
var realLive = collab.Live

// mentionOf is a person's @mention chip, as the editor writes it.
func mentionOf(id string) string {
	return `<span data-type="mention" data-id="` + id + `@0x9" data-label="someone">@someone</span>`
}

// quietWorld is the stores a doc's saves reach, the events they emit (each
// with whether it came from a bulk import's context), and Sam, who writes.
type quietWorld struct {
	sam  *dgraphStruct.DgraphUser
	bus  *bulkSeen
	seen int
}

func newQuietWorld(t *testing.T) *quietWorld {
	t.Helper()
	integration.ConnectServerPostgres(t)
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[],"result":"updated","_id":"x"}`)
	}))
	t.Cleanup(search.Close)
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	oldSearch, oldBus, oldLive := opensearchInit.OpenSearchClient, resourceEventBusiness.Bus, collab.Live
	opensearchInit.OpenSearchClient = client
	w := &quietWorld{bus: &bulkSeen{}}
	resourceEventBusiness.Bus = w.bus
	t.Cleanup(func() {
		opensearchInit.OpenSearchClient, resourceEventBusiness.Bus, collab.Live = oldSearch, oldBus, oldLive
	})
	sam := uuid.NewString()
	uid := dg.Mutate(t, []map[string]any{{"uid": "_:sam", "dgraph.type": "User", "user_uuid": sam, "user_name": "sam",
		"user_full_name": "Sam", "user_deleted_at": "0001-01-01T00:00:00Z"}})["sam"]
	w.sam = &dgraphStruct.DgraphUser{Uid: uid, Uuid: sam, UserName: "sam", UserFullName: "Sam"}
	return w
}

// heardEvents is what the events since the last look said: who doc.mentioned
// told, and whether each doc.updated and doc.created came from a bulk
// import's context.
type heardEvents struct {
	told                 []string
	updatedBulk, created []bool
}

// heard is what was heard since the last look, once the work it started has
// settled.
func (w *quietWorld) heard(t *testing.T) heardEvents {
	t.Helper()
	if busy := helpers.SettleBackground(30 * time.Second); len(busy) > 0 {
		t.Fatalf("still running after 30 s: %v", busy)
	}
	w.bus.mu.Lock()
	defer w.bus.mu.Unlock()
	var h heardEvents
	for i := w.seen; i < len(w.bus.events); i++ {
		switch e := w.bus.events[i]; e.Kind {
		case resourceEventBusiness.DocMentioned:
			h.told = append(h.told, e.Subjects...)
		case resourceEventBusiness.DocUpdated:
			h.updatedBulk = append(h.updatedBulk, w.bus.bulk[i])
		case resourceEventBusiness.DocCreated:
			h.created = append(h.created, w.bus.bulk[i])
		}
	}
	w.seen = len(w.bus.events)
	return h
}

// save is the collaboration service's save of body: Sam's typing, or a quiet
// write in Sam's name.
func (w *quietWorld) save(t *testing.T, docUUID, body string, quiet bool) {
	t.Helper()
	got, err := SaveFromCollab(context.Background(), CollabSave{DocUUID: docUUID, HTML: body, YjsState: "c3RhdGU=", Contributors: []string{w.sam.Uuid}, Quiet: quiet})
	if err != nil || !got.Changed {
		t.Fatalf("the save of %s: %+v %v", docUUID, got, err)
	}
}

// page makes one of Sam's pages at the top of their private pages, holding
// body when it is given (written as the doc's body, with no save and nobody
// told).
func (w *quietWorld) page(t *testing.T, title, body string) string {
	t.Helper()
	return w.pageUnder(t, title, body, nil).Uuid
}

// pageUnder makes one of Sam's pages under parent (nil: at the top of their
// private pages), holding body when it is given, as page does.
func (w *quietWorld) pageUnder(t *testing.T, title, body string, parent *dgraphStruct.DgraphDoc) *dgraphStruct.DgraphDoc {
	t.Helper()
	ctx := context.Background()
	doc, err := CreateDocInTree(ctx, w.sam, NewDoc{Title: title}, parent)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		if err := UpdateDoc(ctx, &adapter.InputUpdateDoc{DocId: doc.Uuid, Body: &body}); err != nil {
			t.Fatal(err)
		}
	}
	w.heard(t)
	return doc
}

// collabSaves is the collaboration service as the API sees it: a seed is
// written, and its save comes back through SaveFromCollab as the service posts
// it (who wrote it, and quiet when the write was).
type collabSaves struct {
	collab.Writer
	seeds []collab.Options
	// markLost: the seed's save comes back loud whatever the seed was (its
	// quiet mark lost to a restart), holding typed, written meanwhile, too.
	markLost bool
	typed    string
}

func (s *collabSaves) Seed(ctx context.Context, docUUID, html string, o collab.Options) (collab.Result, error) {
	s.seeds = append(s.seeds, o)
	save := CollabSave{DocUUID: docUUID, HTML: html + s.typed, YjsState: "c3RhdGU=", Quiet: o.Quiet && !s.markLost}
	if o.By.AgentUUID != "" {
		save.Agents = []AgentContributor{{AgentUUID: o.By.AgentUUID, AskerUUID: o.By.UserUUID}}
	} else {
		save.Contributors = []string{o.By.UserUUID}
	}
	if _, err := SaveFromCollab(ctx, save); err != nil {
		return collab.Result{}, err
	}
	return collab.Result{Blocks: 1, Persisted: true, PersistedAt: time.Now()}, nil
}

// A doc's body saved quietly tells nobody, twice, and keeps whom it mentions:
// the first save by a person after it tells only whom that save adds.
func TestAQuietSaveTellsNobodyAndKeepsWhoItMentions(t *testing.T) {
	w := newQuietWorld(t)
	maya, jo, ana := uuid.NewString(), uuid.NewString(), uuid.NewString()
	doc := w.page(t, "Copy of Launch", "")

	w.save(t, doc, "<p>"+mentionOf(maya)+" owns the launch.</p>", true)
	if h := w.heard(t); len(h.told) != 0 || !slices.Equal(h.updatedBulk, []bool{true}) {
		t.Fatalf("a quiet save told %v; its doc.updated came from a bulk import's context: %v, want [true]", h.told, h.updatedBulk)
	}
	w.save(t, doc, "<p>"+mentionOf(maya)+" owns the launch, "+mentionOf(jo)+" the date.</p>", true)
	if h := w.heard(t); len(h.told) != 0 {
		t.Fatalf("a second quiet save told %v", h.told)
	}
	w.save(t, doc, "<p>"+mentionOf(maya)+" owns the launch, "+mentionOf(jo)+" the date.</p><p>Ask "+mentionOf(ana)+".</p>", false)
	if h := w.heard(t); !slices.Equal(h.told, []string{ana}) || !slices.Equal(h.updatedBulk, []bool{false}) {
		t.Fatalf("Sam's save adding Ana told %v (doc.updated from a bulk import's context: %v), want only Ana", h.told, h.updatedBulk)
	}
}

// Duplicate's copy of a page that mentions Maya tells nobody: its seed is
// quiet, and the collaboration service's save of it says so. Its doc.created
// still fires as any new doc's does, and Sam's save adding Ana tells only Ana.
func TestACopyTellsNobodyWhomItsSourceMentions(t *testing.T) {
	w := newQuietWorld(t)
	maya, ana := uuid.NewString(), uuid.NewString()
	body := "<p>" + mentionOf(maya) + " owns the launch.</p>"
	source := w.page(t, "Launch", body)
	service := &collabSaves{}
	collab.Live = service

	copied, err := DuplicateDocs(context.Background(), w.sam, []string{source}, map[string]bool{source: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := w.heard(t)
	if len(h.told) != 0 {
		t.Fatalf("a copy told %v whom its source mentions", h.told)
	}
	if len(service.seeds) != 1 || !service.seeds[0].Quiet || service.seeds[0].By.UserUUID != w.sam.Uuid {
		t.Fatalf("the copy's seeds %+v, want one quiet seed in Sam's name", service.seeds)
	}
	if !slices.Equal(h.updatedBulk, []bool{true}) || !slices.Equal(h.created, []bool{false}) {
		t.Fatalf("the copy's doc.updated from a bulk import's context %v (want [true]), its doc.created %v (want [false])", h.updatedBulk, h.created)
	}

	w.save(t, copied.UUID, body+"<p>Ask "+mentionOf(ana)+".</p>", false)
	if h := w.heard(t); !slices.Equal(h.told, []string{ana}) {
		t.Fatalf("Sam's save of the copy adding Ana told %v, want only Ana", h.told)
	}
}

// A copy whose seed's save comes back loud, its quiet mark lost to a restart,
// still tells nobody whom its source mentions: the copy keeps them before its
// seed goes in (KeepCopiedMentions, N1 gap 1). Ana, typed into that save, is told.
func TestACopysSeedSavedLoudTellsOnlyWhomItAdds(t *testing.T) {
	w := newQuietWorld(t)
	maya, ana := uuid.NewString(), uuid.NewString()
	body := "<p>" + mentionOf(maya) + " owns the launch.</p>"
	source := w.page(t, "Launch", body)
	collab.Live = &collabSaves{markLost: true, typed: "<p>Ask " + mentionOf(ana) + ".</p>"}

	if _, err := DuplicateDocs(context.Background(), w.sam, []string{source}, map[string]bool{source: true}, nil); err != nil {
		t.Fatal(err)
	}
	if h := w.heard(t); !slices.Equal(h.told, []string{ana}) {
		t.Fatalf("the copy's loud save told %v, want only Ana", h.told)
	}
}

// With the collaboration service down, a copy's body is written directly and
// no save runs: whom it mentions is kept all the same, so nobody is told then,
// and Sam's first save after, adding Ana, tells only Ana.
func TestACopyMadeWithTheServiceDownTellsNobody(t *testing.T) {
	w := newQuietWorld(t)
	maya, ana := uuid.NewString(), uuid.NewString()
	body := "<p>" + mentionOf(maya) + " owns the launch.</p>"
	source := w.page(t, "Launch", body)
	// The service's own client, with no secret to call it by: unavailable.
	collab.Live = realLive
	t.Setenv("INTERNAL_SECRET", "")

	copied, err := DuplicateDocs(context.Background(), w.sam, []string{source}, map[string]bool{source: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if h := w.heard(t); len(h.told) != 0 {
		t.Fatalf("a copy written directly told %v", h.told)
	}
	if stored, err := readStoredBody(context.Background(), copied.UUID); err != nil || stored != body {
		t.Fatalf("the copy's body %q (%v), want its source's", stored, err)
	}

	w.save(t, copied.UUID, body+"<p>Ask "+mentionOf(ana)+".</p>", false)
	if h := w.heard(t); !slices.Equal(h.told, []string{ana}) {
		t.Fatalf("Sam's first save of the copy, adding Ana, told %v, want only Ana", h.told)
	}
}

// oneTemplate is a template source holding one body.
type oneTemplate string

func (b oneTemplate) Body(context.Context, string) (string, error) { return string(b), nil }

// A doc made from a template that mentions Maya tells Maya: making a doc from
// a template is a person's act, and the mentions in it are theirs to send.
func TestADocFromATemplateTellsWhomItMentions(t *testing.T) {
	w := newQuietWorld(t)
	maya := uuid.NewString()
	oldTemplates := Templates
	Templates = oneTemplate("<p>Kickoff with " + mentionOf(maya) + ".</p>")
	t.Cleanup(func() { Templates = oldTemplates })
	service := &collabSaves{}
	collab.Live = service

	if _, err := CreateDocInTree(context.Background(), w.sam, NewDoc{Title: "Kickoff", TemplateID: "kickoff"}, nil); err != nil {
		t.Fatal(err)
	}
	h := w.heard(t)
	if len(service.seeds) != 1 || service.seeds[0].Quiet {
		t.Fatalf("the template's seeds %+v, want one loud seed", service.seeds)
	}
	if !slices.Equal(h.told, []string{maya}) || !slices.Equal(h.updatedBulk, []bool{false}) {
		t.Fatalf("a doc from a template told %v (doc.updated from a bulk import's context: %v), want Maya", h.told, h.updatedBulk)
	}
}

// A seed into a folder writes nothing. The collaboration service opens pages
// only, and its refusal reads as the service being down (here it is down), so
// the seed falls back to writing a new doc's first body directly: that reads
// the folder's kind and answers ErrNotAPage. The folder keeps no body, and
// nobody is told.
func TestASeedIntoAFolderWritesNothing(t *testing.T) {
	w := newQuietWorld(t)
	ctx := context.Background()
	folder, err := CreateDocInTree(ctx, w.sam, NewDoc{Title: "Plans", NodeKind: NodeFolder}, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.heard(t)
	// The service's own client, with no secret to call it by: unavailable.
	collab.Live = realLive
	t.Setenv("INTERNAL_SECRET", "")

	maya := uuid.NewString()
	if _, err := seedBodyWith(ctx, folder.Uuid, "<p>"+mentionOf(maya)+" owns the plan.</p>", collab.Options{By: collab.By{UserUUID: w.sam.Uuid}, Quiet: true}); !errors.Is(err, ErrNotAPage) {
		t.Fatalf("a seed into a folder answered %v, want ErrNotAPage", err)
	}
	stored, err := domain.GetSystemDocByUUID(ctx, folder.Uuid)
	if err != nil || stored == nil || stored.Body != "" || stored.NodeKind != NodeFolder {
		t.Fatalf("the folder after a seed: %+v, %v; want a folder with no body", stored, err)
	}
	if h := w.heard(t); len(h.told) != 0 || len(h.updatedBulk) != 0 {
		t.Fatalf("a seed into a folder told %v and updated %v", h.told, h.updatedBulk)
	}
}

// A copy of a folder has no body seeded into it, even when the folder holds
// one, which none should (docs F1): only a page's copy is seeded.
func TestACopyOfAFolderIsNotSeeded(t *testing.T) {
	w := newQuietWorld(t)
	ctx := context.Background()
	folder, err := CreateDocInTree(ctx, w.sam, NewDoc{Title: "Plans", NodeKind: NodeFolder}, nil)
	if err != nil {
		t.Fatal(err)
	}
	body := "<p>A body no folder should hold.</p>"
	if err := UpdateDoc(ctx, &adapter.InputUpdateDoc{DocId: folder.Uuid, Body: &body}); err != nil {
		t.Fatal(err)
	}
	w.heard(t)
	service := &collabSaves{}
	collab.Live = service

	copied, err := DuplicateDocs(ctx, w.sam, []string{folder.Uuid}, map[string]bool{folder.Uuid: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(service.seeds) != 0 {
		t.Fatalf("a copy of a folder was seeded: %+v", service.seeds)
	}
	stored, err := domain.GetSystemDocByUUID(ctx, copied.UUID)
	if err != nil || stored == nil || stored.Body != "" || stored.NodeKind != NodeFolder {
		t.Fatalf("the folder's copy: %+v, %v; want a folder with no body", stored, err)
	}
}
