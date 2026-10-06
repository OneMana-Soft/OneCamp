package businness

import (
	"context"
	"errors"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Doc"
	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	businessComment "github.com/akashc777/OneCamp/business/Comment"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	domain "github.com/akashc777/OneCamp/domain/Doc"
	globalSearchDomain "github.com/akashc777/OneCamp/domain/GlobalSearch"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	openSearchGlobalSearchModels "github.com/akashc777/OneCamp/models/openSearch/GlobalSearch"

	model "github.com/akashc777/OneCamp/models/postgres/User"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// docSnippetRunes bounds the stored doc preview. Counted in characters, not bytes,
// so the limit means the same thing for every language.
const docSnippetRunes = 1000

// docPreviewRunes bounds the doc body returned in LIST responses, so a listing does
// not ship whole documents. Same value and same reasoning as docSnippetRunes, named
// separately because they bound different things and could diverge.
const docPreviewRunes = 1000

func CreateDoc(ctx context.Context, createdByUser *dgraphStruct.DgraphUser, inputDoc *adapter.InputCreateDoc) (dgraphDoc *dgraphStruct.DgraphDoc, err error) {

	newDocUUID := uuid.New()

	currentTime := time.Now()
	zeroUnixTime := time.Time{}

	boolTrue := true
	boolFalse := false

	isPrivate := boolFalse
	if inputDoc.DocPrivate {
		isPrivate = boolTrue
	}

	dgraphDoc = &dgraphStruct.DgraphDoc{
		DType: []string{"Doc"},
		// Uid is omitted to let Dgraph create a new node
		Uuid:  newDocUUID.String(),
		Title: inputDoc.DocTitle,
		CreatedBy: &dgraphStruct.DgraphUser{
			Uid: createdByUser.Uid,
		},
		IsPrivate:     &isPrivate,
		PublicComment: &boolFalse,
		EditingUser: []*dgraphStruct.DgraphUser{{
			Uid: createdByUser.Uid,
		}},
		CreatedAt: &currentTime,
		DeletedAt: &zeroUnixTime,
	}

	_, err = domain.CreateOrUpdateDgraphDoc(ctx, dgraphDoc)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/CreateDoc Failed to create doc in dgraph err: %+v", err)
		return
	}

	go domain.CreateDocInOpenSearch(&openSearchStruct.OpenSearchDoc{
		Uuid:                 dgraphDoc.Uuid,
		DocTitle:             dgraphDoc.Title,
		DocBody:              "",
		DocCreatedByUserUuid: createdByUser.Uuid,
		DocCreatedByFullName: createdByUser.UserFullName,
		DocCreatedByProfile:  createdByUser.ProfileKey,
		DocPrivate:           *dgraphDoc.IsPrivate,
		DocEditingUsers:      []string{createdByUser.Uuid},
		DocCreatedAt:         currentTime.Unix(),
		DocUpdatedAt:         currentTime.Unix(),
		DocDeletedAt:         nil,
	})

	// embed for AI Second Brain (async)
	/* AI call omitted in v1 */

	return
}

func DeleteDoc(ctx context.Context, docUUID string) (err error) {

	currentTime := time.Now()

	// Uuid is what the upsert finds the doc by. It was missing, so the query
	// matched nothing, the mutation changed nothing, and every delete answered
	// 200 while the doc stayed (found by the daily journey, 6 Oct 2026).
	dgraphDoc := &dgraphStruct.DgraphDoc{
		DType:     []string{"Doc"},
		Uid:       "uid(doc)",
		Uuid:      docUUID,
		DeletedAt: &currentTime,
	}

	_, err = domain.CreateOrUpdateDgraphDoc(ctx, dgraphDoc)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/DeleteDoc Failed to delete doc in dgraph err: %+v", err)
		return
	}

	go domain.DeleteDocInOpenSearch(docUUID, currentTime.Unix())
	go globalSearchDomain.SyncCascadingDeletionInOpenSearch(
		[]string{"comment_doc_id", "attachment_doc_id"},
		docUUID,
		currentTime.Unix(),
		[]string{"comments", "attachments"},
		"cascade",
	)
	// Cascade soft-delete to AI embeddings (doc + its comments)
	go globalSearchDomain.SyncCascadingDeletionInOpenSearch(
		[]string{"content_uuid", "doc_uuid"},
		docUUID,
		currentTime.Unix(),
		[]string{"ai_embeddings"},
		"cascade",
	)

	return
}

