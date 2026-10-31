package business

// Every task comment's search entry that names no project, rewritten as a
// person's comment's entry is written, once, but for the comments of imports
// that ran before their provider's fix, which stay out of search.
//
// An import wrote its comments' search entries itself, with each comment's
// HTML and without its task's project, until it wrote the entry a person's
// comment gets (domain/Comment TaskCommentInSearch). Search shows a task
// comment to its project's people through the project its entry names, so
// nobody ever found a comment that came in with an import. This rewrites
// those entries from the graph, through TaskCommentInSearch: the text as plain
// text, the task and the task's project. Whether and why an entry is deleted
// stays as it was; a comment deleted in the graph is left as it is; and a
// comment of an archived project is marked deleted with the project, as
// archiving it marked its other comments, since the project's people's search
// reaches an archived project too.
//
// An import that ran before its provider's fix (importFixes) may have shown a
// project's people what its source kept from them: Jira's comments kept to a
// role and issues under a security level, a Linear private team's issues, a
// Basecamp project to its clients, a Todoist or Notion project to people it
// wasn't shared with. Such an import's comments are held back: their entries
// are left naming no project, so nobody finds them, until what the import
// brought is checked against its source again and that has settled
// (recheck.go; commentRelease.go lets them in then). So are the comments no
// import recorded, and those of an import of such a provider that can't be
// dated: a comment is let into search only when it is known to be safe.
//
// It runs in the background at startup until it has run to the end once
// (CommentSearchRepair). Running it again is harmless: it rewrites only
// entries that still name no project, and holds back the same comments. No
// migration: the comments are in the graph and the entries in search.

import (
	"context"
	"fmt"
	"time"

	repairBusiness "github.com/akashc777/OneCamp/business/Repair"
	commentDomain "github.com/akashc777/OneCamp/domain/Comment"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	openSearchCommentModels "github.com/akashc777/OneCamp/models/openSearch/Comment"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	reviewModel "github.com/akashc777/OneCamp/models/postgres/ImportReview"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
)

const (
	taskCommentSearchRepairKey  = "task_comments_search_project_v1"
	taskCommentSearchRepairPage = 100
	// taskCommentSearchRepairTries bounds the retries in one process: about
	// three and a half hours of waits, then the next start tries again.
	taskCommentSearchRepairTries = 12
)

// Search, the graph and the imports' records as the repair reads and writes
// them, and when each fix was first run here. Seams for tests.
var (
	entriesWithoutProject = openSearchCommentModels.TaskCommentEntriesWithoutProject
	commentsForSearch     = commentDomain.TaskCommentsForSearch
	rewriteEntries        = openSearchCommentModels.RewriteTaskCommentEntries
	commentImports        = importModels.CommentImports
	fixMoment             = FixedAt
	settledImports        = reviewModel.SettledImports
)

// heldBackUntil is, by provider, the fix before which an import's comments
// are held back (see above). Comments of other providers' imports are
// rewritten whenever their imports ran.
var heldBackUntil = map[string]string{
	importModels.ProviderJira:     JiraRestrictionsFixed,
	importModels.ProviderLinear:   LinearPrivateTeamsFixed,
	importModels.ProviderBasecamp: BasecampClientsFixed,
	importModels.ProviderTodoist:  MembershipFixed,
	importModels.ProviderNotion:   MembershipFixed,
}

// TaskCommentSearchRepair is what one pass did.
type TaskCommentSearchRepair struct {
	Entries   int // entries that named no project
	Rewritten int // of which rewritten with their task's project
	Archived  int // of those, comments of archived projects, marked deleted with them unless deleted already
	Deleted   int // comments deleted in the graph, their entries left as they were
	HeldBack  int // comments of imports before their provider's fix, or of none that can be dated, left as they were
	Orphaned  int // comments the graph doesn't hold whole (no task, project or writer), left as they were
}

func (r TaskCommentSearchRepair) String() string {
	return fmt.Sprintf("%d task comment entries named no project: %d rewritten (%d in archived projects, marked deleted with them unless deleted already); "+
		"%d of deleted comments, %d of comments held back until their imports are checked again and %d of comments the graph doesn't hold whole, left as they were",
		r.Entries, r.Rewritten, r.Archived, r.Deleted, r.HeldBack, r.Orphaned)
}

// CommentSearchRepair rewrites the task comment entries that name no project
// (RepairTaskCommentSearch) once per install.
var CommentSearchRepair = repairBusiness.Once[TaskCommentSearchRepair]{
	Key: taskCommentSearchRepairKey, Name: "import.comment-search-repair", Tries: taskCommentSearchRepairTries, Repair: RepairTaskCommentSearch,
}

