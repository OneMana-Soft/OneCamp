package business

// The upgrade's pass over old task comments (NOTIF-H2). Until 6 Nov 2026 a
// task comment was owned in the graph (comment_on_content_added_by) by its
// own writer, so the feed and the badge, which list a comment for its owner
// and never for its writer, listed it for nobody, while the live item and
// the push went to the task's Addressee (business/Comment). Once per
// install, as the server starts, each comment still owned by its writer is
// re-owned to its task's Addressee, as a new comment is owned
// (addressedTo).
//
// Before any write the whole pass is counted and the count logged (a dry
// run). Then a page of comments is read by uid and the page's changes are
// one write; where it got to is kept (repairBusiness.SaveProgress) after
// each page, so a run cut off (a restart, a power-off) goes on from the last
// page kept, and a page written but not kept is read again and changes
// nothing. A comment owned by someone other than its writer, or whose task
// names no Addressee but its writer, is left as it is, so a second pass
// writes nothing. No node is deleted: the one edge is set.
//
// A re-owned comment lists as what it is, an old comment: it is older than
// its Addressee's last look at their Inbox, so it counts on no badge, unless
// they have never looked at their Inbox at all (then the badge counts the
// feed's first page, as it does for every old item).

import (
	"context"
	"fmt"

	businessComment "github.com/akashc777/OneCamp/business/Comment"
	repairBusiness "github.com/akashc777/OneCamp/business/Repair"
	commentDomain "github.com/akashc777/OneCamp/domain/Comment"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/models/taskread"
)

const commentOwnerRepairKey = "tasks.comment_owner_v1"

// commentOwnerPage is how many comments one read takes and one write
// covers at most: a seam for tests.
var commentOwnerPage = 500

// CommentOwnerRepair re-owns, once per install, each task comment owned by
// its writer to whom it is addressed (repairCommentOwners).
var CommentOwnerRepair = repairBusiness.Once[CommentOwners]{
	Key: commentOwnerRepairKey, Name: "task.comment-owner-repair", Tries: 12, Repair: repairCommentOwners,
}

// CommentOwners is what the repair has done.
type CommentOwners struct {
	DryRun  int `json:"dry_run"` // comments the count before any write found to re-own
	Read    int `json:"read"`    // task comments read by the writing pass
	Reowned int `json:"reowned"` // of which re-owned to whom they are addressed
	Writes  int `json:"writes"`  // graph writes it took, one a page at most
}

func (r CommentOwners) String() string {
	return fmt.Sprintf("dry run: %d task comments owned by their writer to re-own; %d read, %d re-owned to whom they are addressed in %d writes",
		r.DryRun, r.Read, r.Reowned, r.Writes)
}

// commentOwnerProgress is what earlier runs did that a later one goes on
// from: whether the dry run has counted, the last comment uid of the last
// page kept, and the report.
type commentOwnerProgress struct {
	Counted bool          `json:"counted"`
	After   string        `json:"after"`
	Report  CommentOwners `json:"report"`
}

// Seams for tests: the reads, the writes, the progress and the log.
var (
	commentOwnersPage = commentDomain.TaskCommentOwnersPage
	reownComments     = commentDomain.ReownComments
	loadCommentOwners = repairBusiness.Progress[commentOwnerProgress]
	saveCommentOwners = func(p commentOwnerProgress) error { return repairBusiness.SaveProgress(commentOwnerRepairKey, p) }
	commentOwnerLog   = func(format string, args ...any) { helpers.MessageLogs.InfoLog.Printf(format, args...) }
)

// repairCommentOwners counts, then re-owns, a page at a time, every task
// comment owned by its writer to its task's Addressee, reading the graph as
// the system. A read or a write that fails stops the run, to run again from
// the last page kept. Idempotent.
func repairCommentOwners(ctx context.Context) (CommentOwners, error) {
	ctx = taskread.WithSystem(ctx)
	p, err := loadCommentOwners(commentOwnerRepairKey)
	if err != nil {
		return CommentOwners{}, err
	}
	if !p.Counted {
		n := 0
		if err := eachCommentOwnersPage(ctx, "", func(page []*dgraphStruct.DgraphComment) error {
			n += len(reownings(page))
			return nil
		}); err != nil {
			return p.Report, err
		}
		p.Counted, p.Report.DryRun = true, n
		if err := saveCommentOwners(p); err != nil {
			return p.Report, err
		}
		commentOwnerLog("task.comment-owner-repair DRY RUN: %d task comments are owned by their writer and will be re-owned to whom they are addressed; nothing written yet", n)
	}
	err = eachCommentOwnersPage(ctx, p.After, func(page []*dgraphStruct.DgraphComment) error {
		to := reownings(page)
		if len(to) > 0 {
			if err := reownComments(ctx, to); err != nil {
				return err
			}
			p.Report.Reowned += len(to)
			p.Report.Writes++
		}
		p.Report.Read += len(page)
		p.After = page[len(page)-1].Uid
		return saveCommentOwners(p)
	})
	return p.Report, err
}

// eachCommentOwnersPage hands each page of task comments after the uid
// after to each, in uid order, until a short page.
func eachCommentOwnersPage(ctx context.Context, after string, each func([]*dgraphStruct.DgraphComment) error) error {
	for {
		page, err := commentOwnersPage(ctx, after, commentOwnerPage)
		if err != nil || len(page) == 0 {
			return err
		}
		if err := each(page); err != nil {
			return err
		}
		if len(page) < commentOwnerPage {
			return nil
		}
		after = page[len(page)-1].Uid
	}
}

// reownings is, by comment uid, the owner each comment of page is to have:
// its task's Addressee's uid, for each owned by its writer whose task names
// an Addressee other than the writer. Pure.
func reownings(page []*dgraphStruct.DgraphComment) map[string]string {
	to := map[string]string{}
	for _, c := range page {
		if c == nil || c.CommentBy == nil || c.ContentAddedBy == nil || c.CommentBy.Uid == "" || c.ContentAddedBy.Uid != c.CommentBy.Uid {
			continue
		}
		if a := businessComment.Addressee(c.Task); a != nil && a.Uid != "" && a.Uid != c.CommentBy.Uid {
			to[c.Uid] = a.Uid
		}
	}
	return to
}