func GetDgraphDocByUUIDOnlyEditingInfo(ctx context.Context, docUUID string, userUID string) (dgraphDoc *dgraphStruct.DgraphDoc, err error) {

	dgraphDoc, err = domain.GetDgraphDocByUUIDOnlyEditingInfo(ctx, docUUID, userUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetDgraphDocByUUIDOnlyEditingInfo Failed to get doc in dgraph err: %+v", err)
		return
	}

	return
}

func GetBasicDgraphDocByUUID(ctx context.Context, docUUID string, userDgraphUID string) (docDgraph *dgraphStruct.DgraphDoc, err error) {

	docDgraph, err = domain.GetBasicDgraphDocByUUID(ctx, docUUID, userDgraphUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetBasicDgraphDocByUUID Failed to get doc in dgraph err: %+v", err)
		return
	}

	return
}

func GetDocByDocUUID(ctx context.Context, docUUID string, userDgraphUID string) (docDgraph *dgraphStruct.DgraphDoc, err error) {

	docDgraph, err = domain.GetDgraphDocByUUID(ctx, docUUID, userDgraphUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetDoc Failed to get doc in dgraph err: %+v", err)
		return
	}

	return
}

func GetPublicDoc(ctx context.Context, pageIndex int, pageSize int) (dgraphDocList *dgraphStruct.DgraphDocList, err error) {

	dgraphDocList, err = domain.GetDgraphPublicDocFromDgraph(ctx, pageIndex, pageSize)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetPublicDoc Failed to get doc list from dgraph err: %+v", err)
		return
	}

	for _, doc := range dgraphDocList.Docs {
		if len(doc.Snippet) > 0 {
			doc.Body = ""
		} else if len(doc.Body) > 1000 {
			runes := []rune(doc.Body)
			if len(runes) > 1000 {
				doc.Body = string(runes[:1000])
			} else {
				doc.Body = helpers.TruncateRunes(doc.Body, docPreviewRunes)
			}
		}
	}

	return

}

func GetPrivateDoc(ctx context.Context, userDrgaphUID string, pageIndex int, pageSize int) (dgraphDocList *dgraphStruct.DgraphDocList, err error) {

	dgraphDocList, err = domain.GetDgraphPrivateDocFromDgraph(ctx, userDrgaphUID, pageIndex, pageSize)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetPrivateDoc Failed to get doc list from dgraph err: %+v", err)
		return
	}

	for _, doc := range dgraphDocList.Docs {
		if len(doc.Snippet) > 0 {
			doc.Body = ""
		} else if len(doc.Body) > 1000 {
			runes := []rune(doc.Body)
			if len(runes) > 1000 {
				doc.Body = string(runes[:1000])
			} else {
				doc.Body = helpers.TruncateRunes(doc.Body, docPreviewRunes)
			}
		}
	}

	return

}

func GetDocCommentList(ctx context.Context, docUUID string, userUID string) (dgraphDoc *dgraphStruct.DgraphDoc, err error) {
	dgraphDoc, err = domain.GetDgraphDocAllCommentList(ctx, docUUID, userUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetDocCommentList Failed to get doc comment list from dgraph err: %+v", err)
		return
	}

	return
}

