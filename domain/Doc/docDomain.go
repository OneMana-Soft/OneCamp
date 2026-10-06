package domain

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Doc"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Doc"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	OpenSearchDocModels "github.com/akashc777/OneCamp/models/openSearch/Doc"
)

// ErrNoDocToUpdate is an update that names no doc: without a uuid the upsert
// would match nothing and report success.
var ErrNoDocToUpdate = errors.New("domain/doc: an update needs the doc's uuid")

func CreateOrUpdateDgraphDoc(ctx context.Context, dgraphDoc *dgraphStruct.DgraphDoc) (docUid string, err error) {

	var query string
	if dgraphDoc.Uid == "uid(doc)" {
		// An upsert with no uuid finds no doc and silently changes nothing.
		if dgraphDoc.Uuid == "" {
			return "", ErrNoDocToUpdate
		}
		query = fmt.Sprintf(`query {
									  doc as var(func: eq(doc_uuid, "%s"))
								  }`, dgraphDoc.Uuid)
	}

	docUid, err = dgraphModels.CreateOrUpdateDoc(ctx, query, dgraphDoc, "")

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateOrUpdateDgraphDoc Failed to create/update doc err: %+v",
			err)
		return
	}
	return
}

func GetDgraphDocByUUIDOnlyEditingInfo(ctx context.Context, docUUID string, userUID string) (dgraphDoc *dgraphStruct.DgraphDoc, err error) {
	variables := make(map[string]string)
	variables["$id"] = docUUID
	variables["$userId"] = userUID

	query := `query DocInfo($id: string, $userId: string){
				docInfo(func: eq(doc_uuid, $id)) {
					uid
					doc_uuid
					doc_edit_access: count(doc_editing_users @filter(uid($userId)))
					doc_created_by @filter(uid($userId)) {
						uid
						user_uuid
					}
				}
			}`

	dgraphDoc, err = dgraphModels.GetDgraphDocInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphDocByUUIDOnlyEditingInfo Failed to get doc from dgraph err: %+v",
			err,
		)
		return
	}

	return

}

func GetBasicDgraphDocByUUID(ctx context.Context, docUUID string, userUID string) (dgraphDoc *dgraphStruct.DgraphDoc, err error) {
	variables := make(map[string]string)
	variables["$id"] = docUUID
	variables["$userId"] = userUID

	query := `query DocInfo($id: string, $userId: string){
				docInfo(func: eq(doc_uuid, $id)) {
					uid
					doc_uuid
					doc_read_access: count(doc_reading_users @filter(uid($userId)))
					doc_edit_access: count(doc_editing_users @filter(uid($userId)))
					doc_comment_access: count(doc_commenting_users @filter(uid($userId)))
					doc_private
					doc_public_comment
					doc_title
					doc_created_by {
						uid
						user_uuid
					}
					doc_reading_users {
						user_uuid
					}
					doc_editing_users {
						user_uuid
					}
					doc_commenting_users {
						user_uuid
					}
				}
			}`

	dgraphDoc, err = dgraphModels.GetDgraphDocInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetBasicDgraphDocByUUID Failed to get doc from dgraph err: %+v",
			err,
		)
		return
	}

	return

}

func GetDgraphDocByUUID(ctx context.Context, docUUID string, userUID string) (dgraphDoc *dgraphStruct.DgraphDoc, err error) {
	variables := make(map[string]string)
	variables["$id"] = docUUID
	variables["$userId"] = userUID

	query := `query DocInfo($id: string, $userId: string){
				docInfo(func: eq(doc_uuid, $id)) {
					uid
					doc_uuid
					doc_title
					doc_read_access: count(doc_reading_users @filter(uid($userId)))
					doc_edit_access: count(doc_editing_users @filter(uid($userId)))
					doc_comment_access: count(doc_commenting_users @filter(uid($userId)))
					doc_comment_count: count(doc_comments @filter(not gt(comment_deleted_at, "1970-01-01T00:00:00Z")))
					doc_private
					doc_public_comment
					doc_body
					doc_created_at
					doc_created_by {
						uid
						user_uuid
						user_name
						user_profile_object_key
					}
				}
			}`

	dgraphDoc, err = dgraphModels.GetDgraphDocInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphDocByUUID Failed to get doc from dgraph err: %+v",
			err,
		)
		return
	}

	return

}

