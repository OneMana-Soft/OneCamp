package businness

// The trash, for members (structure.md P0.3).
//
// A page goes to the trash with everything under it the person can open, as
// one delete: the same deletion time, batch and deleter on all of them, so
// restoring any of it restores all of it. A page under it they can't open
// stays, moving up to its parent with everything under it (docTake.go: protect
// what they can't see), and a delete for good removes only the pages the
// person has full access to, the rest staying in the trash, moved up the same
// way. A page whose parent is gone when it comes back lands at the top. Docs
// deleted before batches existed come back one at a time.
// Thirty days after (DOC_TRASH_RETENTION_DAYS), the trash's own sweep removes
// a delete for good; before that, its owner, someone with full access, or an
// admin may. The retention archive keeps its own list (Archived), which the
// trash leaves alone, and so does its sweep.
//
// Who may is the caller's to decide, before anything else is said: a delete
// through accessBusiness.Open, a restore or a delete for good through
// accessBusiness.OpenDeleted, which decides by the level the person had
// before the delete (RestoreNeeds, PurgeNeeds). So a doc never there for
// someone answers them as a missing one does, live or deleted. Under the
// tree's lock, a restore checks the top of everything it brings back, and a
// delete for good the doc it removes, again.

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	demoGuard "github.com/akashc777/OneCamp/business/DemoGuard"
	placesBusiness "github.com/akashc777/OneCamp/business/Places"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	"github.com/akashc777/OneCamp/business/Tree/graph"
	"github.com/akashc777/OneCamp/business/Tree/position"
	domain "github.com/akashc777/OneCamp/domain/Doc"
	globalSearchDomain "github.com/akashc777/OneCamp/domain/GlobalSearch"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	opensearchModels "github.com/akashc777/OneCamp/models/openSearch/GlobalSearch"
	"github.com/google/uuid"
)

// Limits on one trash operation.
const (
	// trashSubtreeCap bounds the pages one delete takes; a deeper or wider
	// subtree is refused rather than taken in part.
	trashSubtreeCap = 5000
	// trashListCap bounds what the trash lists of one person's.
	trashListCap = 2000
	// trashSweepBatch is how many docs one pass of the retention sweep removes.
	trashSweepBatch = 200
)

// Errors the trash answers with.
var (
	// ErrNotInTrash is a restore or a purge of a doc that isn't in the trash.
	ErrNotInTrash = errors.New("that doc isn't in the trash")
	// ErrTrashForbidden is a restore or purge the person may not make.
	ErrTrashForbidden = errors.New("you can't do that to this doc")
	// ErrDemoKept is a delete of a page the demo keeps for every visitor.
	ErrDemoKept = errors.New("the demo keeps this page")
	// ErrTooMany is a subtree too large to delete or copy at once: the tree's
	// own (graph.ErrTooMany), so every refusal of a subtree answers alike.
	ErrTooMany = graph.ErrTooMany
)

// TrashRetention is how long a delete stays in the trash before the sweep
// removes it for good: DOC_TRASH_RETENTION_DAYS, 30 by default.
func trashRetention() time.Duration {
	days := 30
	if v, err := strconv.Atoi(os.Getenv("DOC_TRASH_RETENTION_DAYS")); err == nil && v > 0 {
		days = v
	}
	return time.Duration(days) * 24 * time.Hour
}

// Person is someone a list row names: who owns a doc, who deleted it.
type Person struct {
	UUID       string  `json:"user_uuid"`
	Name       string  `json:"user_name,omitempty"`
	FullName   string  `json:"user_full_name,omitempty"`
	ProfileKey *string `json:"user_profile_object_key,omitempty"`
}

// PersonOf is the person a graph user is, for a list row.
func PersonOf(u *dgraphStruct.DgraphUser) *Person {
	if u == nil || u.Uuid == "" {
		return nil
	}
	return &Person{UUID: u.Uuid, Name: u.UserName, FullName: u.UserFullName, ProfileKey: u.ProfileKey}
}