func UpdateDoc(ctx context.Context, updateInput *adapter.InputUpdateDoc) (err error) {

	currentTime := time.Now()

	dgraphDoc := &dgraphStruct.DgraphDoc{
		Uid:       "uid(doc)",
		Uuid:      updateInput.DocId,
		UpdatedAt: &currentTime,
	}

	if updateInput.Body != nil {
		// The snippet is a PREVIEW: it is shown in doc lists and search results, and
		// it is indexed into OpenSearch as doc_snippet. Build it from plain text, the
		// way every other snippet in the codebase is built.
		//
		// Taking it from the raw body instead had three visible consequences: a doc
		// that opened with an inlined image previewed as a block of base64 rather
		// than its own words; the cut landed mid-tag, so the fragment rendered as
		// broken markup that could swallow whatever followed it; and the byte-slice
		// path could split a multi-byte character and emit invalid UTF-8.
		dgraphDoc.Body = *updateInput.Body
		dgraphDoc.Snippet = helpers.TruncateRunes(
			helpers.HTMLToPlainText(*updateInput.Body), docSnippetRunes)
	}

	if updateInput.Title != nil {
		dgraphDoc.Title = *updateInput.Title
	}

	if updateInput.IsPrivate != nil {
		dgraphDoc.IsPrivate = updateInput.IsPrivate
	}

	if updateInput.PublicComment != nil {
		dgraphDoc.PublicComment = updateInput.PublicComment
	}

	_, err = domain.CreateOrUpdateDgraphDoc(ctx, dgraphDoc)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateDoc Failed to update doc in dgraph err: %+v", err)
		return
	}

	openSearchDoc := &openSearchStruct.OpenSearchDoc{
		Uuid:         updateInput.DocId,
		DocUpdatedAt: currentTime.Unix(),
	}

	if updateInput.Title != nil {
		openSearchDoc.DocTitle = *updateInput.Title
	}

	if updateInput.Body != nil {
		// Index the body as PLAIN TEXT, never as markup. doc_body is a field to
		// match against, not one to return (global search excludes it from
		// _source), so markup buys nothing and costs plenty: an <img>, <svg> or
		// <video> element carries its payload in an attribute, which is how an
		// image ends up living in the search index. Extracting text drops every
		// tag and therefore every attribute, so media cannot reach the cluster
		// through a document body at all.
		//
		// This also matches how every other indexed body is built (posts,
		// comments, chats all index helpers.HTMLToPlainText output) and makes
		// matching behave: with markup indexed, a search for "div" hit every doc.
		openSearchDoc.DocBody = helpers.HTMLToPlainText(*updateInput.Body)
		openSearchDoc.DocSnippet = dgraphDoc.Snippet
	}

	if updateInput.IsPrivate != nil {
		openSearchDoc.DocPrivate = *updateInput.IsPrivate
	}

	go func() {
		domain.UpdateDocInOpenSearch(openSearchDoc)

		/* AI call omitted in v1 */

		// 2. Cascading update for doc comments and attachments
		updatedDoc, err := domain.GetDocPermissions(context.Background(), updateInput.DocId)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/UpdateDoc Failed to get updated doc for OpenSearch sync err: %+v", err)
			return
		}

		if updatedDoc == nil {
			helpers.LogErrorWithContext(ctx, "business/UpdateDoc Failed to get updated doc for OpenSearch sync: doc is nil")
			return
		}

		readingUsers := []string{}
		for _, u := range updatedDoc.ReadingUser {
			readingUsers = append(readingUsers, u.Uuid)
		}
		editingUsers := []string{}
		for _, u := range updatedDoc.EditingUser {
			editingUsers = append(editingUsers, u.Uuid)
		}
		commentingUsers := []string{}
		for _, u := range updatedDoc.CommentingUser {
			commentingUsers = append(commentingUsers, u.Uuid)
		}

		createdBy := ""
		if updatedDoc.CreatedBy != nil {
			createdBy = updatedDoc.CreatedBy.Uuid
		}

		err = openSearchGlobalSearchModels.SyncDocMetadataInOpenSearch(ctx, updateInput.DocId, updateInput.Title, updatedDoc.IsPrivate, readingUsers, editingUsers, commentingUsers, createdBy)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/UpdateDoc Failed to sync metadata for doc comments/attachments err: %+v", err)
		}
	}()

	return
}

