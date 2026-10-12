package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Doc"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Doc"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	OpenSearchDocModels "github.com/akashc777/OneCamp/models/openSearch/Doc"
	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/google/uuid"
)

// ErrNoDocToUpdate is an update that names no doc: without a uuid the upsert
// would match nothing and report success.
var ErrNoDocToUpdate = errors.New("domain/doc: an update needs the doc's uuid")

// ErrDocDeleted is a doc in the trash (business/Doc says what that means).
var ErrDocDeleted = errors.New("the doc was deleted")

// ErrNotFound is a doc the graph has no node for (models/dgraph/Doc's, the
// one every doc read here returns), as opposed to a graph that didn't answer.
var ErrNotFound = dgraphModels.ErrNotFound

// liveDocFilter is "not deleted": a live doc carries the zero time
// (0001-01-01), so a deletion time after 1970 is a deleted one.
const liveDocFilter = `not gt(doc_deleted_at, "1970-01-01T00:00:00Z")`

// UpdateLiveDoc writes an update to a doc that exists and is not deleted, and
// to nothing else: ErrNotFound when no doc has the uuid, ErrDocDeleted when it
// is in the trash. The check and the write are one transaction, so a save from
// an editor still open on a doc deleted a moment before is refused, not
// written into the trash; and a uuid with no doc makes no node (an upsert
// naming an empty variable creates one).
func UpdateLiveDoc(ctx context.Context, dgraphDoc *dgraphStruct.DgraphDoc) error {
	return updateDocIf(ctx, dgraphDoc, liveDocFilter)
}

// UpdateStoredDoc writes an update to a doc that exists, in the trash or not:
// for repairing what a doc itself holds (business/Doc's repairdocs puts back a
// body the collaboration service destroyed, so a deleted doc is whole again if
// it is restored), never for anyone's edit. ErrNotFound when no doc has the
// uuid; a uuid with no doc makes no node.
func UpdateStoredDoc(ctx context.Context, dgraphDoc *dgraphStruct.DgraphDoc) error {
	return updateDocIf(ctx, dgraphDoc, "has(doc_uuid)")
}

// updateDocIf writes the update to the doc with its uuid when filter holds of
// it, in one conditional transaction, and says why it didn't otherwise.
func updateDocIf(ctx context.Context, dgraphDoc *dgraphStruct.DgraphDoc, filter string) error {
	req, err := docUpdateRequest(dgraphDoc, filter)
	if err != nil {
		return err
	}
	resp, err := dgraphInit.DoCommitNow(ctx, req)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/updateDocIf err: %+v", err)
		return err
	}
	var found struct {
		Found []*dgraphStruct.DgraphDoc `json:"found"`
	}
	if err := json.Unmarshal(resp.GetJson(), &found); err != nil {
		return err
	}
	switch {
	case len(found.Found) == 0:
		return ErrNotFound
	case filter == liveDocFilter && helpers.IsSoftDeleted(found.Found[0].DeletedAt):
		return ErrDocDeleted
	}
	return nil
}

// docUpdateRequest is updateDocIf's upsert: the update, set on the doc with
// its uuid only when filter holds of it, and the doc as found. The update is
// written with the doc's uuid (doc_uuid), as every update here is, so two
// that change the same doc at once conflict and the second is retried: a
// delete that commits while a save is in flight aborts the save, whose retry
// finds the doc deleted. Pure.
func docUpdateRequest(dgraphDoc *dgraphStruct.DgraphDoc, filter string) (*api.Request, error) {
	if dgraphDoc == nil || dgraphDoc.Uuid == "" {
		return nil, ErrNoDocToUpdate
	}
	if _, err := uuid.Parse(dgraphDoc.Uuid); err != nil {
		return nil, ErrNoDocToUpdate
	}
	update := *dgraphDoc
	update.Uid = "uid(doc)"
	set, err := json.Marshal(&update)
	if err != nil {
		return nil, err
	}
	return &api.Request{
		Query: `query q($id: string) {
			doc as var(func: eq(doc_uuid, $id)) @filter(` + filter + `)
			found(func: eq(doc_uuid, $id)) { uid doc_deleted_at }
		}`,
		Vars:      map[string]string{"$id": dgraphDoc.Uuid},
		Mutations: []*api.Mutation{{SetJson: set, Cond: "@if(eq(len(doc), 1))"}},
	}, nil
}

