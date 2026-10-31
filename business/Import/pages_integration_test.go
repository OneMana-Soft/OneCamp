//go:build integration

package business

// The pages stage (pages_worker.go) against real stores: a provider's tree of
// pages made into docs in the graph, recorded in the import's id map, not made
// again by a retry or by another import of the same workspace, moved to the
// trash by a rollback, and never left half made. The Live Doc Writer is the
// one fake.
// Run: go test -tags=integration ./business/Import/ -run 'TestAProvidersPages|TestATreeHalfMade' -v

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	contentIOBusiness "github.com/akashc777/OneCamp/business/ContentIO"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/services/collab"
	"github.com/akashc777/OneCamp/tests/integration"
)

// pagesFrom is a provider that brings a fixed tree of pages (CapPages).
type pagesFrom struct {
	importProvider.Provider
	pages []importProvider.SourcePage
}

func (p pagesFrom) Name() string { return "notion" }

func (p pagesFrom) PagesOf(context.Context, *importModels.Job, importProvider.JobOptions, string, func(int)) ([]importProvider.SourcePage, error) {
	return p.pages, nil
}

// pageSeeds is the Live Doc Writer, recording what each doc is seeded with;
// past downAfter seeds (when it is set) it is down. With markLost, each seed's
// save comes back as the collaboration service posts it, loud whatever the
// seed was (its quiet mark lost to a restart).
type pageSeeds struct {
	collab.Writer
	mu        sync.Mutex
	seeded    map[string]string
	asked     int
	downAfter int
	markLost  bool
}

func (s *pageSeeds) Seed(ctx context.Context, id, html string, o collab.Options) (collab.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked++
	if s.downAfter > 0 && s.asked > s.downAfter {
		return collab.Result{}, collab.ErrUnavailable
	}
	s.seeded[id] = html
	if s.markLost {
		save := docBusiness.CollabSave{DocUUID: id, HTML: html, YjsState: "c3RhdGU=", Contributors: []string{o.By.UserUUID}}
		if _, err := docBusiness.SaveFromCollab(ctx, save); err != nil {
			return collab.Result{}, err
		}
	}
	return collab.Result{PersistedAt: time.Now()}, nil
}

// pagesWorld is the stores the pages stage writes to, with the Live Doc
// Writer faked, and Ada, who imports Acme's Notion.
type pagesWorld struct {
	env       *integration.Env
	dg        *integration.DgraphEnv
	seeds     *pageSeeds
	admin     uuid.UUID
	uid       string
	importing *userModels.UserInfo
}

const treeRoot, treeChild = "11111111111111111111111111111111", "22222222222222222222222222222222"

// tree is Engineering with Runbooks under it, linked from it.
var tree = pagesFrom{pages: []importProvider.SourcePage{
	{SourceID: treeRoot, Title: "Engineering", Icon: "🛠️", HTML: `<h2>Overview</h2><p>See <a href="` + treeChild + `">Runbooks</a>.</p>`},
	{SourceID: treeChild, ParentSourceID: treeRoot, Title: "Runbooks", Icon: "📕", HTML: `<p>Restart the queue.</p>`},
}}