func CheckUserDocEditAccess(ctx context.Context, docUUID string, userUID string) (bool, error) {
	checkDoc, err := domain.GetDgraphDocByUUIDOnlyEditingInfo(ctx, docUUID, userUID)
	if err != nil {
		return false, err
	}
	if checkDoc == nil {
		return false, nil
	}
	// Check if user is editor OR owner
	if checkDoc.HasEditAccess > 0 || checkDoc.CreatedBy != nil {
		return true, nil
	}
	return false, nil
}

func CheckUserIsDocOwner(ctx context.Context, docUUID string, userUID string) (bool, error) {
	checkDoc, err := domain.GetDgraphDocByUUIDOnlyEditingInfo(ctx, docUUID, userUID)
	if err != nil {
		return false, err
	}
	if checkDoc == nil {
		return false, nil
	}
	// Check if user is owner
	if checkDoc.CreatedBy != nil && checkDoc.CreatedBy.Uid == userUID {
		return true, nil
	}
	return false, nil
}

func UpdateDocPermissions(ctx context.Context, input adapter.InputUpdateDocPermissions, userUID string) (err error) {
	// 1. Verify Owner Access
	// Fetch doc first to check owner. Domain call returns doc with CreatedBy
	checkDoc, err := domain.GetDgraphDocByUUIDOnlyEditingInfo(ctx, input.DocId, userUID)
	if err != nil {
		return err
	}
	if checkDoc == nil {
		return errors.New("document not found")
	}

	if checkDoc.CreatedBy == nil || checkDoc.CreatedBy.Uid != userUID {
		return errors.New("unauthorized: only document owner can manage permissions")
	}

	// 2. Enforce Single Role Policy (Auto-remove from other roles)
	// Helpers to manage lists
	ensureUnique := func(slice []string) []string {
		keys := make(map[string]bool)
		list := []string{}
		for _, entry := range slice {
			if _, value := keys[entry]; !value {
				keys[entry] = true
				list = append(list, entry)
			}
		}
		return list
	}

	if len(input.AddEditors) > 0 {
		input.RemoveViewers = append(input.RemoveViewers, input.AddEditors...)
		input.RemoveCommenters = append(input.RemoveCommenters, input.AddEditors...)
	}
	if len(input.AddViewers) > 0 {
		input.RemoveEditors = append(input.RemoveEditors, input.AddViewers...)
		input.RemoveCommenters = append(input.RemoveCommenters, input.AddViewers...)
	}
	if len(input.AddCommenters) > 0 {
		input.RemoveEditors = append(input.RemoveEditors, input.AddCommenters...)
		input.RemoveViewers = append(input.RemoveViewers, input.AddCommenters...)
	}

	// Clean up duplicates (optional but good for Dgraph cleanliness)
	input.RemoveEditors = ensureUnique(input.RemoveEditors)
	input.RemoveViewers = ensureUnique(input.RemoveViewers)
	input.RemoveCommenters = ensureUnique(input.RemoveCommenters)

	err = domain.UpdateDocPermissions(ctx, &input)
	if err != nil {
		return err
	}

	// Sync with OpenSearch
	go func() {
		updatedDoc, err := domain.GetDocPermissions(context.Background(), input.DocId)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/UpdateDocPermissions Failed to get updated doc for OpenSearch sync err: %+v", err)
			return
		}

		if updatedDoc == nil {
			helpers.LogErrorWithContext(ctx, "business/UpdateDocPermissions Failed to get updated doc for OpenSearch sync: doc is nil")
			return
		}

		openSearchDoc := &openSearchStruct.OpenSearchDoc{
			Uuid:         input.DocId,
			DocUpdatedAt: time.Now().Unix(),
		}

		for _, u := range updatedDoc.ReadingUser {
			openSearchDoc.DocReadingUsers = append(openSearchDoc.DocReadingUsers, u.Uuid)
		}
		for _, u := range updatedDoc.EditingUser {
			openSearchDoc.DocEditingUsers = append(openSearchDoc.DocEditingUsers, u.Uuid)
		}
		for _, u := range updatedDoc.CommentingUser {
			openSearchDoc.DocCommentingUsers = append(openSearchDoc.DocCommentingUsers, u.Uuid)
		}

		domain.UpdateDocInOpenSearch(openSearchDoc)

		createdBy := ""
		if updatedDoc.CreatedBy != nil {
			createdBy = updatedDoc.CreatedBy.Uuid
		}

		// 3. Sync permissions for doc comments and attachments
		err = openSearchGlobalSearchModels.SyncDocMetadataInOpenSearch(ctx, input.DocId, nil, updatedDoc.IsPrivate, openSearchDoc.DocReadingUsers, openSearchDoc.DocEditingUsers, openSearchDoc.DocCommentingUsers, createdBy)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/UpdateDocPermissions Failed to sync permissions for doc comments/attachments err: %+v", err)
		}
	}()

	return nil
}