// couldEditDeleted is whether the person (graph uid) could change a doc before
// it was deleted: what the trash shows them and lets them restore
// (CanRestore, without an admin's say).
func couldEditDeleted(doc *dgraphStruct.DgraphDoc, viewerUID string) bool {
	return CanRestore(doc, Viewer{UID: viewerUID}, false)
}

// canPurge is whether the person may delete a doc for good (CanPurge).
func canPurge(doc *dgraphStruct.DgraphDoc, viewerUID string, isAdmin bool) bool {
	return CanPurge(doc, Viewer{UID: viewerUID}, isAdmin)
}

// TrashResult is what one delete put in the trash, and how many pages under
// it stayed because the person can't open them (Kept): each moved up to its
// parent with whatever is under it, which isn't counted, as the person can't
// see into it.
type TrashResult struct {
	Batch string   `json:"batch_id"`
	Count int      `json:"count"`
	Kept  int      `json:"kept"`
	UUIDs []string `json:"-"`
}

// siblingsCap bounds the level read to place the pages that move up.
const siblingsCap = 5000

// TrashSubtree puts a live doc and every live page under it the person can
// open in the trash as one delete, by the person (graph uid and uuid, email
// for the demo's guard). A page under it they can't open stays, with
// everything under it, and moves up to the doc's parent in the same write
// (takeForDelete), each move recorded. The caller has decided they may delete
// the doc. ErrDemoKept when the demo keeps any page that would go;
// ErrNotFound / ErrDocDeleted when the doc isn't there or is in the trash.
func TrashSubtree(ctx context.Context, docUUID, byUID, byUUID, byEmail string) (*TrashResult, error) {
	unlock, err := graph.Lock(ctx, DocTree.Kind)
	if err != nil {
		return nil, err
	}
	defer unlock()

	under, _, err := graph.Subtree(ctx, DocTree, docUUID, trashSubtreeCap+1)
	if errors.Is(err, graph.ErrNotFound) {
		return nil, liveOr(ctx, docUUID)
	}
	if err != nil {
		return nil, err
	}
	if len(under) > trashSubtreeCap {
		return nil, ErrTooMany
	}
	ids := []string{docUUID}
	for _, r := range under {
		ids = append(ids, r.UUID)
	}
	rows, err := domain.GetDocRowsByUUID(ctx, ids, byUID)
	if err != nil {
		return nil, err
	}
	root := rowOf(rows, docUUID)
	if root == nil {
		return nil, liveOr(ctx, docUUID)
	}
	take, err := takeFor(ctx, Viewer{UID: byUID, UUID: byUUID}, pageOf(root), pagesUnder(under, rows), takesOnDelete)
	if err != nil {
		return nil, err
	}
	// The demo's own pages stay for every visitor (business/DemoGuard).
	goes := map[string]bool{}
	for _, p := range take.Takes {
		goes[strings.ToLower(p.UUID)] = true
	}
	for _, d := range rows {
		if goes[strings.ToLower(d.Uuid)] && demoGuard.KeepsFromVisitor(byEmail, d.CreatedAt) {
			return nil, ErrDemoKept
		}
	}
	up, err := movesUp(ctx, root, take.Stays)
	if err != nil {
		return nil, err
	}

	batch := uuid.NewString()
	now := time.Now()
	uids := uidsOf(take.Takes[1:])
	if err := domain.TrashDocs(ctx, docUUID, uids, batch, byUID, now, up...); err != nil {
		return nil, err
	}
	uuids := uuidsOf(take.Takes)
	searchFollowsTrash(uuids, now)
	for _, id := range uuids {
		resourceEventBusiness.Bus.Emit(ctx, resourceEventBusiness.Event{
			Kind: resourceEventBusiness.DocDeleted, ResourceKind: "doc", ResourceUUID: id,
			Actor: resourceEventBusiness.Actor{UserUUID: byUUID}, At: now,
			Data: map[string]string{"batch": batch},
		})
	}
	emitMovedUp(ctx, take.Stays, parentUUIDOf(root), byUUID, now)
	return &TrashResult{Batch: batch, Count: len(uuids), Kept: len(take.Stays), UUIDs: uuids}, nil
}