// MarkDocDeleted puts a live doc in the trash at the given time: ErrNotFound
// when no doc has the uuid, ErrDocDeleted when it is in the trash already.
// One conditional transaction, as UpdateLiveDoc: a uuid with no doc makes no
// node (the plain upsert it replaces made one for an id that matched nothing).
func MarkDocDeleted(ctx context.Context, docUUID string, at time.Time) error {
	return UpdateLiveDoc(ctx, &dgraphStruct.DgraphDoc{Uuid: docUUID, DType: []string{"Doc"}, DeletedAt: &at})
}

// GetDocDeletion reads whether a doc exists and whether it is deleted, and
// nothing else: for the paths that hold a doc's id without reading the doc
// (a guest's link). ErrNotFound when no doc has the uuid.
func GetDocDeletion(ctx context.Context, docUUID string) (*dgraphStruct.DgraphDoc, error) {
	return dgraphModels.GetDgraphDocInfoByUUID(ctx, `query DocInfo($id: string){
				docInfo(func: eq(doc_uuid, $id)) {
					uid
					doc_uuid
					doc_deleted_at
				}
			}`, map[string]string{"$id": docUUID})
}

func CreateOrUpdateDgraphDoc(ctx context.Context, dgraphDoc *dgraphStruct.DgraphDoc) (docUid string, err error) {

	var query string
	if dgraphDoc.Uid == "uid(doc)" {
		// An upsert with no uuid finds no doc and silently changes nothing.
		if dgraphDoc.Uuid == "" {
			return "", ErrNoDocToUpdate
		}
		// It's written into the query below: a uuid, and nothing else.
		if _, err := uuid.Parse(dgraphDoc.Uuid); err != nil {
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
					doc_deleted_at
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
					doc_deleted_at
					doc_created_at
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
					doc_deleted_at
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
					doc_deleted_at
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

// GetCollabDocByUUID is a doc as the collaboration service opens it: its body,
// and the Yjs state saved with the hash of the body it matches.
func GetCollabDocByUUID(ctx context.Context, docUUID string) (*dgraphStruct.DgraphDoc, error) {
	query := `query DocInfo($id: string){
				docInfo(func: eq(doc_uuid, $id)) {
					uid
					doc_uuid
					doc_title
					doc_body
					doc_yjs_state
					doc_yjs_body_hash
					doc_deleted_at
				}
			}`
	doc, err := dgraphModels.GetDgraphDocInfoByUUID(ctx, query, map[string]string{"$id": docUUID})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetCollabDocByUUID err: %+v", err)
	}
	return doc, err
}

// GetDocBodyPage reads one page of every doc's body and the hash its saved Yjs
// state was stored with, deleted docs included, in uid order (business/Doc
// RepairDocs). The states themselves are read one doc at a time.
func GetDocBodyPage(ctx context.Context, offset, first int) ([]*dgraphStruct.DgraphDoc, error) {
	query := fmt.Sprintf(`{
				docInfo(func: has(doc_uuid), first: %d, offset: %d) {
					uid
					doc_uuid
					doc_body
					doc_yjs_body_hash
				}
			}`, first, offset)
	docs, err := dgraphModels.GetDgraphDocs(ctx, query, map[string]string{})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetDocBodyPage err: %+v", err)
		return nil, err
	}
	return docs, nil
}

// SetDocCollabState saves the collaboration service's Yjs state of a live doc
// and the hash of the body it was saved with, as UpdateLiveDoc writes a save:
// ErrNotFound when no doc has the uuid (it makes no node), ErrDocDeleted when
// the doc is in the trash, and a delete that commits while it is in flight
// aborts it (docUpdateRequest). It used to write the state on whatever doc
// had the uuid, after a separate read had found it live, so a delete landing
// between the two left the state of edits made after it on the trashed doc.
func SetDocCollabState(ctx context.Context, docUUID, state, bodyHash string) error {
	return UpdateLiveDoc(ctx, collabStateUpdate(docUUID, state, bodyHash))
}

// collabStateUpdate is the update SetDocCollabState writes. Pure.
func collabStateUpdate(docUUID, state, bodyHash string) *dgraphStruct.DgraphDoc {
	return &dgraphStruct.DgraphDoc{Uuid: docUUID, YjsState: state, YjsBodyHash: bodyHash}
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
					doc_deleted_at
					doc_created_by {
						uid
						user_uuid
						user_name
						user_profile_object_key
					}
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

// GetDocAccessPage reads one page of docs, private and public, deleted ones
// included, with who made them and who they're shared with: what their AI
// search entries are filtered by (business/Doc's repair). The graph returns
// them in uid order, so offsets page through them steadily.
func GetDocAccessPage(ctx context.Context, offset, first int) ([]*dgraphStruct.DgraphDoc, error) {
	query := fmt.Sprintf(`{
				docInfo(func: has(doc_uuid), first: %d, offset: %d) {
					uid
					doc_uuid
					doc_private
					doc_created_by {
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
			}`, first, offset)

	docs, err := dgraphModels.GetDgraphDocs(ctx, query, map[string]string{})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetDocAccessPage Failed to get docs from dgraph err: %+v", err)
		return nil, err
	}
	return docs, nil
}

// docSearchFields is everything a doc's search entry is written from.
const docSearchFields = `
					uid
					doc_uuid
					doc_title
					doc_body
					doc_snippet
					doc_private
					doc_created_at
					doc_updated_at
					doc_deleted_at
					doc_created_by {
						user_uuid
						user_name
						user_full_name
						user_email_id
						user_profile_object_key
					}
					doc_reading_users {
						user_uuid
					}
					doc_editing_users {
						user_uuid
					}
					doc_commenting_users {
						user_uuid
					}`

// GetDocSearchEntry reads a doc as its search entry is written: its text, who
// made it, who it's shared with, and whether it is deleted. ErrNotFound for
// no such doc.
func GetDocSearchEntry(ctx context.Context, docUUID string) (*dgraphStruct.DgraphDoc, error) {
	return dgraphModels.GetDgraphDocInfoByUUID(ctx, `query DocInfo($id: string){
				docInfo(func: eq(doc_uuid, $id)) {`+docSearchFields+`
				}
			}`, map[string]string{"$id": docUUID})
}

// GetDocSearchEntryPage reads one page of every doc, deleted ones included,
// as GetDocSearchEntry reads one: for rewriting every entry (business/Doc's
// search repair). The graph returns them in uid order, so offsets page
// through them steadily.
func GetDocSearchEntryPage(ctx context.Context, offset, first int) ([]*dgraphStruct.DgraphDoc, error) {
	docs, err := dgraphModels.GetDgraphDocs(ctx, fmt.Sprintf(`{
				docInfo(func: has(doc_uuid), first: %d, offset: %d) {`+docSearchFields+`
				}
			}`, first, offset), map[string]string{})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetDocSearchEntryPage err: %+v", err)
	}
	return docs, err
}