func SearchUsersForDoc(ctx context.Context, userDgraphUUID string, searchText string) (usersList []*dgraphStruct.DgraphUser, err error) {
	dgraphUsers, err := userDomain.GetUserListWithSearchText(ctx, userDgraphUUID, searchText)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/SearchUsersForDoc Failed to get users list err: %+v", err)
		return nil, err
	}

	return dgraphUsers, nil
}

func GetDocPermissions(ctx context.Context, docUUID string, userUID string) (dgraphDoc *dgraphStruct.DgraphDoc, err error) {
	// 1. Verify user has edit access
	hasEditAccess, err := CheckUserDocEditAccess(ctx, docUUID, userUID)
	if err != nil {
		return
	}
	if !hasEditAccess {
		err = errors.New("user does not have permission to manage this document")
		return
	}

	return domain.GetDocPermissions(ctx, docUUID)
}

func CreateDocComment(ctx context.Context, rawDocDgraph *dgraphStruct.DgraphDoc, createdByUser *model.UserInfo, createTaskCommentInfoInput *adapter.CreateOrUpdateDocCommentInput, mentionsDgraphUsersList []*dgraphStruct.DgraphUser) (commentInfoRes *adapter.OutputCreateCommentForDoc, err error) {

	commentUUID := uuid.New()
	currentTime := time.Now()
	zeroUnixTime := time.Time{}

	var commentMentions []*dgraphStruct.DgraphUser

	for _, mention := range mentionsDgraphUsersList {
		commentMentions = append(commentMentions, &dgraphStruct.DgraphUser{
			DType: []string{"User"},
			Uid:   mention.Uid,
		})
	}

	for _, mediaObj := range createTaskCommentInfoInput.Attachments {
		mediaObj.DType = []string{"Attachment"}
		mediaObj.CreatedBy = &dgraphStruct.DgraphUser{
			Uid: createdByUser.UserDgraphInfo.Uid,
		}
		mediaObj.CreatedAt = &currentTime
	}

	dgraphDoc := &dgraphStruct.DgraphDoc{
		Uid:  "uid(doc)",
		Uuid: rawDocDgraph.Uuid,
		Comments: []*dgraphStruct.DgraphComment{
			{
				DType: []string{"Comment"},
				Uid:   "uid(co)",
				Uuid:  commentUUID.String(),
				Text:  createTaskCommentInfoInput.CommentBody,
				Doc: &dgraphStruct.DgraphDoc{
					Uid: "uid(doc)",
				},
				ContentAddedBy: &dgraphStruct.DgraphUser{
					Uid: rawDocDgraph.CreatedBy.Uid,
				},
				Attachments: createTaskCommentInfoInput.Attachments,
				CreatedAt:   &currentTime,
				DeletedAt:   &zeroUnixTime,
				Mentions: &dgraphStruct.DgraphMentions{
					CreatedAt:   &currentTime,
					Mentions:    commentMentions,
					CommentUuid: commentUUID.String(),
					Comment: &dgraphStruct.DgraphComment{
						Uid: "uid(co)",
					},
					DType: []string{"Mention"},
				},
				CommentBy: &dgraphStruct.DgraphUser{
					Uid: createdByUser.UserDgraphInfo.Uid,
				},
			},
		},
	}

	_, err = businessComment.CreateCommentInADoc(ctx, dgraphDoc, createdByUser, commentUUID, currentTime, rawDocDgraph, mentionsDgraphUsersList)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/CreateDocComment Failed to create comment in dgraph err: %+v", err)
		return
	}

	commentInfoRes = &adapter.OutputCreateCommentForDoc{
		Uuid:             commentUUID.String(),
		CommentCreatedAt: currentTime,
	}

	mqttDocComment := mqttStruct.MqttDocComment{
		Type:           mqttStruct.TYPE_CREATE,
		DocUuid:        rawDocDgraph.Uuid,
		CommentUuid:    commentUUID.String(),
		CreatedAt:      &currentTime,
		HTMLText:       createTaskCommentInfoInput.CommentBody,
		UserUuid:       createdByUser.UserDgraphInfo.Uuid,
		UserName:       createdByUser.UserDgraphInfo.UserName,
		UserProfileKey: createdByUser.UserDgraphInfo.ProfileKey,
		Attachments:    createTaskCommentInfoInput.Attachments,
	}

	go mqttBusiness.PublishDocComment(&mqttDocComment, rawDocDgraph.Uuid)

	go PublishDocCommentActivity(commentUUID.String(), createTaskCommentInfoInput.CommentBody, currentTime, rawDocDgraph, createdByUser.UserDgraphInfo)

	return
}

