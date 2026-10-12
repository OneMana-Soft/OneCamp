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
	docModels "github.com/akashc777/OneCamp/models/dgraph/Doc"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"

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
		// Made is the first change: a doc nobody has edited yet sorted last
		// among its owner's docs, which order by it.
		UpdatedAt: &currentTime,
		DeletedAt: &zeroUnixTime,
	}

	_, err = domain.CreateOrUpdateDgraphDoc(ctx, dgraphDoc)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/CreateDoc Failed to create doc in dgraph err: %+v", err)
		return
	}

	// Its search entry, from the doc as stored: its editors include anyone
	// it was made editable by, and a public doc says it is public.
	syncDocSearch(dgraphDoc.Uuid, false)

	// embed for AI Second Brain (async)
	/* AI call omitted in v1 */

	return
}

func DeleteDoc(ctx context.Context, docUUID string) (err error) {

	currentTime := time.Now()

	// By its uuid, and only a live doc: a doc already deleted, or none, is an
	// error, never a new node (found by the daily journey, 6 Oct 2026: the
	// uuid was once missing, and every delete answered 200 while the doc
	// stayed).
	err = domain.MarkDocDeleted(ctx, docUUID, currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/DeleteDoc Failed to delete doc in dgraph err: %+v", err)
		return
	}

	syncDocSearch(docUUID, false)
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

// ErrNotFound is the error a doc lookup gives for a doc that doesn't exist, as
// opposed to one the graph couldn't be asked about.
var ErrNotFound = docModels.ErrNotFound

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
	return updateDoc(ctx, updateInput, "", false)
}

// updateDoc is UpdateDoc, and with a Yjs state also saves it beside the body,
// with the body's hash, in the same Dgraph mutation: the body and the state it
// was written from are stored together or not at all.
func updateDoc(ctx context.Context, updateInput *adapter.InputUpdateDoc, yjsState string, inTrashToo bool) (err error) {

	currentTime := time.Now()

	dgraphDoc := &dgraphStruct.DgraphDoc{
		Uid:       "uid(doc)",
		Uuid:      updateInput.DocId,
		UpdatedAt: &currentTime,
	}
	if yjsState != "" && updateInput.Body != nil {
		dgraphDoc.YjsState = yjsState
		dgraphDoc.YjsBodyHash = collabBodyHash(*updateInput.Body)
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

	// Only a live doc is written: a deleted one is refused (ErrDocDeleted), so
	// an editor still open on a doc its owner deleted saves nothing into the
	// trash, and no caller can bring one back by editing it. inTrashToo is a
	// repair of what the doc itself holds (repairdocs), which a doc in the
	// trash gets too, for when it is restored.
	if inTrashToo {
		err = domain.UpdateStoredDoc(ctx, dgraphDoc)
	} else {
		err = domain.UpdateLiveDoc(ctx, dgraphDoc)
	}

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateDoc Failed to update doc in dgraph err: %+v", err)
		return
	}

	// Its search entry, written whole from the doc as it now is (searchEntry.go).
	// The body is indexed as PLAIN TEXT, never as markup. doc_body is a field
	// to match against, not one to return (global search excludes it from
	// _source), so markup buys nothing and costs plenty: an <img>, <svg> or
	// <video> element carries its payload in an attribute, which is how an
	// image ends up living in the search index. Extracting text drops every tag
	// and therefore every attribute, so media cannot reach the cluster through
	// a document body at all; and with markup indexed, a search for "div" hit
	// every doc. Its comments' and files' entries carry its title and privacy,
	// so they are written too when those changed, and not on every save from
	// the editor.
	syncDocSearch(updateInput.DocId, updateInput.Title != nil || updateInput.IsPrivate != nil)

	return
}

func CheckUserDocEditAccess(ctx context.Context, docUUID string, userUID string) (bool, error) {
	checkDoc, err := domain.GetDgraphDocByUUIDOnlyEditingInfo(ctx, docUUID, userUID)
	if err != nil {
		return false, err
	}
	return CanEdit(checkDoc, userUID), nil
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

	if !IsOwner(checkDoc, userUID) {
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

	// Its search entries, its own and its comments' and files', written from
	// who it is shared with now: someone taken off it stops finding it.
	syncDocSearch(input.DocId, true)

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
			newDocComment(rawDocDgraph, createdByUser.UserDgraphInfo.Uid, createTaskCommentInfoInput, mentionsDgraphUsersList, commentUUID, currentTime),
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
		UserName:       createdByUser.UserDgraphInfo.DisplayName(),
		UserProfileKey: createdByUser.UserDgraphInfo.ProfileKey,
		Attachments:    createTaskCommentInfoInput.Attachments,
	}

	go mqttBusiness.PublishDocComment(&mqttDocComment, rawDocDgraph.Uuid)

	go PublishDocCommentActivity(commentUUID.String(), createTaskCommentInfoInput.CommentBody, currentTime, rawDocDgraph, createdByUser.UserDgraphInfo, mentionsDgraphUsersList)

	return
}