// pagesUnder is the pages of a subtree's structure (under, from
// graph.Subtree) as the walk knows them, each with the key and making time
// its row (rows) gives it, which order it among its siblings.
func pagesUnder(under []graph.Ref, rows []*dgraphStruct.DgraphDoc) []treePage {
	byID := make(map[string]*dgraphStruct.DgraphDoc, len(rows))
	for _, d := range rows {
		if d != nil {
			byID[strings.ToLower(d.Uuid)] = d
		}
	}
	pages := make([]treePage, 0, len(under))
	for _, r := range under {
		p := treePage{UID: r.UID, UUID: r.UUID, ParentUUID: r.ParentUUID}
		if d := byID[strings.ToLower(r.UUID)]; d != nil {
			p.Position = d.Position
			if d.CreatedAt != nil {
				p.CreatedAt = *d.CreatedAt
			}
		}
		pages = append(pages, p)
	}
	return pages
}

// rowOf is the row for a doc, or nil.
func rowOf(rows []*dgraphStruct.DgraphDoc, docUUID string) *dgraphStruct.DgraphDoc {
	for _, d := range rows {
		if d != nil && strings.EqualFold(d.Uuid, docUUID) {
			return d
		}
	}
	return nil
}

func parentUUIDOf(d *dgraphStruct.DgraphDoc) string {
	if d != nil && d.Parent != nil {
		return d.Parent.Uuid
	}
	return ""
}

// movesUp is where the pages that stay go: under root's parent (at the top
// when it has none), placed right after root among its siblings, in the order
// they sat, so the level reads as before with the deleted page's place taken
// by what stayed under it. At the top, or when the level's keys can't order
// them, each keeps its own key.
func movesUp(ctx context.Context, root *dgraphStruct.DgraphDoc, stays []treePage) ([]domain.MoveUp, error) {
	if len(stays) == 0 {
		return nil, nil
	}
	parentUID := ""
	if root.Parent != nil {
		parentUID = root.Parent.Uid
	}
	keys := make([]string, len(stays))
	if root.Parent != nil && root.Parent.Uuid != "" {
		sibs, err := graph.Siblings(ctx, DocTree, root.Parent.Uuid, siblingsCap)
		if err != nil {
			return nil, err
		}
		if spread, ok := keysAfter(sibs, root.Uuid, len(stays)); ok {
			keys = spread
		}
	}
	up := make([]domain.MoveUp, len(stays))
	for i, p := range stays {
		up[i] = domain.MoveUp{UID: p.UID, Parent: parentUID, Key: keys[i]}
	}
	return up, nil
}

// keysAfter is n keys right after id among its siblings (in shown order) and
// before the one after it, when their keys can order them. Pure.
func keysAfter(sibs []position.Sibling, id string, n int) ([]string, bool) {
	for i, s := range sibs {
		if s.ID != id {
			continue
		}
		next := ""
		if i+1 < len(sibs) {
			next = sibs[i+1].Position
		}
		if !position.ValidPosition(s.Position) || (next != "" && (!position.ValidPosition(next) || next <= s.Position)) {
			return nil, false
		}
		keys, err := position.Spread(s.Position, next, n)
		return keys, err == nil
	}
	return nil, false
}

// emitMovedUp records each page that moved up when the page above it went
// (I14), as any move is: to its new parent ("" for the top), from the one it
// was under, by the person whose delete moved it, so its open tabs read their
// trees again.
func emitMovedUp(ctx context.Context, stays []treePage, toParent, byUUID string, at time.Time) {
	for _, p := range stays {
		e := movedEvent(p.UUID, toParent, byUUID, nil, at)
		e.Data["from_parent"] = p.ParentUUID
		resourceEventBusiness.Bus.Emit(ctx, e)
	}
}

// liveOr says why a doc can't be put in the trash: it isn't there, or it is
// in the trash already.
func liveOr(ctx context.Context, docUUID string) error {
	d, err := domain.GetDocDeletion(ctx, docUUID)
	switch {
	case errors.Is(err, domain.ErrNotFound) || (err == nil && (d == nil || d.Uuid == "")):
		return ErrNotFound
	case err != nil:
		return err
	case IsDeleted(d):
		return ErrDocDeleted
	}
	return ErrNotFound
}

