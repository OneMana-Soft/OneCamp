package models

// The graph side of anchored threads (business/Thread): a doc's comments stay
// Comment nodes on doc_comments, and a comment in a thread carries the
// thread's id in comment_thread_id (PROGRAM.md §3.8). Comments written before
// threads, or through the old comment route, have none.
//
// Nothing here asks the graph for comment_thread_id at the root, so it works
// before the predicate is declared with its index: Dgraph keeps an undeclared
// predicate as a string, and the reads below reach comments through their doc
// or their own uuid.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/google/uuid"
)

// CommentRow is one live comment as threads read it. Attachments and
// reactions are passed on as the graph gives them, in the shape the doc
// comment list has always had.
type CommentRow struct {
	Uuid        string                   `json:"comment_uuid"`
	ThreadID    string                   `json:"comment_thread_id,omitempty"`
	Text        string                   `json:"comment_text"`
	Attachments json.RawMessage          `json:"comment_attachments,omitempty"`
	Reactions   json.RawMessage          `json:"comment_reactions,omitempty"`
	By          *dgraphStruct.DgraphUser `json:"comment_by,omitempty"`
	CreatedAt   *time.Time               `json:"comment_created_at,omitempty"`
	UpdatedAt   *time.Time               `json:"comment_updated_at,omitempty"`
	Doc         *struct {
		Uuid string `json:"doc_uuid"`
	} `json:"comment_doc,omitempty"`
}

// ErrNoComment is a comment uuid that matches no live doc comment.
var ErrNoComment = errors.New("no such doc comment")

// The fields every read takes. The attachment and reaction shapes are the doc
// comment list's (domain/Doc GetDgraphDocAllCommentList), so a thread's
// comments render as the doc's always have.
const commentFields = `
		comment_uuid
		comment_thread_id
		comment_text
		comment_attachments {
			attachment_uuid
			attachment_file_name
			attachment_obj_key
			attachment_width
			attachment_height
			attachment_size
			attachment_raw_type
			attachment_type
			attachment_duration
			attachment_created_at
		}
		comment_reactions {
			uid
			reaction_emoji_id
			reaction_added_by {
				user_uuid
				user_name
			}
		}
		comment_by {
			user_uuid
			user_name
			user_full_name
			user_email_id
			user_profile_object_key
		}
		comment_created_at
		comment_updated_at`

// DocComments is a doc's live comments, each with its thread when it has one.
// ok is false when there is no such doc.
func DocComments(ctx context.Context, docUUID string) (comments []CommentRow, ok bool, err error) {
	q := `query DocComments($id: string) {
		doc(func: eq(doc_uuid, $id)) {
			uid
			doc_comments @filter(not gt(comment_deleted_at, "1970-01-01T00:00:00Z")) {` + commentFields + `
			}
		}
	}`
	var out struct {
		Doc []struct {
			Uid      string       `json:"uid"`
			Comments []CommentRow `json:"doc_comments"`
		} `json:"doc"`
	}
	if err := dgraphInit.ReadInto(ctx, q, map[string]string{"$id": docUUID}, &out); err != nil {
		helpers.LogErrorWithContext(ctx, "models/Thread DocComments failed for doc %s: %+v", docUUID, err)
		return nil, false, err
	}
	if len(out.Doc) == 0 {
		return nil, false, nil
	}
	return out.Doc[0].Comments, true, nil
}

// DocComment is one live doc comment, with the doc it is on.
func DocComment(ctx context.Context, commentUUID string) (*CommentRow, error) {
	q := `query DocComment($id: string) {
		comment(func: eq(comment_uuid, $id)) @filter(has(comment_doc) AND not gt(comment_deleted_at, "1970-01-01T00:00:00Z")) {` + commentFields + `
			comment_doc {
				doc_uuid
			}
		}
	}`
	var out struct {
		Comment []CommentRow `json:"comment"`
	}
	if err := dgraphInit.ReadInto(ctx, q, map[string]string{"$id": commentUUID}, &out); err != nil {
		helpers.LogErrorWithContext(ctx, "models/Thread DocComment failed for %s: %+v", commentUUID, err)
		return nil, err
	}
	if len(out.Comment) == 0 || out.Comment[0].Doc == nil || out.Comment[0].Doc.Uuid == "" {
		return nil, ErrNoComment
	}
	return &out.Comment[0], nil
}

// SetCommentThread puts a doc comment in a thread. Only a comment that exists
// on a doc is changed: the write is conditional, so a uuid that matches
// nothing makes no node.
func SetCommentThread(ctx context.Context, commentUUID, threadID string) error {
	// Both are written into the request, so both must be uuids and nothing else.
	if _, err := uuid.Parse(commentUUID); err != nil {
		return fmt.Errorf("comment id %q: %w", commentUUID, err)
	}
	tid, err := uuid.Parse(threadID)
	if err != nil {
		return fmt.Errorf("thread id %q: %w", threadID, err)
	}
	req := &api.Request{
		Query: `query Tag($id: string) {
			c as var(func: eq(comment_uuid, $id)) @filter(has(comment_doc))
		}`,
		Vars: map[string]string{"$id": commentUUID},
		Mutations: []*api.Mutation{{
			Cond:      "@if(eq(len(c), 1))",
			SetNquads: []byte(fmt.Sprintf("uid(c) <comment_thread_id> %q .", tid.String())),
		}},
		CommitNow: true,
	}
	txn := dgraphInit.DgraphClient.NewTxn()
	defer func() { _ = txn.Discard(context.WithoutCancel(ctx)) }()
	if _, err := txn.Do(ctx, req); err != nil {
		helpers.LogErrorWithContext(ctx, "models/Thread SetCommentThread %s -> %s failed: %+v", commentUUID, threadID, err)
		return err
	}
	return nil
}