// RepairTaskCommentSearch rewrites the task comment entries that name no
// project, a page at a time, reading the graph as the system: the entries it
// writes are what search reads with each reader's rule. Idempotent.
func RepairTaskCommentSearch(ctx context.Context) (rep TaskCommentSearchRepair, err error) {
	fixed, err := fixMoments()
	if err != nil {
		return rep, err
	}
	settled, err := settledImports(ctx)
	if err != nil {
		return rep, err
	}
	ctx = taskread.WithSystem(ctx)
	after := ""
	for {
		ids, next, err := entriesWithoutProject(ctx, after, taskCommentSearchRepairPage)
		if err != nil {
			return rep, err
		}
		if err := repairTaskCommentPage(ctx, &rep, ids, fixed, settled); err != nil {
			return rep, err
		}
		if next == "" {
			return rep, nil
		}
		after = next
	}
}

// fixMoments is, by provider in heldBackUntil, when this install first ran
// its fix. A provider whose fix no server has claimed yet is absent: all its
// imports came before it.
func fixMoments() (map[string]time.Time, error) {
	fixed := make(map[string]time.Time, len(heldBackUntil))
	for provider, key := range heldBackUntil {
		at, ok, err := fixMoment(key)
		if err != nil {
			return nil, err
		}
		if ok {
			fixed[provider] = at
		}
	}
	return fixed, nil
}

// heldBack is whether a comment the imports in brought stays out of search:
// when no import recorded it, or when one of them is of a provider in
// heldBackUntil, ran before its fix (fixed, by fixMoments) or can't be
// dated, and hasn't settled its re-check against its source (settled, by
// import). Pure.
func heldBack(in []importModels.CommentImport, fixed map[string]time.Time, settled map[uuid.UUID]bool) bool {
	if len(in) == 0 {
		return true
	}
	for _, c := range in {
		if _, gated := heldBackUntil[c.Provider]; !gated || settled[c.Import] {
			continue
		}
		at, ok := fixed[c.Provider]
		if !ok || c.At == nil || c.At.Before(at) {
			return true
		}
	}
	return false
}

// repairTaskCommentPage rewrites a page of entries from their comments, in
// one write that marks those of archived projects deleted with their
// projects, and leaves the held-back ones as they are.
func repairTaskCommentPage(ctx context.Context, rep *TaskCommentSearchRepair, ids []string, fixed map[string]time.Time, settled map[uuid.UUID]bool) error {
	rep.Entries += len(ids)
	parsed := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if p, err := uuid.Parse(id); err == nil {
			parsed = append(parsed, p)
		}
	}
	comments, err := commentsForSearch(ctx, parsed)
	if err != nil {
		return err
	}
	imported, err := commentImports(ctx, parsed)
	if err != nil {
		return err
	}
	importsOf := make(map[string][]importModels.CommentImport, len(imported))
	for _, c := range imported {
		importsOf[c.Comment.String()] = append(importsOf[c.Comment.String()], c)
	}
	entries := make(map[string]*openSearchStruct.OpenSearchComment, len(comments))
	archived := map[string]int64{}
	deleted, held := 0, 0
	for _, c := range comments {
		switch {
		case helpers.IsSoftDeleted(c.DeletedAt):
			deleted++
			continue
		case !wholeForSearch(c):
			continue
		case heldBack(importsOf[c.Uuid], fixed, settled):
			held++
			continue
		}
		entries[c.Uuid] = commentDomain.TaskCommentInSearch(c.Task, c.Uuid, c.CommentBy, c.Text, *c.CreatedAt)
		if p := c.Task.Project; helpers.IsSoftDeleted(p.DeletedAt) {
			archived[c.Uuid] = p.DeletedAt.Unix()
		}
	}
	rep.Deleted += deleted
	rep.HeldBack += held
	rep.Orphaned += len(ids) - len(entries) - deleted - held
	written, err := rewriteEntries(ctx, entries, archived)
	rep.Rewritten += written
	if err != nil {
		return err
	}
	rep.Archived += len(archived)
	return nil
}

// wholeForSearch is whether the graph holds a comment whole enough to write
// its entry from: its task, the task's project, its writer and when it was
// written.
func wholeForSearch(c *dgraphStruct.DgraphComment) bool {
	return c.Task != nil && c.Task.Project != nil && c.Task.Project.Uuid != "" && c.CommentBy != nil && c.CreatedAt != nil
}