// docCascade is how a doc's comments, files and AI entries name it in search
// (business/Archive's docCascadeFields, which this follows).
var (
	docCascadeFields  = []string{"comment_doc_id", "attachment_doc_id", "content_uuid", "doc_uuid"}
	docCascadeIndexes = []string{"comments", "attachments"}
	docAIIndexes      = []string{"ai_embeddings"}
)

func cascadeFields(uuids []string) map[string][]string {
	m := map[string][]string{}
	for _, f := range docCascadeFields {
		m[f] = uuids
	}
	return m
}

// searchFollowsTrash has search follow docs into the trash: each doc's own
// entry written from the doc (deleted now), its comments' and files' entries
// marked deleted. The AI index is asked apart, as DeleteDoc does: an edition
// without it fails that call alone.
func searchFollowsTrash(uuids []string, at time.Time) {
	for _, id := range uuids {
		domain.SyncDocSearch(id, false)
	}
	fields := cascadeFields(uuids)
	helpers.GoSafeNamed("doc.trash-search", func() {
		globalSearchDomain.SyncCascadingDeletionInOpenSearchCombined(fields, at.Unix(), docCascadeIndexes, "cascade")
		globalSearchDomain.SyncCascadingDeletionInOpenSearchCombined(fields, at.Unix(), docAIIndexes, "cascade")
	})
}

// searchFollowsRestore is the reverse: entries written from the docs (live
// again), and what the delete marked unmarked.
func searchFollowsRestore(uuids []string) {
	for _, id := range uuids {
		domain.SyncDocSearch(id, true)
	}
	fields := cascadeFields(uuids)
	helpers.GoSafeNamed("doc.restore-search", func() {
		globalSearchDomain.SyncCascadingUnarchiveInOpenSearchCombined(fields, docCascadeIndexes, "cascade")
		globalSearchDomain.SyncCascadingUnarchiveInOpenSearchCombined(fields, docAIIndexes, "cascade")
	})
}

// TrashItem is one entry of a person's trash: a page deleted on its own, or
// the top of a subtree deleted with it.
type TrashItem struct {
	UUID      string     `json:"doc_uuid"`
	Title     string     `json:"doc_title"`
	Icon      string     `json:"doc_icon,omitempty"`
	DeletedAt time.Time  `json:"deleted_at"`
	DeletedBy *Person    `json:"deleted_by,omitempty"`
	Batch     string     `json:"batch_id,omitempty"`
	Subpages  int        `json:"subpages"`
	CanPurge  bool       `json:"can_purge"`
	PurgeAt   *time.Time `json:"purge_at,omitempty"`
}

// Trash is what a person sees in their trash: the deletes of the last
// retention period of docs they could change, newest first, a subtree as its
// top page with how many went with it.
func Trash(ctx context.Context, viewerUID string, isAdmin bool, now time.Time) ([]TrashItem, error) {
	keep := trashRetention()
	docs, err := domain.GetDocTrash(ctx, viewerUID, now.Add(-keep), trashListCap)
	if err != nil {
		return nil, err
	}
	var batches []string
	seen := map[string]bool{}
	for _, d := range docs {
		if d.DeletedBatch != "" && !seen[d.DeletedBatch] {
			seen[d.DeletedBatch] = true
			batches = append(batches, d.DeletedBatch)
		}
	}
	sizes, err := domain.CountDocBatches(ctx, batches)
	if err != nil {
		return nil, err
	}
	out := make([]TrashItem, 0, len(docs))
	for _, d := range docs {
		if !couldEditDeleted(d, viewerUID) || !helpers.IsSoftDeleted(d.DeletedAt) {
			continue
		}
		// A page deleted with its parent is the parent's entry's.
		if d.DeletedBatch != "" && d.Parent != nil && d.Parent.DeletedBatch == d.DeletedBatch {
			continue
		}
		item := TrashItem{
			UUID: d.Uuid, Title: d.Title, Icon: d.Icon, DeletedAt: *d.DeletedAt, DeletedBy: PersonOf(d.DeletedBy),
			Batch: d.DeletedBatch, CanPurge: canPurge(d, viewerUID, isAdmin),
		}
		if d.DeletedBatch != "" {
			item.Subpages = max(sizes[d.DeletedBatch]-1, 0)
			at := d.DeletedAt.Add(keep)
			item.PurgeAt = &at
		}
		out = append(out, item)
	}
	return out, nil
}