func GetSystemDocByUUID(ctx context.Context, docUUID string) (dgraphDoc *dgraphStruct.DgraphDoc, err error) {
	variables := make(map[string]string)
	variables["$id"] = docUUID

	query := `query DocInfo($id: string){
				docInfo(func: eq(doc_uuid, $id)) {
					uid
					doc_uuid
					doc_title
					doc_private
					doc_public_comment
					doc_body
					doc_created_at
					doc_created_by {
						uid
						user_uuid
						user_name
						user_profile_object_key
					}
				}
			}`

	dgraphDoc, err = dgraphModels.GetDgraphDocInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetSystemDocByUUID Failed to get doc from dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphDocAllCommentList(ctx context.Context, docUUID string, userUID string) (dgraphDoc *dgraphStruct.DgraphDoc, err error) {
	variables := make(map[string]string)
	variables["$id"] = docUUID
	variables["$userId"] = userUID

	query := `query DocInfo($id: string, $userId: string){
				docInfo(func: eq(doc_uuid, $id)) {
					uid
					doc_comment_access: count(doc_commenting_users @filter(uid($userId)))
					doc_read_access: count(doc_reading_users @filter(uid($userId)))
					doc_edit_access: count(doc_editing_users @filter(uid($userId)))
					doc_private
					doc_public_comment
					doc_created_by {
						uid
						user_uuid
						user_name
						user_profile_object_key
					}s
					doc_comments @filter(not gt(comment_deleted_at, "1970-01-01T00:00:00Z")) {
						comment_uuid
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
							user_profile_object_key
							user_name
						}
						comment_created_at
					}
				}
			}`

	dgraphDoc, err = dgraphModels.GetDgraphDocInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphDocAllCommentList Failed to get doc from dgraph err: %+v",
			err,
		)
		return
	}

	return

}