// GetDocUUIDPage reads the uuids of one page of every doc, the page
// GetDocSearchEntryPage reads at the same offset: for naming a doc whose
// entry is too large to read (business/Doc's search repair).
func GetDocUUIDPage(ctx context.Context, offset, first int) ([]string, error) {
	docs, err := dgraphModels.GetDgraphDocs(ctx, fmt.Sprintf(`{
				docInfo(func: has(doc_uuid), first: %d, offset: %d) {
					doc_uuid
				}
			}`, first, offset), map[string]string{})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetDocUUIDPage err: %+v", err)
		return nil, err
	}
	ids := make([]string, len(docs))
	for i, d := range docs {
		if d != nil {
			ids[i] = d.Uuid
		}
	}
	return ids, nil
}

// GetDocsReadAccessFor reads, for one person (by uuid), what CanRead decides
// each of the named docs by: its privacy, its deletion time, its creator, and
// whether it is shared with them. One query for the lot; ids that aren't
// uuids are left out, and so is any doc that doesn't exist.
func GetDocsReadAccessFor(ctx context.Context, docUUIDs []string, userUUID string) ([]*dgraphStruct.DgraphDoc, error) {
	quoted := make([]string, 0, len(docUUIDs))
	for _, id := range docUUIDs {
		if parsed, err := uuid.Parse(id); err == nil {
			quoted = append(quoted, `"`+parsed.String()+`"`)
		}
	}
	if len(quoted) == 0 {
		return nil, nil
	}
	query := `query Readable($user: string) {
				docInfo(func: eq(doc_uuid, [` + strings.Join(quoted, ", ") + `])) {
					doc_uuid
					doc_private
					doc_deleted_at
					doc_created_by {
						user_uuid
					}
					doc_read_access: count(doc_reading_users @filter(eq(user_uuid, $user)))
					doc_edit_access: count(doc_editing_users @filter(eq(user_uuid, $user)))
					doc_comment_access: count(doc_commenting_users @filter(eq(user_uuid, $user)))
				}
			}`
	docs, err := dgraphModels.GetDgraphDocs(ctx, query, map[string]string{"$user": userUUID})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetDocsReadAccessFor err: %+v", err)
	}
	return docs, err
}

