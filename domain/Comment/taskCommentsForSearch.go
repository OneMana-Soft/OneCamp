package domain

import (
	"context"

	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
)

// TaskCommentsForSearch is the comments named, as a task comment's search
// entry is written from them (TaskCommentInSearch): the text, who wrote it
// and when, whether it is deleted, and its task with the task's project and
// whether that is archived. A comment the graph doesn't hold is left out; one
// whose task the reader of ctx may not see comes without its task. Read a
// few hundred at a time at most.
func TaskCommentsForSearch(ctx context.Context, ids []uuid.UUID) ([]*dgraphStruct.DgraphComment, error) {
	list := dgraphquery.UUIDListOf(ids)
	if list == "" {
		return nil, nil
	}
	tr, err := taskread.From(ctx)
	if err != nil {
		return nil, err
	}
	return dgexec.List[*dgraphStruct.DgraphComment](ctx, tr.Fill(`{
		comments(func: eq(comment_uuid, [`+list+`])) {
			comment_uuid comment_text comment_created_at comment_deleted_at
			comment_by { user_uuid user_name user_full_name user_email_id user_profile_object_key }
			comment_task @filter(has(task_uuid) `+taskread.Mark+`) {
				task_uuid
				task_project { project_uuid project_name project_deleted_at }
			}
		}
	}`), nil, "comments")
}