func GetDgraphPublicDocFromDgraph(ctx context.Context, pageIndex int, pageSize int) (dgraphDocList *dgraphStruct.DgraphDocList, err error) {

	variables := make(map[string]string)

	offset := pageIndex * pageSize
	firstVal := strconv.Itoa(pageSize)
	offsetVal := strconv.Itoa(offset)

	// Corrected to use doc_created_at for sorting.
	transcriptParams := fmt.Sprintf(", orderdesc: doc_created_at, first: %v, offset: %v", firstVal, offsetVal)

	query := fmt.Sprintf(`query DocInfo(){
				var(func: eq(doc_private, false)) @filter(not gt(doc_deleted_at, "1970-01-01T00:00:00Z")) {
					public_doc_count as count(uid)
				}
				
				doc_count(func: uid(public_doc_count)) {
					count: val(public_doc_count)
				}

				docInfo(func: eq(doc_private, false)%s) @filter(not gt(doc_deleted_at, "1970-01-01T00:00:00Z")) {
					doc_uuid
					doc_title
					doc_snippet
					doc_created_at
					doc_created_by {
						uid
						user_uuid
						user_name
						user_profile_object_key
					}
					
				}
			}`, transcriptParams)

	dgraphDocList, err = dgraphModels.GetDgraphDocsWithCount(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphPublicDocFromDgraph Failed to get doc in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphPrivateDocFromDgraph(ctx context.Context, userUID string, pageIndex int, pageSize int) (dgraphDocList *dgraphStruct.DgraphDocList, err error) {

	variables := make(map[string]string)

	offset := pageIndex * pageSize
	firstVal := strconv.Itoa(pageSize)
	offsetVal := strconv.Itoa(offset)

	// Corrected to use doc_created_at for sorting.
	transcriptParams := fmt.Sprintf(", orderdesc: doc_created_at, first: %v, offset: %v", firstVal, offsetVal)

	variables["$userId"] = userUID

	query := fmt.Sprintf(`query DocInfo($userId: string){
				var(func: uid($userId)) {
					user_private_docs as ~doc_created_by @filter(eq(doc_private, true) AND not gt(doc_deleted_at, "1970-01-01T00:00:00Z"))
					private_doc_count_val as count(~doc_created_by) @filter(eq(doc_private, true) AND not gt(doc_deleted_at, "1970-01-01T00:00:00Z"))
				}

				doc_count(func: uid($userId)) {
					count: val(private_doc_count_val)
				}

				docInfo(func: uid(user_private_docs)%s) {
					doc_uuid
					doc_title
					doc_snippet
					doc_created_at
					doc_created_by {
						uid
						user_uuid
						user_name
						user_profile_object_key
					}
					
				}
			}`, transcriptParams)

	dgraphDocList, err = dgraphModels.GetDgraphDocsWithCount(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphPrivateDocFromDgraph Failed to get doc in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphPublicDocFromDgraphWithSearchText(ctx context.Context, inputDocName *adapter.InputSearchDocName) (dgraphDocList *dgraphStruct.DgraphDocList, err error) {

	variables := make(map[string]string)

	offset := inputDocName.PageIndex * inputDocName.PageSize
	firstVal := strconv.Itoa(inputDocName.PageSize)
	offsetVal := strconv.Itoa(offset)

	// Corrected to use doc_created_at for sorting.
	transcriptParams := fmt.Sprintf(", orderdesc: doc_created_at, first: %v, offset: %v", firstVal, offsetVal)

	query := fmt.Sprintf(`query DocInfo(){
				var(func: eq(doc_private, false)) @filter(regexp(doc_title, /%s/i) AND not gt(doc_deleted_at, "1970-01-01T00:00:00Z")) {
					public_doc_count as count(uid)
				}
				
				doc_count(func: uid(public_doc_count)) {
					count: val(public_doc_count)
				}

				docInfo(func: eq(doc_private, false)%s) @filter(regexp(doc_title, /%s/i) AND not gt(doc_deleted_at, "1970-01-01T00:00:00Z")) {
					doc_uuid
					doc_title
					doc_snippet
					doc_created_at
					doc_created_by {
						uid
						user_uuid
						user_name
						user_profile_object_key
					}
					
				}
			}`, inputDocName.SearchText, transcriptParams, inputDocName.SearchText)

	dgraphDocList, err = dgraphModels.GetDgraphDocsWithCount(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphPublicDocFromDgraphWithSearchText Failed to get doc in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphPrivateDocFromDgraphWithSearchText(ctx context.Context, userUID string, inputDocName *adapter.InputSearchDocName) (dgraphDocList *dgraphStruct.DgraphDocList, err error) {

	variables := make(map[string]string)

	offset := inputDocName.PageIndex * inputDocName.PageSize
	firstVal := strconv.Itoa(inputDocName.PageSize)
	offsetVal := strconv.Itoa(offset)

	// Corrected to use doc_created_at for sorting. Defaulting to descending order (latest first).
	transcriptParams := fmt.Sprintf(", orderdesc: doc_created_at, first: %v, offset: %v", firstVal, offsetVal)

	variables["$userId"] = userUID

	query := fmt.Sprintf(`query DocInfo($userId: string){
				var(func: uid($userId)) {
					user_private_docs as ~doc_created_by @filter(eq(doc_private, true) AND regexp(doc_title, /%s/i) AND not gt(doc_deleted_at, "1970-01-01T00:00:00Z"))
					private_doc_count_val as count(~doc_created_by) @filter(eq(doc_private, true) AND regexp(doc_title, /%s/i) AND not gt(doc_deleted_at, "1970-01-01T00:00:00Z"))
				}

				doc_count(func: uid($userId)) {
					count: val(private_doc_count_val)
				}

				docInfo(func: uid(user_private_docs)%s) {

					doc_uuid

					doc_title
					doc_snippet
					doc_created_at
					doc_created_by {
						uid
						user_uuid
						user_name
						user_profile_object_key
					}
					
				}
			}`, inputDocName.SearchText, inputDocName.SearchText, transcriptParams)

	dgraphDocList, err = dgraphModels.GetDgraphDocsWithCount(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphPrivateDocFromDgraphWithSearchText Failed to get doc in dgraph err: %+v",
			err,
		)
		return
	}

	return
}
func UpdateDocPermissions(ctx context.Context, input *adapter.InputUpdateDocPermissions) (err error) {
	return dgraphModels.UpdateDocPermissions(ctx, input)
}

func GetDocPermissions(ctx context.Context, docUUID string) (dgraphDoc *dgraphStruct.DgraphDoc, err error) {
	variables := make(map[string]string)
	variables["$id"] = docUUID

	query := `query DocPermissions($id: string){
				docInfo(func: eq(doc_uuid, $id)) {
					uid
					doc_uuid
					doc_private
					doc_deleted_at
					doc_editing_users {
						uid
						user_uuid
						user_name
						user_profile_object_key
					}
					doc_reading_users {
						uid
						user_uuid
						user_name
						user_profile_object_key
					}
					doc_commenting_users {
						uid
						user_uuid
						user_name
						user_profile_object_key
					}
					doc_created_by {
						uid
						user_uuid
						user_name
						user_profile_object_key
					}
				}
			}`

	dgraphDoc, err = dgraphModels.GetDgraphDocInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDocPermissions Failed to get doc in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func CreateDocInOpenSearch(openSearchDoc *openSearchStruct.OpenSearchDoc) (err error) {
	ctx := context.Background()
	err = OpenSearchDocModels.CreateDocInOpenSearch(ctx, openSearchDoc)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateDocInOpenSearch Failed to insert to openSearch err: %+v",
			err,
		)
		return
	}

	return
}

func UpdateDocInOpenSearch(openSearchDoc *openSearchStruct.OpenSearchDoc) (err error) {
	ctx := context.Background()
	err = OpenSearchDocModels.UpdateDocInOpenSearch(ctx, openSearchDoc)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateDocInOpenSearch Failed to update in openSearch err: %+v",
			err,
		)
		return
	}

	return
}

func DeleteDocInOpenSearch(docUUID string, deletedAt int64) (err error) {
	ctx := context.Background()
	err = OpenSearchDocModels.DeleteDocInOpenSearch(ctx, docUUID, deletedAt)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/DeleteDocInOpenSearch Failed to mark as deleted in openSearch err: %+v",
			err,
		)
		return
	}

	return
}

