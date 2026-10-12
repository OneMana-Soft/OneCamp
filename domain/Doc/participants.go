package domain

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Doc"
)

// GetDocCommentParticipants reads who takes part in a doc's comments and who
// may read it: its maker, its editors, readers and commenters, the authors of
// its live comments, and whether it is private or deleted. For telling people
// about a new comment (business/Comment). ErrNotFound for no such doc.
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

// ReadableBy is who may read a doc, decided from its sharing lists rather
// than from the counts a doc query reads for one person (business/Doc
// CanRead): the same rule, for deciding who else may be told about the doc.
// Not private: anyone; private: its maker and the people it's shared with;
// deleted: nobody. doc must be read with doc_private, doc_deleted_at, and
// doc_created_by, doc_reading_users, doc_editing_users and
// doc_commenting_users, each with user_uuid. business/Doc's
// TestReadableByAgreesWithCanRead holds the two together.
func ReadableBy(doc *dgraphStruct.DgraphDoc, userUUID string) bool {
	if doc == nil || userUUID == "" || helpers.IsSoftDeleted(doc.DeletedAt) {
		return false
	}
	if doc.IsPrivate == nil || !*doc.IsPrivate {
		return true
	}
	if doc.CreatedBy != nil && doc.CreatedBy.Uuid == userUUID {
		return true
	}
	for _, list := range [][]*dgraphStruct.DgraphUser{doc.EditingUser, doc.ReadingUser, doc.CommentingUser} {
		for _, u := range list {
			if u != nil && u.Uuid == userUUID {
				return true
			}
		}
	}
	return false
}