func PublishDocCommentActivity(commentUUID string, commentBody string, currentTime time.Time, rawDocDgraph *dgraphStruct.DgraphDoc, draphUser dgraphStruct.DgraphUser) {
	if rawDocDgraph.CreatedBy.Uuid != draphUser.Uuid {
		activityItem := &dgraphModels.UnifiedActivityItem{
			ActivityType: mqttStruct.MESSAGE_ACTIVITY_COMMENT,
			Time:         time.Now().Format(time.RFC3339),
			Comment: &dgraphStruct.DgraphComment{
				Uuid: commentUUID,
				Text: commentBody,
				CommentBy: &dgraphStruct.DgraphUser{
					Uuid:     draphUser.Uuid,
					UserName: draphUser.UserName,
				},
				Doc: &dgraphStruct.DgraphDoc{
					Uuid:  rawDocDgraph.Uuid,
					Title: rawDocDgraph.Title,
				},
				CreatedAt: &currentTime,
			},
		}
		activityBusiness.PublishActivityToUser(rawDocDgraph.CreatedBy.Uuid, activityItem)
	}

}

func DeleteDocComment(ctx context.Context, commentUUID uuid.UUID, docId string, rawDgraphComment *dgraphStruct.DgraphComment) (err error) {
	currentTime := time.Now()
	dgraphComment := &dgraphStruct.DgraphComment{
		Uid:       "uid(co)",
		Uuid:      commentUUID.String(),
		DeletedAt: &currentTime,
		DType:     []string{"Comment"},
	}
	err = businessComment.DeleteComment(ctx, dgraphComment, currentTime, commentUUID, rawDgraphComment)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/DeleteDocComment Failed to delete comment from doc err: %+v",
			err)

		return
	}

	mqttDocComment := mqttStruct.MqttDocComment{
		Type:        mqttStruct.TYPE_DELETE,
		DocUuid:     docId,
		CommentUuid: commentUUID.String(),
		UserUuid:    rawDgraphComment.CommentBy.Uuid,
	}

	go mqttBusiness.PublishDocComment(&mqttDocComment, rawDgraphComment.Doc.Uuid)

	return
}