// Restored is what a restore brought back.
type Restored struct {
	Count int `json:"restored"`
	// ParentUUID is where the restored page landed: its parent, or "" at the top.
	ParentUUID string   `json:"parent_uuid"`
	UUIDs      []string `json:"-"`
}

// RestoreDoc brings a doc back from the trash with everything deleted with it
// (or from the archive with everything archived under it), for a person who
// could change it before. A page whose parent is gone lands at the top. The
// caller has decided that the person may (accessBusiness.OpenDeleted with
// RestoreNeeds); this checks the top of everything it brings back, which may
// be above the doc asked for.
//
// It brings back exactly what one delete took, and nothing more: the pages
// that stayed when it was deleted (the ones its deleter couldn't open, which
// moved up: docTake.go) were never deleted, so they stay where they moved,
// as their people may have rearranged them since. A restore that pulled them
// back would undo that without a word, and one that brought back only part of
// a delete would leave the rest in the trash for someone to find later.
func RestoreDoc(ctx context.Context, docUUID, viewerUID, viewerUUID string, isAdmin bool) (*Restored, error) {
	unlock, err := graph.Lock(ctx, DocTree.Kind)
	if err != nil {
		return nil, err
	}
	defer unlock()

	rows, err := domain.GetDocRowsByUUID(ctx, []string{docUUID}, viewerUID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrNotFound
	}
	doc := rows[0]
	if !IsDeleted(doc) {
		return nil, ErrNotInTrash
	}

	var members []*dgraphStruct.DgraphDoc
	switch {
	case helpers.IsSoftDeleted(doc.ArchivedAt):
		members, err = domain.GetArchivedSubtree(ctx, docUUID, viewerUID)
	case doc.DeletedBatch != "":
		members, err = domain.GetDocBatch(ctx, doc.DeletedBatch, viewerUID)
	default:
		members, err = domain.GetDocDeletedWithParent(ctx, docUUID, viewerUID)
	}
	if err != nil {
		return nil, err
	}
	inSet := map[string]bool{}
	for _, m := range members {
		inSet[m.Uid] = true
	}
	var uids, toTop []string
	var top *dgraphStruct.DgraphDoc
	for _, m := range members {
		uids = append(uids, m.Uid)
		if m.Parent != nil && inSet[m.Parent.Uid] {
			continue
		}
		// A top of what's restored: the person must have been able to change it.
		if !isAdmin && !couldEditDeleted(m, viewerUID) {
			return nil, ErrTrashForbidden
		}
		if m.Parent != nil && helpers.IsSoftDeleted(m.Parent.DeletedAt) {
			toTop = append(toTop, m.Uid)
		}
		if m.Uuid == docUUID || top == nil {
			top = m
		}
	}
	back, err := domain.RestoreDocs(ctx, uids, toTop, time.Now())
	if err != nil {
		return nil, err
	}
	searchFollowsRestore(back)
	now := time.Now()
	for _, id := range back {
		resourceEventBusiness.Bus.Emit(ctx, resourceEventBusiness.Event{
			Kind: resourceEventBusiness.DocRestored, ResourceKind: "doc", ResourceUUID: id,
			Actor: resourceEventBusiness.Actor{UserUUID: viewerUUID}, At: now,
		})
	}
	out := &Restored{Count: len(back), UUIDs: back}
	if top != nil && top.Parent != nil && !helpers.IsSoftDeleted(top.Parent.DeletedAt) {
		out.ParentUUID = top.Parent.Uuid
	}
	return out, nil
}

// Purged is what a delete for good removed, and how many of the pages deleted
// with it stayed in the trash because the person hasn't full access to them
// (Kept): each moved up to its parent with whatever is under it, which isn't
// counted.
type Purged struct {
	Count int `json:"purged"`
	Kept  int `json:"kept"`
}