func newPagesWorld(t *testing.T) *pagesWorld {
	t.Helper()
	env, dg := importEnv(t)
	w := &pagesWorld{env: env, dg: dg, seeds: &pageSeeds{seeded: map[string]string{}}, admin: uuid.New()}
	savedLive := collab.Live
	collab.Live = w.seeds
	t.Cleanup(func() { collab.Live = savedLive })
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, 'ada', 'Ada', NOW(), NOW())`,
		w.admin, w.admin.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	w.uid = dg.Mutate(t, []map[string]any{{"uid": "_:ada", "dgraph.type": "User", "user_uuid": w.admin.String(), "user_name": "ada",
		"user_full_name": "Ada", "user_deleted_at": "0001-01-01T00:00:00Z"}})["ada"]
	w.importing = &userModels.UserInfo{}
	w.importing.UserPostgresInfo.Id = w.admin
	w.importing.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: w.uid, Uuid: w.admin.String()}
	return w
}

// job is an import of Acme's Notion by Ada, with the chunk of the tree.
func (w *pagesWorld) job(t *testing.T) (*importModels.Job, *importModels.Chunk) {
	t.Helper()
	ctx := context.Background()
	job := &importModels.Job{Id: uuid.New(), Provider: "notion", SourceWorkspaceName: "Acme", Source: "api", Status: "running", TriggeredBy: &w.admin}
	if err := importModels.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	r := treeRoot
	chunk := &importModels.Chunk{Id: uuid.New(), ImportId: job.Id, ChunkType: importModels.ChunkDocPages, ParentSourceId: &r, Status: importModels.ChunkStatusPending, MaxAttempts: 5}
	if err := importModels.CreateChunks(ctx, []*importModels.Chunk{chunk}); err != nil {
		t.Fatal(err)
	}
	return job, chunk
}

// structure.md P2.4: a provider's tree of pages becomes docs with their tree,
// titles and icons, a link between two of them pointing at the doc the other
// became, its chunk's claim renewed while the docs are made; a tree made once
// is not made again, by a retry or by another import of the same workspace;
// and rolling the import back moves its docs to the trash.
func TestAProvidersPagesBecomeDocsAndRollBackToTheTrash(t *testing.T) {
	ctx := context.Background()
	w := newPagesWorld(t)
	job, chunk := w.job(t)
	// Claimed an hour ago, as by a worker; making the docs outlasts a beat.
	if _, err := w.env.PG.Exec(`UPDATE import_chunks SET status = 'in_progress', claimed_at = NOW() - interval '1 hour' WHERE id = $1`, chunk.Id); err != nil {
		t.Fatal(err)
	}
	savedEvery, savedMake := heartbeatEvery, makePages
	t.Cleanup(func() { heartbeatEvery, makePages = savedEvery, savedMake })
	heartbeatEvery = 20 * time.Millisecond
	makePages = func(ctx context.Context, who accessBusiness.Subject, parent string, pages []contentIOBusiness.Page) (contentIOBusiness.Made, error) {
		time.Sleep(200 * time.Millisecond)
		return savedMake(ctx, who, parent, pages)
	}
	if err := processPageChunk(ctx, tree, job, importProvider.JobOptions{}, chunk, w.importing); err != nil {
		t.Fatal(err)
	}
	var renewed bool
	if err := w.env.PG.QueryRow(`SELECT claimed_at > NOW() - interval '1 minute' FROM import_chunks WHERE id = $1`, chunk.Id).Scan(&renewed); err != nil || !renewed {
		t.Fatalf("the chunk's claim was not renewed while its docs were made (%v)", err)
	}
	rootDoc, _ := importModels.LookupIdMapping(ctx, job.Id, importModels.EntityDoc, treeRoot)
	childDoc, _ := importModels.LookupIdMapping(ctx, job.Id, importModels.EntityDoc, treeChild)
	if rootDoc == uuid.Nil || childDoc == uuid.Nil {
		t.Fatalf("recorded %s and %s", rootDoc, childDoc)
	}
	rows, err := docBusiness.DocRows(ctx, []string{rootDoc.String(), childDoc.String()}, w.uid)
	if err != nil || len(rows) != 2 {
		t.Fatalf("docs %+v %v", rows, err)
	}
	byID := map[string]*dgraphStruct.DgraphDoc{}
	for _, d := range rows {
		byID[d.Uuid] = d
	}
	r, c := byID[rootDoc.String()], byID[childDoc.String()]
	if r.Title != "Engineering" || r.Icon != "🛠️" || c.Title != "Runbooks" || c.Icon != "📕" || c.Parent == nil || c.Parent.Uuid != rootDoc.String() {
		t.Fatalf("the tree came across as %+v / %+v", r, c)
	}
	if !strings.Contains(w.seeds.seeded[rootDoc.String()], "/app/doc/"+childDoc.String()) {
		t.Fatalf("the link to Runbooks: %s", w.seeds.seeded[rootDoc.String()])
	}

	// A retry of the chunk, and another import of the same workspace, make
	// nothing new.
	before := len(w.seeds.seeded)
	if err := processPageChunk(ctx, tree, job, importProvider.JobOptions{}, chunk, w.importing); err != nil {
		t.Fatal(err)
	}
	if _, err := w.env.PG.Exec(`UPDATE import_jobs SET status = 'completed' WHERE id = $1`, job.Id); err != nil {
		t.Fatal(err)
	}
	again, againChunk := w.job(t)
	if err := processPageChunk(ctx, tree, again, importProvider.JobOptions{}, againChunk, w.importing); err != nil {
		t.Fatal(err)
	}
	if len(w.seeds.seeded) != before {
		t.Fatalf("pages made again: %d seeds, then %d", before, len(w.seeds.seeded))
	}
	if mapped, _ := importModels.LookupIdMapping(ctx, again.Id, importModels.EntityDoc, treeRoot); mapped != rootDoc {
		t.Fatalf("the second import's root is %s, not the doc the first made", mapped)
	}

	// Rolling the first import back moves its docs to the trash, as one
	// delete; the second import made nothing, so its rollback takes nothing.
	if _, err := w.env.PG.Exec(`UPDATE import_jobs SET status = 'completed' WHERE id = $1`, again.Id); err != nil {
		t.Fatal(err)
	}
	if err := RollbackImport(ctx, again.Id); err != nil {
		t.Fatal(err)
	}
	if rows, _ := docBusiness.DocRows(ctx, []string{rootDoc.String()}, w.uid); len(rows) != 1 || helpers.IsSoftDeleted(rows[0].DeletedAt) {
		t.Fatal("rolling back an import that made nothing took the earlier import's docs")
	}
	if err := RollbackImport(ctx, job.Id); err != nil {
		t.Fatal(err)
	}
	rows, err = docBusiness.DocRows(ctx, []string{rootDoc.String(), childDoc.String()}, w.uid)
	if err != nil || len(rows) != 2 {
		t.Fatalf("docs after the rollback %+v %v", rows, err)
	}
	for _, d := range rows {
		if !helpers.IsSoftDeleted(d.DeletedAt) {
			t.Errorf("%s is not in the trash", d.Title)
		}
	}
	if rows[0].DeletedBatch == "" || rows[0].DeletedBatch != rows[1].DeletedBatch {
		t.Errorf("the tree went to the trash in pieces: %q and %q", rows[0].DeletedBatch, rows[1].DeletedBatch)
	}
}

// A tree the document service stops taking part way is taken away again, and
// its chunk tried again later, where it comes across whole: never half a tree
// recorded as done.
func TestATreeHalfMadeIsTakenAwayAndMadeAgainWhole(t *testing.T) {
	ctx := context.Background()
	w := newPagesWorld(t)
	w.seeds.downAfter = 1
	job, chunk := w.job(t)
	if err := processPageChunk(ctx, tree, job, importProvider.JobOptions{}, chunk, w.importing); !errors.Is(err, errTryTreeAgain) {
		t.Fatalf("a tree half made: %v", err)
	}
	if mapped, _ := importModels.LookupIdMapping(ctx, job.Id, importModels.EntityDoc, treeRoot); mapped != uuid.Nil {
		t.Fatalf("half a tree was recorded: %s", mapped)
	}
	for id := range w.seeds.seeded {
		if rows, _ := docBusiness.DocRows(ctx, []string{id}, w.uid); len(rows) != 0 {
			t.Fatalf("the doc made before the service stopped is still there: %+v", rows[0])
		}
	}

	w.seeds.downAfter = 0
	if err := processPageChunk(ctx, tree, job, importProvider.JobOptions{}, chunk, w.importing); err != nil {
		t.Fatal(err)
	}
	rootDoc, _ := importModels.LookupIdMapping(ctx, job.Id, importModels.EntityDoc, treeRoot)
	childDoc, _ := importModels.LookupIdMapping(ctx, job.Id, importModels.EntityDoc, treeChild)
	if rows, err := docBusiness.DocRows(ctx, []string{rootDoc.String(), childDoc.String()}, w.uid); err != nil || len(rows) != 2 {
		t.Fatalf("tried again, the tree is %+v (%v)", rows, err)
	}
}

// N1 gap 1: a page that mentions Maya tells her nothing as it comes across,
// even when its seed's save comes back loud, its quiet mark lost to a
// restart: the import keeps a page's mentions before it seeds it.
func TestAnImportedPagesMentionsTellNobodyFromALoudSave(t *testing.T) {
	ctx := context.Background()
	w := newPagesWorld(t)
	w.seeds.markLost = true
	job, chunk := w.job(t)
	maya := uuid.NewString()
	mentioning := pagesFrom{pages: []importProvider.SourcePage{
		{SourceID: treeRoot, Title: "Engineering", HTML: `<p><span data-type="mention" data-id="` + maya + `" data-label="Maya">@Maya</span> owns the launch.</p>`},
	}}
	tap := &busTap{next: resourceEventBusiness.Bus}
	resourceEventBusiness.Bus = tap
	t.Cleanup(func() { resourceEventBusiness.Bus = tap.next })

	if err := processPageChunk(ctx, mentioning, job, importProvider.JobOptions{}, chunk, w.importing); err != nil {
		t.Fatal(err)
	}
	if len(w.seeds.seeded) != 1 {
		t.Fatalf("seeded %d docs, want the page's", len(w.seeds.seeded))
	}
	heard := tap.heard(t)
	for _, kind := range heard {
		if kind == string(resourceEventBusiness.DocMentioned) {
			t.Fatalf("an imported page's mention of Maya told her: %v", heard)
		}
	}
}

// busTap records what the bus is told, and passes it on.
type busTap struct {
	next  resourceEventBusiness.Emitter
	mu    sync.Mutex
	kinds []string
}

func (b *busTap) Emit(ctx context.Context, e resourceEventBusiness.Event) {
	b.mu.Lock()
	b.kinds = append(b.kinds, string(e.Kind))
	b.mu.Unlock()
	b.next.Emit(ctx, e)
}

// heard is every kind emitted once the background work the import started
// has finished: who a save tells is worked out after it answers, so a read
// before that misses an event on its way, and a test that nobody was told
// passes whatever happened.
func (b *busTap) heard(t *testing.T) []string {
	t.Helper()
	if busy := helpers.SettleBackground(30 * time.Second); len(busy) > 0 {
		t.Fatalf("still running after 30 s: %v", busy)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.kinds...)
}

// Docs N1: a page's comments come across as its doc's comments, by their
// author when the import knows them as a person here (Maya), by the person
// importing when not, at the time they were made, once each, telling nobody;
// and a rollback takes them with the import.
func TestAPagesCommentsComeAcrossQuietly(t *testing.T) {
	ctx := context.Background()
	w := newPagesWorld(t)
	job, chunk := w.job(t)

	maya := uuid.New()
	if _, err := w.env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, 'maya', 'Maya', NOW(), NOW())`,
		maya, maya.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	w.dg.Mutate(t, []map[string]any{{"uid": "_:maya", "dgraph.type": "User", "user_uuid": maya.String(), "user_name": "maya",
		"user_full_name": "Maya", "user_deleted_at": "0001-01-01T00:00:00Z"}})
	if err := importModels.UpsertIdMappingWithOwnership(ctx, job.Id, importModels.EntityUser, "u-maya", maya, nil, nil, false); err != nil {
		t.Fatal(err)
	}

	asked := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	answered := asked.Add(90 * time.Minute)
	withComments := pagesFrom{pages: []importProvider.SourcePage{
		{SourceID: treeRoot, Title: "Engineering", HTML: `<p>Ship on Friday.</p>`, Comments: []importProvider.SourceComment{
			{SourceID: "cm-1", Body: "<p>Can we ship on <strong>Friday</strong>?</p>", AuthorSourceID: "u-maya", Created: asked},
			{SourceID: "cm-2", Body: "<p>Yes, after the review.</p>", AuthorSourceID: "u-someone-not-here", Created: answered},
		}},
	}}
	tap := &busTap{next: resourceEventBusiness.Bus}
	resourceEventBusiness.Bus = tap
	t.Cleanup(func() { resourceEventBusiness.Bus = tap.next })

	if err := processPageChunk(ctx, withComments, job, importProvider.JobOptions{}, chunk, w.importing); err != nil {
		t.Fatal(err)
	}
	rootDoc, _ := importModels.LookupIdMapping(ctx, job.Id, importModels.EntityDoc, treeRoot)
	first, _ := importModels.LookupIdMapping(ctx, job.Id, importModels.EntityComment, "cm-1")
	second, _ := importModels.LookupIdMapping(ctx, job.Id, importModels.EntityComment, "cm-2")
	if rootDoc == uuid.Nil || first == uuid.Nil || second == uuid.Nil {
		t.Fatalf("recorded doc %s, comments %s and %s", rootDoc, first, second)
	}
	for id, want := range map[uuid.UUID]struct {
		by uuid.UUID
		at time.Time
	}{first: {maya, asked}, second: {w.admin, answered}} {
		var by uuid.UUID
		var at time.Time
		if err := w.env.PG.QueryRow(`SELECT created_by, created_at FROM comments WHERE id = $1`, id).Scan(&by, &at); err != nil {
			t.Fatal(err)
		}
		if by != want.by || !at.Equal(want.at) {
			t.Errorf("comment %s by %s at %s, want %s at %s", id, by, at, want.by, want.at)
		}
	}
	doc, err := docBusiness.GetDocCommentList(ctx, rootDoc.String(), w.uid)
	if err != nil || doc == nil || len(doc.Comments) != 2 {
		t.Fatalf("the doc's comments: %+v %v", doc, err)
	}
	texts := map[string]string{}
	for _, c := range doc.Comments {
		texts[c.Uuid] = c.Text
	}
	if texts[first.String()] != "<p>Can we ship on <strong>Friday</strong>?</p>" || texts[second.String()] != "<p>Yes, after the review.</p>" {
		t.Fatalf("the comments read %v", texts)
	}
	heard := tap.heard(t)
	for _, kind := range heard {
		if kind == string(resourceEventBusiness.DocCommentCreated) {
			t.Fatalf("an imported comment told someone: %v", heard)
		}
	}

	// Written once: the page's comments again add nothing.
	pageComments(ctx, job, chunk, w.importing, rootDoc.String(), withComments.pages[0])
	if doc, _ := docBusiness.GetDocCommentList(ctx, rootDoc.String(), w.uid); doc == nil || len(doc.Comments) != 2 {
		t.Fatal("a page's comments came across twice")
	}

	// A rollback takes them with the import.
	if _, err := w.env.PG.Exec(`UPDATE import_jobs SET status = 'completed' WHERE id = $1`, job.Id); err != nil {
		t.Fatal(err)
	}
	if err := RollbackImport(ctx, job.Id); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := w.env.PG.QueryRow(`SELECT COUNT(*) FROM comments WHERE id IN ($1, $2) AND deleted_at IS NULL`, first, second).Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d comments outlived the rollback (%v)", left, err)
	}
}