func UpdateDocComment(ctx context.Context, commentUUID uuid.UUID, commentInfo *adapter.CreateOrUpdateDocCommentInput, mentionsDgraphUsersList []*dgraphStruct.DgraphUser, rawDgraphCommentInfo *dgraphStruct.DgraphComment) (err error) {
	currentTime := time.Now()

	var commentMentions []*dgraphStruct.DgraphUser

	for _, mention := range mentionsDgraphUsersList {
		commentMentions = append(commentMentions, &dgraphStruct.DgraphUser{
			DType: []string{"User"},
			Uid:   mention.Uid,
		})
	}

	dgraphComment := dgraphStruct.DgraphComment{
		Uid:   "uid(co)",
		Uuid:  commentInfo.UUID,
		DType: []string{"Comment"},
		Text:  commentInfo.CommentBody,
		// Attachments: commentInfo.MediaObj,
		Mentions: &dgraphStruct.DgraphMentions{
			Uid: "uid(me)",
			Comment: &dgraphStruct.DgraphComment{
				Uid: "uid(co)",
			},
			CommentUuid: commentInfo.UUID,
			Mentions:    commentMentions,
		},
		UpdatedAt: &currentTime,
	}

	err = businessComment.UpdateComment(ctx, &dgraphComment, commentUUID, currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/UpdateDocComment Failed to update comment on doc err: %+v",
			err)
		return
	}

	mqttDocComment := mqttStruct.MqttDocComment{
		Type:        mqttStruct.TYPE_UPDATE,
		DocUuid:     rawDgraphCommentInfo.Doc.Uuid,
		CommentUuid: commentUUID.String(),
		UpdatedAt:   &currentTime,
		UserUuid:    rawDgraphCommentInfo.CommentBy.Uuid,
		HTMLText:    commentInfo.CommentBody,
	}

	go mqttBusiness.PublishDocComment(&mqttDocComment, rawDgraphCommentInfo.Doc.Uuid)

	return
}

func DeleteCommentOnDoc(ctx context.Context, commentUUID uuid.UUID, rawDgraphComment *dgraphStruct.DgraphComment, userId string) (err error) {

	currentTime := time.Now()
	dgraphComment := &dgraphStruct.DgraphComment{
		Uid:       "uid(co)",
		Uuid:      commentUUID.String(),
		DeletedAt: &currentTime,
		DType:     []string{"Comment"},
	}
	err = businessComment.DeleteComment(ctx, dgraphComment, currentTime, commentUUID, rawDgraphComment)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/DeleteCommentOnDoc Failed to delete comment from doc err: %+v",
			err)

		return
	}

	mqttDocComment := mqttStruct.MqttDocComment{
		Type:        mqttStruct.TYPE_DELETE,
		DocUuid:     rawDgraphComment.Doc.Uuid,
		CommentUuid: commentUUID.String(),
		UserUuid:    userId,
	}

	go mqttBusiness.PublishDocComment(&mqttDocComment, rawDgraphComment.Doc.Uuid)

	return
}

func CreateOrUpdateDocCommentReaction(ctx context.Context, reactionInfo *adapter.InputUpdateReactionForCommentInDoc, dgraphCommentRaw *dgraphStruct.DgraphComment, userDgraph *dgraphStruct.DgraphUser) (reactionUUID string, err error) {
	currentTime := time.Now()

	dgraphComment := &dgraphStruct.DgraphComment{
		Uid:   "uid(co)",
		Uuid:  reactionInfo.CommentId,
		DType: []string{"Comment"},
		Reactions: []*dgraphStruct.DgraphReaction{
			{
				Uid:       reactionInfo.ReactionDgraphUid,
				DType:     []string{"Reaction"},
				EmojiUuid: reactionInfo.EmojiReactionUuid,
				AddedAt:   &currentTime,
				ContentAddedBy: &dgraphStruct.DgraphUser{
					Uid: dgraphCommentRaw.CommentBy.Uid,
				},
				AddedBy: &dgraphStruct.DgraphUser{
					Uid: userDgraph.Uid,
				},
			},
		},
	}

	reactionUUID, err = businessComment.CreateOrUpdateDgraphCommentReaction(ctx, dgraphComment)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateOrUpdateDocCommentReaction Failed to update comment reaction in doc err: %+v",
			err)
		return
	}

	mqttDocCommentReaction := mqttStruct.MqttDocCommentReaction{
		Type:            mqttStruct.TYPE_CREATE,
		EmojiReactionId: reactionInfo.EmojiReactionUuid,
		CommentUuid:     reactionInfo.CommentId,
		AddedByUserName: userDgraph.UserName,
		AddedByUuid:     userDgraph.Uuid,
		ReactionUuid:    reactionUUID,
		DocUuid:         dgraphCommentRaw.Doc.Uuid,
	}

	if len(mqttDocCommentReaction.ReactionUuid) == 0 {
		mqttDocCommentReaction.Type = mqttStruct.TYPE_UPDATE
		mqttDocCommentReaction.ReactionUuid = reactionInfo.ReactionDgraphUid
	}

	go mqttBusiness.PublishDocCommentReaction(&mqttDocCommentReaction, dgraphCommentRaw.Doc.Uuid)

	return
}