// newDocComment is the comment node CreateDocComment adds to doc: written by
// authorUID at at, mentioning mentions, with the input's body and files. It is
// on the content of the doc's creator (comment_on_content_added_by), and of
// nobody's on a doc with no creator, which everyone may comment on when it is
// public and takes everyone's comments: reading that doc's creator took the
// request down. Pure.
func newDocComment(doc *dgraphStruct.DgraphDoc, authorUID string, input *adapter.CreateOrUpdateDocCommentInput, mentions []*dgraphStruct.DgraphUser, commentUUID uuid.UUID, at time.Time) *dgraphStruct.DgraphComment {
	var mentioned []*dgraphStruct.DgraphUser
	for _, mention := range mentions {
		mentioned = append(mentioned, &dgraphStruct.DgraphUser{
			DType: []string{"User"},
			Uid:   mention.Uid,
		})
	}
	zeroUnixTime := time.Time{}
	comment := &dgraphStruct.DgraphComment{
		DType: []string{"Comment"},
		Uid:   "uid(co)",
		Uuid:  commentUUID.String(),
		Text:  input.CommentBody,
		Doc: &dgraphStruct.DgraphDoc{
			Uid: "uid(doc)",
		},
		Attachments: input.Attachments,
		CreatedAt:   &at,
		DeletedAt:   &zeroUnixTime,
		Mentions: &dgraphStruct.DgraphMentions{
			CreatedAt:   &at,
			Mentions:    mentioned,
			CommentUuid: commentUUID.String(),
			Comment: &dgraphStruct.DgraphComment{
				Uid: "uid(co)",
			},
			DType: []string{"Mention"},
		},
		CommentBy: &dgraphStruct.DgraphUser{
			Uid: authorUID,
		},
	}
	if doc != nil && doc.CreatedBy != nil && doc.CreatedBy.Uid != "" {
		comment.ContentAddedBy = &dgraphStruct.DgraphUser{Uid: doc.CreatedBy.Uid}
	}
	return comment
}

// PublishDocCommentActivity puts a new comment in the activity of whoever it
// is for, live, as their feed lists it when it loads: the doc's owner, as a
// comment on what they made, unless they wrote it; and anyone it mentions who
// may read the doc, as a mention. The owner was read without a check (a doc
// with no maker took the goroutine down), and a mention reached nobody live.
// Push and email are business/Comment's (sendNewDocCommentNotification).
func PublishDocCommentActivity(commentUUID string, commentBody string, currentTime time.Time, rawDocDgraph *dgraphStruct.DgraphDoc, author dgraphStruct.DgraphUser, mentions []*dgraphStruct.DgraphUser) {
	if rawDocDgraph == nil {
		return
	}
	comment := func() *dgraphStruct.DgraphComment {
		return &dgraphStruct.DgraphComment{
			Uuid: commentUUID,
			Text: commentBody,
			CommentBy: &dgraphStruct.DgraphUser{
				Uuid:     author.Uuid,
				UserName: author.DisplayName(),
			},
			Doc: &dgraphStruct.DgraphDoc{
				Uuid:  rawDocDgraph.Uuid,
				Title: rawDocDgraph.Title,
			},
			CreatedAt: &currentTime,
		}
	}
	owner := ""
	if rawDocDgraph.CreatedBy != nil {
		owner = rawDocDgraph.CreatedBy.Uuid
	}
	if owner != "" && owner != author.Uuid {
		activityBusiness.PublishActivityToUser(owner, &dgraphModels.UnifiedActivityItem{
			ActivityType: mqttStruct.MESSAGE_ACTIVITY_COMMENT,
			Time:         time.Now().UTC().Format(time.RFC3339Nano),
			Comment:      comment(),
		})
	}
	told := map[string]bool{author.Uuid: true, owner: true, "": true}
	for _, m := range mentions {
		if m == nil || told[m.Uuid] || !domain.ReadableBy(rawDocDgraph, m.Uuid) {
			continue
		}
		told[m.Uuid] = true
		activityBusiness.PublishActivityToUser(m.Uuid, &dgraphModels.UnifiedActivityItem{
			ActivityType: mqttStruct.MESSAGE_ACTIVITY_MENTION,
			Time:         time.Now().UTC().Format(time.RFC3339Nano),
			Mention: &dgraphStruct.DgraphMentions{
				CommentUuid: commentUUID,
				Comment:     comment(),
				CreatedAt:   &currentTime,
			},
		})
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
		AddedByUserName: userDgraph.DisplayName(),
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

	// As the collaboration service opens it: with its saved Yjs state.
	dgraphDoc, err = domain.GetCollabDocByUUID(ctx, docUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetSystemDocByUUID Failed to get doc from dgraph err: %+v",
			err,
		)
		return
	}

	return
}