// CouldRead is who may read a doc, its deletion set aside: anyone signed in,
// for a doc that isn't private; for a private one, its creator and the people
// it's shared with. doc is read for the person (GetDocsReadAccessFor reads
// their counts). The rule business/Doc's CanRead and CouldRead decide by, here
// so the packages business/Doc depends on can ask it too (business/Archive's
// restore list). Pure.
func CouldRead(doc *dgraphStruct.DgraphDoc, userUUID string) bool {
	if doc == nil {
		return false
	}
	if doc.IsPrivate == nil || !*doc.IsPrivate {
		return true
	}
	return doc.HasEditAccess > 0 || doc.HasReadAccess > 0 || doc.HasCommentAccess > 0 ||
		(doc.CreatedBy != nil && doc.CreatedBy.Uuid == userUUID)
}

// DocsCouldReadFor answers, of the named docs, which the person (by uuid)
// could read, their deletion set aside (CouldRead), keyed by lower-case uuid.
// A doc that doesn't exist is absent, and nobody (an empty id) reads any. An
// error is the graph not answering, and says nothing about any doc.
func DocsCouldReadFor(ctx context.Context, docUUIDs []string, userUUID string) (map[string]bool, error) {
	out := make(map[string]bool, len(docUUIDs))
	if userUUID == "" || len(docUUIDs) == 0 {
		return out, nil
	}
	docs, err := GetDocsReadAccessFor(ctx, docUUIDs, userUUID)
	if err != nil {
		return nil, err
	}
	for _, d := range docs {
		if d != nil && d.Uuid != "" {
			out[strings.ToLower(d.Uuid)] = CouldRead(d, userUUID)
		}
	}
	return out, nil
}

// GetDocsDeletion reads the named docs' deletion and archive marks, title and
// maker: what tells a doc its owner deleted from one the workspace's retention
// archived, before a restore clears both. One query for the lot; ids that
// aren't uuids are left out, and so is any doc that doesn't exist.
func GetDocsDeletion(ctx context.Context, docUUIDs []string) ([]*dgraphStruct.DgraphDoc, error) {
	quoted := make([]string, 0, len(docUUIDs))
	for _, id := range docUUIDs {
		if parsed, err := uuid.Parse(id); err == nil {
			quoted = append(quoted, `"`+parsed.String()+`"`)
		}
	}
	if len(quoted) == 0 {
		return nil, nil
	}
	docs, err := dgraphModels.GetDgraphDocs(ctx, `{
				docInfo(func: eq(doc_uuid, [`+strings.Join(quoted, ", ")+`])) {
					doc_uuid
					doc_title
					doc_deleted_at
					doc_archived_at
					doc_created_by {
						user_uuid
						user_name
						user_full_name
						user_email_id
					}
				}
			}`, map[string]string{})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetDocsDeletion err: %+v", err)
	}
	return docs, err
}

// IndexDocEntry writes a doc's whole search entry.
func IndexDocEntry(ctx context.Context, entry *openSearchStruct.OpenSearchDocEntry) error {
	return OpenSearchDocModels.IndexDocEntry(ctx, entry)
}

