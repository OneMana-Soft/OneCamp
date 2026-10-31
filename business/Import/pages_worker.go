package business

// The pages stage (CapPages): a provider's trees of pages become docs, a tree
// at a time, through the docs pipeline (business/ContentIO MakePages): each
// page under the page it sat under, its pictures uploaded, its content seeded
// through the Live Doc Writer, and a link from one of its pages to another
// pointed at the doc the other became. A tree arrives at the top of the
// importing admin's docs, private to them, as a new page there is.
//
// A tree is one chunk, and is retried whole: its docs are recorded in the id
// map (EntityDoc) once it is made, and a tree whose root is there is done. A
// tree an earlier import of the same workspace brought, and whose doc is still
// there, is left as it is. The chunk's claim is kept alive while its tree is
// read and made, so the reaper never hands it to a second worker.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
	contentIOBusiness "github.com/akashc777/OneCamp/business/ContentIO"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	docDomain "github.com/akashc777/OneCamp/domain/Doc"
	importDomain "github.com/akashc777/OneCamp/domain/Import"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// pageWorkers is how many trees are read at once. A provider's rate limit is
// shared by all of them, so more would only wait their turn.
var pageWorkers = envIntDefault("IMPORT_PAGE_WORKERS", 2)

// heartbeatEvery is how often a tree's chunk renews its claim, well inside the
// reaper's five minutes.
var heartbeatEvery = 30 * time.Second

// Seams for tests: the docs pipeline (making docs and taking them away again),
// renewing a chunk's claim, whether a doc is still there, and a page's
// comments (the doc read for them, and one written).
var (
	// commentDoc is the doc as a comment on it is written against, read as
	// the person importing (its owner), as the comment route reads it.
	commentDoc = func(ctx context.Context, docUUID, importerUID string) (*dgraphStruct.DgraphDoc, error) {
		return docDomain.GetBasicDgraphDocByUUID(ctx, docUUID, importerUID)
	}
	writeDocComment = docBusiness.ImportDocComment
	// mappedIDs and peopleByUUID read many at once: a page's comments and
	// their authors are resolved in a read each, never one per comment.
	mappedIDs    = importDomain.LookupIdMappingsBatch
	peopleByUUID = userDomain.GetPeopleByUUIDs
	makePages    = contentIOBusiness.MakePages
	unmakePages  = contentIOBusiness.UnmakePages
	renewClaim   = importModels.HeartbeatChunk
	docIsLive    = func(ctx context.Context, docUUID, viewerUID string) bool {
		rows, err := docBusiness.DocRows(ctx, []string{docUUID}, viewerUID)
		return err == nil && len(rows) == 1 && !helpers.IsSoftDeleted(rows[0].DeletedAt) && !helpers.IsSoftDeleted(rows[0].ArchivedAt)
	}
)

// errTryTreeAgain is a tree some of whose pages couldn't be made because the
// graph or the document service failed part way: what was made is taken away
// again, and the chunk is tried again whole later, so a tree never stays half
// made.
var errTryTreeAgain = errors.New("some pages of the tree could not be written; it will be tried again")

