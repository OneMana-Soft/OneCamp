package domain

import (
	"context"
	"errors"
	"fmt"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/models/taskread"
)

// TaskCommentOwnersPage is up to first task comments after the comment uid
// after ("" from the start), in uid order, each with its uid, its writer's
// and its owner's uids, and its task's assignee and creator (uid and uuid),
// the task as the reader of ctx may see it: what business/Task's
// comment-owner repair re-owns them by.
func TaskCommentOwnersPage(ctx context.Context, after string, first int) ([]*dgraphStruct.DgraphComment, error) {
	if after != "" && !helpers.IsDgraphUID(after) {
		return nil, errors.New("domain/TaskCommentOwnersPage: after is not a uid")
	}
	tr, err := taskread.From(ctx)
	if err != nil {
		return nil, err
	}
	window := fmt.Sprintf("first: %d", first)
	if after != "" {
		window += ", after: " + after
	}
	return dgexec.List[*dgraphStruct.DgraphComment](ctx, tr.Fill(`{
		page(func: has(comment_task), `+window+`) {
			uid
			comment_by { uid }
			comment_on_content_added_by { uid }
			comment_task @filter(has(task_uuid) `+taskread.Mark+`) {
				task_assignee { uid user_uuid }
				task_created_by { uid user_uuid }
			}
		}
	}`), nil, "page")
}

// ReownComments sets each comment's owner (comment_on_content_added_by,
// which the feed and the badge list a comment by), by comment uid, in one
// write. No node is deleted: the one edge is set.
func ReownComments(ctx context.Context, owners map[string]string) error {
	if len(owners) == 0 {
		return nil
	}
	nodes := make([]map[string]any, 0, len(owners))
	for comment, owner := range owners {
		if !helpers.IsDgraphUID(comment) || !helpers.IsDgraphUID(owner) {
			return fmt.Errorf("domain/ReownComments: %q or %q is not a uid", comment, owner)
		}
		nodes = append(nodes, map[string]any{"uid": comment, "comment_on_content_added_by": map[string]string{"uid": owner}})
	}
	_, err := dgexec.Set(ctx, nodes)
	return err
}