// BulkIndexDocEntries writes many docs' whole search entries.
func BulkIndexDocEntries(ctx context.Context, entries []*openSearchStruct.OpenSearchDocEntry) (int, error) {
	return OpenSearchDocModels.BulkIndexDocEntries(ctx, entries)
}

// inactiveDocFilter holds of a doc nobody has changed or brought back since
// cutoff, a DQL value (a variable, or a quoted time): last edited before it,
// or never edited and made before it, and not restored since. A doc made long
// ago and edited yesterday is in use, and was archived when this read the
// creation time alone. A restore leaves the last edit as it was, so a doc an
// admin restored, or a run's undo brought back, was archived again by the
// next hourly run; it now waits out the retention period from its restore.
func inactiveDocFilter(cutoff string) string {
	return fmt.Sprintf(`(lt(doc_updated_at, %[1]s) OR (NOT has(doc_updated_at) AND lt(doc_created_at, %[1]s))) AND NOT ge(doc_restored_at, %[1]s)`, cutoff)
}

// GetDocsInactiveSince returns up to limit live docs nobody has changed or
// restored since the cutoff (inactiveDocFilter).
func GetDocsInactiveSince(ctx context.Context, cutoff time.Time, limit int) ([]string, error) {
	query := fmt.Sprintf(`query Docs($cutoff: string){
		live as var(func: has(doc_uuid)) @filter(%s)
		docInfo(func: uid(live), first: %d) @filter(%s) {
			doc_uuid
		}
	}`, liveDocFilter, limit, inactiveDocFilter("$cutoff"))
	docs, err := dgraphModels.GetDgraphDocs(ctx, query, map[string]string{"$cutoff": cutoff.UTC().Format(time.RFC3339)})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetDocsInactiveSince failed: %v", err)
		return nil, err
	}
	ids := make([]string, 0, len(docs))
	for _, d := range docs {
		if d.Uuid != "" {
			ids = append(ids, d.Uuid)
		}
	}
	return ids, nil
}

// docBatch bounds one archive or restore request: a mutation per doc, each
// with its own condition.
const docBatch = 200

// moveDocs runs one upsert per batch (docMoveRequest) and answers the docs it
// changed.
func moveDocs(ctx context.Context, docUUIDs []string, filter string, mutation func(v, id string) *api.Mutation) ([]string, error) {
	var moved []string
	for start := 0; start < len(docUUIDs); start += docBatch {
		req := docMoveRequest(docUUIDs[start:min(start+docBatch, len(docUUIDs))], filter, mutation)
		if req == nil {
			continue
		}
		resp, err := dgraphInit.DoCommitNow(ctx, req)
		if err != nil {
			return moved, err
		}
		var got struct {
			Moved []*dgraphStruct.DgraphDoc `json:"moved"`
		}
		if err := json.Unmarshal(resp.GetJson(), &got); err != nil {
			return moved, err
		}
		for _, d := range got.Moved {
			moved = append(moved, d.Uuid)
		}
	}
	return moved, nil
}

// docMoveRequest is one batch's upsert: a variable per doc, matching it only
// if filter holds; a mutation per doc that runs only if its own variable
// matched one doc, so a uuid with no doc makes no node; and a "moved" block
// naming the docs the mutations change. Every doc used to share one variable
// the query never defined ("query { uid(doc) uid(doc) }"), which is not valid
// DQL, so every archive and restore of docs failed. mutation is given each
// doc's variable and uuid. Ids that aren't uuids are left out; nil when none
// is left. Pure.
func docMoveRequest(docUUIDs []string, filter string, mutation func(v, id string) *api.Mutation) *api.Request {
	var q strings.Builder
	q.WriteString("query {\n")
	var vars []string
	var muts []*api.Mutation
	for i, id := range docUUIDs {
		parsed, err := uuid.Parse(id)
		if err != nil {
			continue
		}
		v := fmt.Sprintf("d%d", i)
		fmt.Fprintf(&q, "  %s as var(func: eq(doc_uuid, %q)) @filter(%s)\n", v, parsed.String(), filter)
		m := mutation(v, parsed.String())
		m.Cond = fmt.Sprintf("@if(eq(len(%s), 1))", v)
		muts = append(muts, m)
		vars = append(vars, v)
	}
	if len(vars) == 0 {
		return nil
	}
	fmt.Fprintf(&q, "  moved(func: uid(%s)) { doc_uuid }\n}", strings.Join(vars, ", "))
	return &api.Request{Query: q.String(), Mutations: muts}
}