// PurgeDoc removes a doc in the trash for good, with the pages deleted with
// it under it that the person had full access to (takesOnPurge): for its
// owner, someone with full access, or an admin, whose say covers the doc
// they chose and never a page under it. The caller has decided that the
// person may (accessBusiness.OpenDeleted with PurgeNeeds); this checks it
// again under the tree's lock.
func PurgeDoc(ctx context.Context, docUUID, viewerUID, viewerUUID string, isAdmin bool) (*Purged, error) {
	unlock, err := graph.Lock(ctx, DocTree.Kind)
	if err != nil {
		return nil, err
	}
	defer unlock()

	rows, err := domain.GetDocRowsByUUID(ctx, []string{docUUID}, viewerUID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrNotFound
	}
	doc := rows[0]
	if !IsDeleted(doc) || helpers.IsSoftDeleted(doc.ArchivedAt) {
		return nil, ErrNotInTrash
	}
	if !canPurge(doc, viewerUID, isAdmin) {
		return nil, ErrTrashForbidden
	}
	members := []*dgraphStruct.DgraphDoc{doc}
	if doc.DeletedBatch != "" {
		batch, err := domain.GetDocBatch(ctx, doc.DeletedBatch, viewerUID)
		if err != nil {
			return nil, err
		}
		members = underWithin(doc, batch)
	}
	// Only what the person had full access to goes (takesOnPurge). The rest
	// stays in the trash, moved up to the doc's parent, each top with what is
	// under it a delete of its own, so restoring one never waits on another.
	byUUID := map[string]*dgraphStruct.DgraphDoc{}
	under := make([]treePage, 0, len(members))
	for _, d := range members {
		byUUID[d.Uuid] = d
		if d.Uuid != doc.Uuid {
			under = append(under, pageOf(d))
		}
	}
	take, err := takeFor(ctx, Viewer{UID: viewerUID, UUID: viewerUUID}, pageOf(doc), under, takesOnPurge)
	if err != nil {
		return nil, err
	}
	gone := make([]*dgraphStruct.DgraphDoc, 0, len(take.Takes))
	for _, p := range take.Takes {
		gone = append(gone, byUUID[p.UUID])
	}
	var up []domain.MoveUp
	rebatch := map[string]string{}
	for _, top := range take.Stays {
		up = append(up, domain.MoveUp{UID: top.UID, Parent: parentUIDOf(doc)})
		batch := uuid.NewString()
		for _, p := range take.With[top.UUID] {
			rebatch[p.UID] = batch
		}
	}
	n, err := purge(ctx, gone, up, rebatch)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for _, d := range gone {
		resourceEventBusiness.Bus.Emit(ctx, resourceEventBusiness.Event{
			Kind: resourceEventBusiness.DocPurged, ResourceKind: "doc", ResourceUUID: d.Uuid,
			Actor: resourceEventBusiness.Actor{UserUUID: viewerUUID}, At: now,
		})
	}
	emitMovedUp(ctx, take.Stays, parentUUIDOf(doc), viewerUUID, now)
	return &Purged{Count: n, Kept: len(take.Stays)}, nil
}

func parentUIDOf(d *dgraphStruct.DgraphDoc) string {
	if d != nil && d.Parent != nil {
		return d.Parent.Uid
	}
	return ""
}

// underWithin is a doc and the members of its batch under it.
func underWithin(root *dgraphStruct.DgraphDoc, batch []*dgraphStruct.DgraphDoc) []*dgraphStruct.DgraphDoc {
	kids := map[string][]*dgraphStruct.DgraphDoc{}
	for _, d := range batch {
		if d.Parent != nil {
			kids[d.Parent.Uid] = append(kids[d.Parent.Uid], d)
		}
	}
	out := []*dgraphStruct.DgraphDoc{root}
	for i := 0; i < len(out); i++ {
		out = append(out, kids[out[i].Uid]...)
	}
	return out
}