func DeleteReactionOnCommentDoc(ctx context.Context, commentDgraph *dgraphStruct.DgraphComment, reactionDgraphUUID string) (err error) {
	err = businessComment.DeleteCommentReaction(ctx, commentDgraph.Uid, reactionDgraphUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/DeleteReactionOnCommentDoc Failed to delete reaction on comment in doc err: %+v",
			err)

		return
	}

	mqttDocCommentReaction := mqttStruct.MqttDocCommentReaction{
		Type:         mqttStruct.TYPE_DELETE,
		CommentUuid:  commentDgraph.Uuid,
		ReactionUuid: reactionDgraphUUID,
		DocUuid:      commentDgraph.Doc.Uuid,
		AddedByUuid:  commentDgraph.CommentBy.Uuid, // Assuming the user deleting is the one who added it, or we need `userId` param
	}

	go mqttBusiness.PublishDocCommentReaction(&mqttDocCommentReaction, commentDgraph.Doc.Uuid)

	return
}

func GetPrivateDocListWithSearchText(ctx context.Context, userDgraphInfo *dgraphStruct.DgraphUser, inputDocName *adapter.InputSearchDocName) (dgraphDoc *dgraphStruct.DgraphDocList, err error) {

	dgraphDoc, err = domain.GetDgraphPrivateDocFromDgraphWithSearchText(ctx, userDgraphInfo.Uid, inputDocName)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetPrivateDocListWithSearchText Failed to get doc list from dgraph err: %+v", err)
		return
	}

	for _, doc := range dgraphDoc.Docs {
		if len(doc.Snippet) > 0 {
			doc.Body = ""
		} else if len(doc.Body) > 1000 {
			runes := []rune(doc.Body)
			if len(runes) > 1000 {
				doc.Body = string(runes[:1000])
			} else {
				doc.Body = helpers.TruncateRunes(doc.Body, docPreviewRunes)
			}
		}
	}

	return
}

func GetPublicDocListWithSearchText(ctx context.Context, inputDocName *adapter.InputSearchDocName) (dgraphDoc *dgraphStruct.DgraphDocList, err error) {

	dgraphDoc, err = domain.GetDgraphPublicDocFromDgraphWithSearchText(ctx, inputDocName)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetPublicDocListWithSearchText Failed to get doc list from dgraph err: %+v", err)
		return
	}

	for _, doc := range dgraphDoc.Docs {
		if len(doc.Snippet) > 0 {
			doc.Body = ""
		} else if len(doc.Body) > 1000 {
			runes := []rune(doc.Body)
			if len(runes) > 1000 {
				doc.Body = string(runes[:1000])
			} else {
				doc.Body = helpers.TruncateRunes(doc.Body, docPreviewRunes)
			}
		}
	}

	return
}

func GetSystemDocByUUID(ctx context.Context, docUUID string) (dgraphDoc *dgraphStruct.DgraphDoc, err error) {

	dgraphDoc, err = domain.GetSystemDocByUUID(ctx, docUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetSystemDocByUUID Failed to get doc from dgraph err: %+v",
			err,
		)
		return
	}

	return
}
