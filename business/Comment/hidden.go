package business

import (
	"context"
	"time"

	"github.com/google/uuid"

	domain "github.com/akashc777/OneCamp/domain/Comment"
	globalSearchDomain "github.com/akashc777/OneCamp/domain/GlobalSearch"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	commentModels "github.com/akashc777/OneCamp/models/postgres/Comment"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// A comment taken out of sight and brought back as it was. One a person
// deletes (DeleteComment) is gone for good: its search entries are marked
// "user". One its task's or document's archive hides is marked "cascade",
// which their restore clears. HideComment marks the entries with its caller's
// own marker (by), over a cascade's too, so only ShowComment with that marker
// brings them back, never a restore of what the comment is on; and it takes
// the workspace memory captured from the comment out of sight under the same
// marker, over a project archive's too, rather than deleting it.

// A comment's search entries: its own, its files' and its AI entry, by the
// field that names it in each.
var (
	hiddenEntryFields  = []string{"comment_id", "attachment_comment_id", "content_uuid"}
	hiddenEntryIndexes = []string{"comments", "attachments", "ai_embeddings"}
)

// memorySource is the workspace memory's name for what is captured from a
// comment.
const memorySource = "comment"

// HideComment takes a live comment out of every list (the graph and
// Postgres), out of search (its entry, its files' and its AI entry, marked by)
// and out of workspace memory (by too), at at, and answers whether it is
// hidden at at now. Postgres goes last, so a comment it still has live is one
// to hide again, with the same at: each step is idempotent. One deleted
// otherwise is left deleted.
func HideComment(ctx context.Context, commentUUID uuid.UUID, at time.Time, by string) (bool, error) {
	id := commentUUID.String()
	if err := domain.UpdateDgraphCommentInAPost(ctx, deletedAt(id, at)); err != nil {
		return false, err
	}
	if err := globalSearchDomain.HideInOpenSearch(hiddenEntryFields, id, at.Unix(), hiddenEntryIndexes, by); err != nil {
		return false, err
	}
	if err := ai.HideMemoryBySources(ctx, memorySource, []string{id}, by); err != nil {
		return false, err
	}
	return commentModels.Hide(ctx, commentUUID, at)
}

// Away is where a comment's parent is as the comment comes back: out of
// sight since Since (the zero time: in sight), in the trash, or archived with
// its project (Archived), whose archive took the workspace memory captured
// from the comment too; a task's trash takes only search.
type Away struct {
	Since    time.Time
	Archived bool
}

// ShowComment brings back a comment HideComment hid at at with by, as its
// parent is now (away): in sight, or out of sight with it, so that parent's
// restore brings it back. It answers whether the comment is live now.
// Postgres goes first: a comment deleted since stays deleted, and nothing
// else of it comes back. A retry finds it live there, and redoes the rest.
func ShowComment(ctx context.Context, commentUUID uuid.UUID, at time.Time, by string, away Away) (bool, error) {
	shown, err := commentModels.Show(ctx, commentUUID, at)
	if err != nil || !shown {
		return false, err
	}
	id := commentUUID.String()
	if err := domain.UpdateDgraphCommentInAPost(ctx, deletedAt(id, time.Time{})); err != nil {
		return false, err
	}
	if away.Since.IsZero() {
		err = globalSearchDomain.SyncCascadingUnarchiveInOpenSearch(hiddenEntryFields, id, hiddenEntryIndexes, by)
	} else {
		err = globalSearchDomain.HandOverInOpenSearch(hiddenEntryFields, id, by, away.Since.Unix(), hiddenEntryIndexes, "cascade")
	}
	if err != nil {
		return false, err
	}
	if err := ai.ShowMemoryBySources(ctx, memorySource, []string{id}, by, away.Archived); err != nil {
		return false, err
	}
	return true, nil
}

// deletedAt is the graph's write of a comment's deletion time: the zero time
// is a live comment.
func deletedAt(commentUUID string, at time.Time) *dgraphStruct.DgraphComment {
	return &dgraphStruct.DgraphComment{Uid: "uid(co)", Uuid: commentUUID, DeletedAt: &at, DType: []string{"Comment"}}
}