// purge removes docs for good: from the graph (with their comments, and every
// edge others hold to them), from search, from people's places, views and
// saved items, their guest links, and their history. The pages a person's
// delete for good leaves move up and are rebatched in the same write (up,
// rebatch: domain.PurgeDocNodes); the trash's own sweep leaves none.
func purge(ctx context.Context, docs []*dgraphStruct.DgraphDoc, up []domain.MoveUp, rebatch map[string]string) (int, error) {
	var uids, uuids, comments []string
	for _, d := range docs {
		uids = append(uids, d.Uid)
		uuids = append(uuids, d.Uuid)
	}
	withComments, err := domain.GetDocCommentUIDs(ctx, uids)
	if err != nil {
		return 0, err
	}
	comments = withComments
	if err := domain.PurgeDocNodes(ctx, uids, comments, time.Now(), up, rebatch); err != nil {
		return 0, err
	}
	if err := placesBusiness.Forget(ctx, "doc", uuids); err != nil {
		helpers.LogErrorWithContext(ctx, "business/purge: places: %v", err)
	}
	purgeDocRows(ctx, uuids)
	for _, id := range uuids {
		if err := opensearchModels.ForgetEntry(ctx, openSearchStruct.DOC_INDEX, id); err != nil {
			helpers.LogErrorWithContext(ctx, "business/purge: search entry %s: %v", id, err)
		}
	}
	return len(uuids), nil
}

// purgeDocRows removes what Postgres holds of docs gone for good: their
// history (and its stored bodies), who viewed them, saved-for-later entries,
// and guest links (with the guests' comments, which cascade). Each table is
// tried on its own, and a failure is logged: the docs are gone either way.
func purgeDocRows(ctx context.Context, uuids []string) {
	if len(uuids) == 0 || postgresInit.DBConn == nil {
		return
	}
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	db := postgresInit.DBConn.SqlDB
	rows, err := db.QueryContext(c, `DELETE FROM doc_snapshots WHERE doc_uuid = ANY($1) RETURNING object_key`, uuids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/purgeDocRows doc_snapshots: %v", err)
	} else {
		var keys []string
		for rows.Next() {
			var k string
			if rows.Scan(&k) == nil {
				keys = append(keys, k)
			}
		}
		_ = rows.Close()
		for _, k := range keys {
			removeDocSnapshotObject(ctx, k)
		}
	}
	for _, q := range []string{
		`DELETE FROM resource_views WHERE resource_type = 'doc' AND resource_uuid = ANY($1)`,
		`DELETE FROM saved_items WHERE item_type = 'doc' AND item_id = ANY($1)`,
		`DELETE FROM guest_grants WHERE resource_type = 'doc' AND resource_id = ANY($1)`,
	} {
		if _, err := db.ExecContext(c, q, uuids); err != nil {
			helpers.LogErrorWithContext(ctx, "business/purgeDocRows %s: %v", strings.Fields(q)[2], err)
		}
	}
}

// purgeExpiredTrash is the trash's retention: deletes older than the
// retention period, removed for good, a batch at a time. Deletes from before
// the trash existed (no batch) are left as they were.
func purgeExpiredTrash(ctx context.Context) error {
	due, err := domain.GetDocsTrashedBefore(ctx, time.Now().Add(-trashRetention()), trashSweepBatch)
	if err != nil || len(due) == 0 {
		return err
	}
	n, err := purge(ctx, due, nil, nil)
	if n > 0 {
		helpers.LogInfoWithContext(ctx, "doc trash: removed %d docs for good after the retention period", n)
	}
	if err == nil {
		// The server's own purge is recorded like anyone's (I14): the audit
		// lists it, and what the docs left behind goes with them.
		for _, d := range due {
			resourceEventBusiness.Bus.Emit(ctx, resourceEventBusiness.Event{
				Kind: resourceEventBusiness.DocPurged, ResourceKind: "doc", ResourceUUID: d.Uuid,
				Actor: resourceEventBusiness.Actor{System: true}, At: time.Now(),
				Data: map[string]string{"reason": "retention"},
			})
		}
	}
	return err
}

func init() {
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "doc.trash.purge", Every: time.Hour, Run: purgeExpiredTrash})
}
