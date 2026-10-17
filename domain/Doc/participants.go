package domain

import (
	"context"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Doc"
)

// GetDocCommentParticipants reads who takes part in a doc's comments: its
// owner (doc_owner, after a hand-over; OwnerOf), its maker, its full-access
// people, editors, readers and commenters, the authors of its live comments,
// and whether it is private or deleted. For telling people about a
// new comment (business/Comment) and the doc's other events (business/Follow);
// who of them may read it now is the Access Resolver's to say (business/Doc
// Readers). ErrNotFound for no such doc.
func GetDocCommentParticipants(ctx context.Context, docUUID string) (*dgraphStruct.DgraphDoc, error) {
	return dgraphModels.GetDgraphDocInfoByUUID(ctx, `query DocInfo($id: string){
				docInfo(func: eq(doc_uuid, $id)) {
					uid
					doc_uuid
					doc_title
					doc_private
					doc_deleted_at
					doc_created_by {
						user_uuid
					}
					doc_owner {
						user_uuid
					}
					doc_full_access_users {
						user_uuid
					}
					doc_editing_users {
						user_uuid
					}
					doc_reading_users {
						user_uuid
					}
					doc_commenting_users {
						user_uuid
					}
					doc_comments @filter(`+`not gt(comment_deleted_at, "1970-01-01T00:00:00Z")`+`) {
						comment_by {
							user_uuid
						}
					}
				}
			}`, map[string]string{"$id": docUUID})
}