func processPageChunk(ctx context.Context, prov importProvider.Provider, job *importModels.Job,
	opts importProvider.JobOptions, chunk *importModels.Chunk, importingUser *userModels.UserInfo) error {

	src, ok := prov.(importProvider.PageSource)
	if !ok {
		return fmt.Errorf("%s brings no pages", prov.Name())
	}
	if chunk.ParentSourceId == nil || *chunk.ParentSourceId == "" {
		return fmt.Errorf("page chunk missing its root page")
	}
	root := *chunk.ParentSourceId
	if made, err := importModels.LookupIdMapping(ctx, job.Id, importModels.EntityDoc, root); err != nil {
		return err
	} else if made != uuid.Nil {
		return importModels.FinishChunk(ctx, chunk.Id, 0, nil)
	}
	if before, err := importModels.LookupWorkspaceMapping(ctx, job.Provider, job.SourceWorkspaceName, importModels.EntityDoc, root); err == nil &&
		before != uuid.Nil && docIsLive(ctx, before.String(), importingUser.UserDgraphInfo.Uid) {
		if err := importModels.RecordMapping(ctx, job.Id, job.Provider, job.SourceWorkspaceName, importModels.EntityDoc, root, before, nil,
			mustMarshal(map[string]any{"matched_by": "workspace_map"}), false); err != nil {
			return err
		}
		importModels.LogImportError(ctx, job.Id, &chunk.Id, importModels.EntityDoc, root, importModels.SeverityWarning,
			"PAGES_IMPORTED_BEFORE", "These pages came across in an earlier import, so they were left as they are; pages added under them since are not brought across.", nil)
		return importModels.FinishChunk(ctx, chunk.Id, 0, nil)
	}

	var read atomic.Int64
	stop := keepClaimed(ctx, chunk.Id, func() int { return int(read.Load()) })
	defer stop()
	pages, err := src.PagesOf(ctx, job, opts, root, func(n int) { read.Store(int64(n)) })
	if err != nil {
		if d, ok := importProvider.IsRateLimited(err); ok {
			waitOutRateLimit(ctx, job, err, d)
			_ = importModels.ResetChunkForRetry(ctx, chunk.Id, "provider rate-limited")
			return errReaperOnly
		}
		return err
	}
	if len(pages) == 0 {
		return importModels.FinishChunk(ctx, chunk.Id, 0, nil)
	}

	who := accessBusiness.SubjectOf(importingUser)
	made, err := makePages(ctx, who, "", docPages(pages))
	if err != nil {
		return err
	}
	if made.TryAgain {
		if left := unmakePages(ctx, who, made); left > 0 {
			importModels.LogImportError(ctx, job.Id, &chunk.Id, importModels.EntityDoc, root, importModels.SeverityWarning,
				"PAGES_LEFT_BEHIND", fmt.Sprintf("%d docs made before the import stopped couldn't be taken away; delete them if the tree comes across again.", left), nil)
		}
		return errTryTreeAgain
	}
	// What the tree couldn't bring goes to the report together.
	notes := newChunkNotes(job.Id, &chunk.Id)
	for _, p := range pages {
		id, err := uuid.Parse(made.Docs[p.SourceID])
		if err != nil {
			continue
		}
		var parent *string
		if p.ParentSourceID != "" {
			parent = strPtr(p.ParentSourceID)
		}
		if err := importModels.RecordMapping(ctx, job.Id, job.Provider, job.SourceWorkspaceName, importModels.EntityDoc, p.SourceID, id, parent, nil, true); err != nil {
			notes.add(importModels.EntityDoc, p.SourceID, importModels.SeverityError,
				"PAGE_NOT_RECORDED", "The page came across, but the import couldn't note it, so a roll back leaves it in place.")
		}
	}
	for _, s := range made.Skipped {
		notes.add(importModels.EntityDoc, s.What, importModels.SeverityWarning, "PAGE_PART_LEFT_OUT", s.Why)
	}
	for _, p := range pages {
		if len(p.Comments) > 0 {
			pageComments(ctx, job, chunk, importingUser, made.Docs[p.SourceID], p)
		}
	}
	notes.flush(ctx)
	return importModels.FinishChunk(ctx, chunk.Id, len(made.Docs), nil)
}

// pageComments writes a page's comments on the doc it became (docs N1), as
// they were made: by their author when the import knows them as a person
// here, by the person importing when not, at the time they were made. Each
// is written once (EntityComment), and none tells anyone (ImportDocComment).
// One that can't be written is said, and the rest go on.
func pageComments(ctx context.Context, job *importModels.Job, chunk *importModels.Chunk, importingUser *userModels.UserInfo, docUUID string, p importProvider.SourcePage) {
	if docUUID == "" {
		return
	}
	doc, err := commentDoc(ctx, docUUID, importingUser.UserDgraphInfo.Uid)
	if err != nil || doc == nil {
		importModels.LogImportError(ctx, job.Id, &chunk.Id, importModels.EntityComment, p.SourceID, importModels.SeverityWarning,
			"COMMENTS_LEFT_OUT", "The page came across, but its comments couldn't be added to it.", nil)
		return
	}
	ids := make([]string, 0, len(p.Comments))
	for _, c := range p.Comments {
		ids = append(ids, c.SourceID)
	}
	done, err := mappedIDs(ctx, job.Id, importModels.EntityComment, ids)
	if err != nil {
		// Not knowing which came across already, write none: a retry does.
		importModels.LogImportError(ctx, job.Id, &chunk.Id, importModels.EntityComment, p.SourceID, importModels.SeverityWarning,
			"COMMENTS_LEFT_OUT", "The page came across, but its comments couldn't be added to it.", nil)
		return
	}
	authors := commentAuthors(ctx, job, p.Comments)
	// Each comment is written on its own and recorded at once, so a chunk
	// tried again after a crash never writes one twice (store-loop baseline).
	for _, c := range p.Comments {
		if done[c.SourceID] != uuid.Nil {
			continue
		}
		author := importingUser
		if a := authors[c.AuthorSourceID]; a != nil {
			author = a
		}
		at := c.Created
		if at.IsZero() {
			at = time.Now()
		}
		id, err := writeDocComment(ctx, doc, author, c.Body, at)
		if err != nil {
			importModels.LogImportError(ctx, job.Id, &chunk.Id, importModels.EntityComment, c.SourceID, importModels.SeverityError,
				"COMMENT_IMPORT_FAILED", err.Error(), nil)
			continue
		}
		if made, err := uuid.Parse(id); err == nil {
			if err := importModels.RecordMapping(ctx, job.Id, job.Provider, job.SourceWorkspaceName, importModels.EntityComment, c.SourceID, made, strPtr(p.SourceID),
				mustMarshal(map[string]any{"page_source_id": p.SourceID, "author": c.AuthorSourceID}), true); err != nil {
				importModels.LogImportError(ctx, job.Id, &chunk.Id, importModels.EntityComment, c.SourceID, importModels.SeverityError,
					"COMMENT_IMPORT_FAILED", err.Error(), nil)
			}
		}
	}
}