// GetDocUuidsOlderThan returns UUIDs of active docs created before the cutoff.
func GetDocUuidsOlderThan(ctx context.Context, cutoff time.Time) ([]string, error) {
	cutoffStr := cutoff.Format(time.RFC3339)
	variables := make(map[string]string)
	variables["$cutoff"] = cutoffStr
	query := `query Docs($cutoff: string){
		docs(func: has(doc_uuid)) @filter(lt(doc_created_at, $cutoff) AND not gt(doc_deleted_at, "1970-01-01T00:00:00Z")) {
			doc_uuid
		}
	}`

	docs, err := dgraphModels.GetDgraphDocs(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetDocUuidsOlderThan failed: %v", err)
		return nil, err
	}

	var ids []string
	for _, d := range docs {
		if d.Uuid != "" {
			ids = append(ids, d.Uuid)
		}
	}
	return ids, nil
}

// BulkArchiveDocs soft-deletes multiple docs in a single Dgraph mutation.
func BulkArchiveDocs(ctx context.Context, docUUIDs []string) error {
	now := time.Now()
	docs := make([]*dgraphStruct.DgraphDoc, len(docUUIDs))
	for i, uuidStr := range docUUIDs {
		docs[i] = &dgraphStruct.DgraphDoc{
			DType:     []string{"Doc"},
			Uid:       "uid(doc)",
			Uuid:      uuidStr,
			DeletedAt: &now,
		}
	}

	var uidPlaceholders []string
	for range docUUIDs {
		uidPlaceholders = append(uidPlaceholders, "uid(doc)")
	}
	query := fmt.Sprintf(`query { %s }`, strings.Join(uidPlaceholders, " "))

	err := dgraphModels.BulkSoftDeleteDgraphDocs(ctx, docs, query)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/BulkArchiveDocs failed: %v", err)
		return err
	}
	return nil
}

// BulkRestoreDocs clears doc_deleted_at on multiple docs in a single Dgraph mutation.
func BulkRestoreDocs(ctx context.Context, docUUIDs []string) error {
	zeroTime := time.Time{}
	docs := make([]*dgraphStruct.DgraphDoc, len(docUUIDs))
	for i, uuidStr := range docUUIDs {
		docs[i] = &dgraphStruct.DgraphDoc{
			DType:     []string{"Doc"},
			Uid:       "uid(doc)",
			Uuid:      uuidStr,
			DeletedAt: &zeroTime,
		}
	}

	var uidPlaceholders []string
	for range docUUIDs {
		uidPlaceholders = append(uidPlaceholders, "uid(doc)")
	}
	query := fmt.Sprintf(`query { %s }`, strings.Join(uidPlaceholders, " "))

	return dgraphModels.BulkSoftDeleteDgraphDocs(ctx, docs, query)
}

// CountRecentlyArchivedDocs returns the number of docs soft-deleted after cutoff.
func CountRecentlyArchivedDocs(ctx context.Context, cutoffRFC3339 string) (int64, error) {
	variables := make(map[string]string)
	variables["$cutoff"] = cutoffRFC3339
	query := `query Docs($cutoff: string){
		doc_count(func: has(doc_uuid)) @filter(gt(doc_deleted_at, $cutoff)) { count(uid) }
	}`

	docs, err := dgraphModels.GetDgraphDocsWithCount(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/CountRecentlyArchivedDocs failed: %v", err)
		return 0, err
	}
	return int64(docs.Count), nil
}

// GetRecentlyArchivedDocs returns docs soft-deleted after cutoff, paginated.
func GetRecentlyArchivedDocs(ctx context.Context, cutoffRFC3339 string, first, offset int) ([]*dgraphStruct.DgraphDoc, int64, error) {
	variables := make(map[string]string)
	variables["$cutoff"] = cutoffRFC3339
	variables["$first"] = strconv.Itoa(first)
	variables["$offset"] = strconv.Itoa(offset)
	query := `query Docs($cutoff: string, $first: int, $offset: int){
		doc_count(func: has(doc_uuid)) @filter(gt(doc_deleted_at, $cutoff)) { count(uid) }
		docInfo(func: has(doc_uuid), first: $first, offset: $offset, orderdesc: doc_deleted_at)
			@filter(gt(doc_deleted_at, $cutoff)) {
				uid
				doc_uuid
				doc_title
				doc_deleted_at
			}
	}`

	docs, err := dgraphModels.GetDgraphDocsWithCount(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetRecentlyArchivedDocs failed: %v", err)
		return nil, 0, err
	}
	return docs.Docs, int64(docs.Count), nil
}