// BulkArchiveDocs archives live docs nobody has changed or restored since
// cutoff (docArchiveFilter): deleted, and marked as archived by the
// workspace's retention (doc_archived_at), which is what tells them from docs
// their owners deleted. Answers the docs it archived; one already deleted, in
// use again, or not there is left alone.
func BulkArchiveDocs(ctx context.Context, docUUIDs []string, cutoff time.Time) ([]string, error) {
	archived, err := moveDocs(ctx, docUUIDs, docArchiveFilter(cutoff), docArchiveMutation(time.Now()))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/BulkArchiveDocs failed: %v", err)
	}
	return archived, err
}

// docArchiveFilter holds of a doc a run may archive: live, and nobody has
// changed or restored it since cutoff. The run picks docs by that first
// (GetDocsInactiveSince), and the archive checks it again in its own write,
// so a doc someone edited in between is left alone, not archived in use.
// Pure.
func docArchiveFilter(cutoff time.Time) string {
	return liveDocFilter + " AND " + inactiveDocFilter(strconv.Quote(cutoff.UTC().Format(time.RFC3339)))
}

// docArchiveMutation archives a doc at the given time, and writes its uuid
// again. Every save writes the uuid (UpdateLiveDoc), and Dgraph aborts the
// second of two transactions only when both write the same thing: with it, a
// save and the archive of one doc can't both land, and whichever runs again
// sees the other (a save then finds the doc archived and is refused). The
// archive wrote only its two marks, so a save that had read the doc live
// still went into it once archived. Pure.
func docArchiveMutation(at time.Time) func(v, id string) *api.Mutation {
	stamp, _ := json.Marshal(at.UTC())
	return func(v, id string) *api.Mutation {
		return &api.Mutation{SetJson: []byte(fmt.Sprintf(`{"uid": "uid(%s)", "doc_uuid": %q, "doc_deleted_at": %s, "doc_archived_at": %s}`, v, id, stamp, stamp))}
	}
}

// BulkRestoreDocs brings deleted docs back: the zero deletion time a live doc
// carries, no archive mark, and when it was restored (doc_restored_at, which
// keeps the retention from archiving it again before it has gone unused for
// the whole period since). archivedOnly restores only docs the retention
// archived (undoing an archive run must not bring back a doc its owner
// deleted since); otherwise any deleted doc, as an admin restoring from the
// trash does. Answers the docs it restored; a live one is left alone.
func BulkRestoreDocs(ctx context.Context, docUUIDs []string, archivedOnly bool) ([]string, error) {
	filter := `gt(doc_deleted_at, "1970-01-01T00:00:00Z")`
	if archivedOnly {
		filter += ` AND gt(doc_archived_at, "1970-01-01T00:00:00Z")`
	}
	at, _ := json.Marshal(time.Now().UTC())
	restored, err := moveDocs(ctx, docUUIDs, filter, func(v, _ string) *api.Mutation {
		return &api.Mutation{
			SetJson:   []byte(fmt.Sprintf(`{"uid": "uid(%s)", "doc_deleted_at": "0001-01-01T00:00:00Z", "doc_restored_at": %s}`, v, at)),
			DelNquads: []byte(fmt.Sprintf("uid(%s) <doc_archived_at> * .", v)),
		}
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/BulkRestoreDocs failed: %v", err)
	}
	return restored, err
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

// GetRecentlyArchivedDocs returns docs deleted after cutoff, archived or
// deleted by their owners, newest first and paginated, with what tells them
// apart (doc_archived_at) and whose they are.
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
				doc_archived_at
				doc_created_by {
					user_uuid
					user_name
					user_full_name
					user_email_id
				}
			}
	}`

	docs, err := dgraphModels.GetDgraphDocsWithCount(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetRecentlyArchivedDocs failed: %v", err)
		return nil, 0, err
	}
	return docs.Docs, int64(docs.Count), nil
}