// commentAuthors are the people who wrote comments, by their source id, read
// for all of them at once: who the import brought across (the id map), and
// who of those the graph knows. Only a person the graph knows can be a
// comment's author; anyone else's comment is the importer's.
func commentAuthors(ctx context.Context, job *importModels.Job, comments []importProvider.SourceComment) map[string]*userModels.UserInfo {
	out := map[string]*userModels.UserInfo{}
	seen := map[string]bool{}
	var ids []string
	for _, c := range comments {
		if c.AuthorSourceID != "" && !seen[c.AuthorSourceID] {
			seen[c.AuthorSourceID] = true
			ids = append(ids, c.AuthorSourceID)
		}
	}
	if len(ids) == 0 {
		return out
	}
	mapped, err := mappedIDs(ctx, job.Id, importModels.EntityUser, ids)
	if err != nil || len(mapped) == 0 {
		return out
	}
	uuids := make([]string, 0, len(mapped))
	for _, id := range mapped {
		uuids = append(uuids, id.String())
	}
	people, err := peopleByUUID(ctx, uuids)
	if err != nil {
		return out
	}
	known := map[string]*dgraphStruct.DgraphUser{}
	for _, person := range people {
		if person != nil && person.Uid != "" {
			known[strings.ToLower(person.Uuid)] = person
		}
	}
	for source, id := range mapped {
		if person := known[strings.ToLower(id.String())]; person != nil {
			out[source] = &userModels.UserInfo{UserPostgresInfo: userModels.User{Id: id}, UserDgraphInfo: *person}
		}
	}
	return out
}

// keepClaimed renews the chunk's claim every heartbeatEvery, with how many
// pages have been read, until stop is called. A renewal that fails is logged:
// the claim may lapse, and the reaper then hands the tree to a second worker.
// The beat runs behind the recover guard (GoSafeNamed), so a panic in it ends
// the beat, never the server, and stop still returns.
func keepClaimed(ctx context.Context, chunkID uuid.UUID, read func() int) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	helpers.GoSafeNamed("import:keepClaimed", func() {
		defer close(done)
		tick := time.NewTicker(heartbeatEvery)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if err := renewClaim(ctx, chunkID, read(), nil); err != nil && ctx.Err() == nil {
					helpers.LogWarnWithContext(ctx, "import: chunk %s couldn't renew its claim: %v", chunkID, err)
				}
			}
		}
	})
	return func() {
		cancel()
		<-done
	}
}

// docPages are a provider's pages as the docs pipeline makes them.
func docPages(pages []importProvider.SourcePage) []contentIOBusiness.Page {
	out := make([]contentIOBusiness.Page, 0, len(pages))
	for _, p := range pages {
		body, err := contentcodec.FromHTML(p.HTML, contentcodec.HTMLOptions{Foreign: true, Assets: len(p.Files) > 0})
		if err != nil {
			body = contentcodec.Normalize(nil, nil)
		}
		pg := contentIOBusiness.Page{Key: p.SourceID, ParentKey: p.ParentSourceID, Title: p.Title, Icon: p.Icon, Body: body}
		if len(p.Files) > 0 {
			pg.Assets = contentIOBusiness.FileAssets(p.Files)
		}
		out = append(out, pg)
	}
	return out
}
