package businness

import (
	"context"
	"errors"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Doc"
	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	businessComment "github.com/akashc777/OneCamp/business/Comment"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	domain "github.com/akashc777/OneCamp/domain/Doc"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
	docModels "github.com/akashc777/OneCamp/models/dgraph/Doc"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"

	model "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"

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

// CreateDoc creates a document owned by createdByUser.
//
// alsoEditableBy grants edit access to further users at creation. Variadic
// because every existing caller creates a document for one person, and the one
// caller that does not is the meeting notes agent: a document about a call
// belongs to the people who were on it, not to whichever of them the agent
// happened to author it as. Granting at creation rather than afterwards means
// there is no window where the document exists and its participants cannot open
// it. Duplicates and the creator's own uid are ignored.
func CreateDoc(ctx context.Context, createdByUser *dgraphStruct.DgraphUser, inputDoc *adapter.InputCreateDoc, alsoEditableBy ...string) (dgraphDoc *dgraphStruct.DgraphDoc, err error) {

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
		EditingUser:   docEditors(createdByUser.Uid, alsoEditableBy),
		CreatedAt:     &currentTime,
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
	ai.EmbedDocContent(dgraphDoc.Title, "", dgraphDoc.Uuid, createdByUser.Uuid, createdByUser.DisplayName(), *dgraphDoc.IsPrivate, createdByUser.Uuid, nil, []string{createdByUser.Uuid}, nil)

	return
}

// docEditors builds the editing-user set for a new document: the creator, plus
// any extra uids, deduplicated. Empty uids are dropped rather than written,
// because an empty uid in a Dgraph edge is a dangling reference nobody can
// resolve and nothing would report.
func docEditors(creatorUID string, extra []string) []*dgraphStruct.DgraphUser {
	seen := map[string]bool{}
	out := make([]*dgraphStruct.DgraphUser, 0, len(extra)+1)
	for _, uid := range append([]string{creatorUID}, extra...) {
		uid = strings.TrimSpace(uid)
		if uid == "" || seen[uid] {
			continue
		}
		seen[uid] = true
		out = append(out, &dgraphStruct.DgraphUser{Uid: uid})
	}
	return out
}

// ErrNotFound is the error a doc lookup gives for a doc that doesn't exist, as
// opposed to one the graph couldn't be asked about.
var ErrNotFound = docModels.ErrNotFound

// GetDgraphDocByUUIDOnlyEditingInfo, GetBasicDgraphDocByUUID, GetDocByDocUUID,
// GetDocCommentList and GetSystemDocByUUID read one doc. A read that failed is
// logged where it is made (domain/Doc, helpers.LogReadFailure); one that found
// nothing comes back as ErrNotFound, unlogged, for the caller to answer.
func GetDgraphDocByUUIDOnlyEditingInfo(ctx context.Context, docUUID string, userUID string) (*dgraphStruct.DgraphDoc, error) {
	return domain.GetDgraphDocByUUIDOnlyEditingInfo(ctx, docUUID, userUID)
}

func GetBasicDgraphDocByUUID(ctx context.Context, docUUID string, userDgraphUID string) (*dgraphStruct.DgraphDoc, error) {
	return domain.GetBasicDgraphDocByUUID(ctx, docUUID, userDgraphUID)
}

func GetDocByDocUUID(ctx context.Context, docUUID string, userDgraphUID string) (*dgraphStruct.DgraphDoc, error) {
	return domain.GetDgraphDocByUUID(ctx, docUUID, userDgraphUID)
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

func GetDocCommentList(ctx context.Context, docUUID string, userUID string) (*dgraphStruct.DgraphDoc, error) {
	return domain.GetDgraphDocAllCommentList(ctx, docUUID, userUID)
}

func UpdateDoc(ctx context.Context, updateInput *adapter.InputUpdateDoc) (err error) {
	// A rename or a change to who may open or comment on it is a Resource
	// Event (docEvents.go), told against the doc as it was.
	before := headBeforeUpdate(ctx, updateInput)
	if err = updateDoc(ctx, updateInput, "", false); err == nil {
		emitUpdated(ctx, before, updateInput)
	}
	return err
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

	helpers.GoSafeNamed("doc.update-ai-entries", func() {
		// The doc as it is now: who may see it and who it's shared with. The
		// AI index's entries are filtered by these, so they come from the
		// doc, not from the update, which carries privacy only when the
		// owner changes it (a save from the editor carries the body alone,
		// and indexing that as public put a private doc in everyone's AI
		// search).
		updatedDoc, err := domain.GetDocPermissions(context.Background(), updateInput.DocId)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/UpdateDoc Failed to get updated doc for OpenSearch sync err: %+v", err)
			return
		}

		if updatedDoc == nil {
			helpers.LogErrorWithContext(ctx, "business/UpdateDoc Failed to get updated doc for OpenSearch sync: doc is nil")
			return
		}
		access := ai.DocAccessOf(updateInput.DocId, updatedDoc)

		// Re-embed for AI Second Brain if content changed. The text is the doc's
		// title and body as stored after the save, not the update's: a save from
		// the editor carries the body alone and a rename the title alone, and
		// embedding only what the update carried dropped the title from the
		// vector (or, on a rename, the whole body).
		if updateInput.Title != nil || updateInput.Body != nil {
			stored, err := domain.GetSystemDocByUUID(context.Background(), updateInput.DocId)
			switch {
			case err != nil || stored == nil:
				helpers.LogErrorWithContext(ctx, "business/UpdateDoc Failed to read the saved doc to re-embed it err: %+v", err)
			case domain.IsHosted(stored):
				// A hosted doc (a table row's page) is in no docs index, the
				// AI's included: it is found through its host (D-18).
			default:
				// Plain text, for the same reason the indexed body is plain text —
				// and one more: this text is what the embedding VECTOR is computed
				// from. Feeding it markup spends the model's limited input budget on
				// tags (and, when an image was inlined, on base64) instead of the
				// document's words, which makes the vector describe noise and
				// quietly degrades semantic search for that doc.
				body := helpers.HTMLToPlainText(stored.Body)
				ai.EmbedDocContent(stored.Title, body, updateInput.DocId, "", "", access.Private, access.CreatedBy, access.Reading, access.Editing, access.Commenting)
			}
		}

		// Who may see it changed: its comments' entries follow (and its own,
		// when no text changed and it wasn't re-embedded).
		if updateInput.IsPrivate != nil {
			if _, err := ai.SetDocAccess(context.Background(), []ai.DocAccess{access}); err != nil {
				helpers.LogErrorWithContext(ctx, "business/UpdateDoc Failed to write the doc's privacy to the AI index err: %+v", err)
			}
		}
	})

	return
}

func UpdateDocPermissions(ctx context.Context, input adapter.InputUpdateDocPermissions, userUID string) (err error) {
	// 1. Verify Owner Access
	// Fetch doc first to check owner. Domain call returns doc with CreatedBy
	checkDoc, err := domain.GetDgraphDocByUUIDOnlyEditingInfo(ctx, input.DocId, userUID)
	if err != nil {
		return err
	}
	if checkDoc == nil {
		return ErrNotFound
	}

	if !IsOwner(checkDoc, userUID) {
		return errors.New("unauthorized: only document owner can manage permissions")
	}
	// Who could open it before, so the change is told as what it did
	// (docEvents.go): the people it's now shared with, and those whose access
	// changed. input as the owner sent it, before the single-role rule below.
	sharedBefore, _ := domain.GetDocPermissions(ctx, input.DocId)
	asked := input

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
	emitShared(ctx, sharedBefore, asked, checkDoc.CreatedBy.Uuid)

	// Its search entries, its own and its comments' and files', written from
	// who it is shared with now: someone taken off it stops finding it.
	syncDocSearch(input.DocId, true)

	helpers.GoSafeNamed("doc.sharing-ai-entries", func() {
		updatedDoc, err := domain.GetDocPermissions(context.Background(), input.DocId)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/UpdateDocPermissions Failed to get updated doc for OpenSearch sync err: %+v", err)
			return
		}

		if updatedDoc == nil {
			helpers.LogErrorWithContext(ctx, "business/UpdateDocPermissions Failed to get updated doc for OpenSearch sync: doc is nil")
			return
		}

		// And for its AI search entries, the doc's and its comments': a
		// person it's no longer shared with stops finding it there too.
		if _, err := ai.SetDocAccess(context.Background(), []ai.DocAccess{ai.DocAccessOf(input.DocId, updatedDoc)}); err != nil {
			helpers.LogErrorWithContext(ctx, "business/UpdateDocPermissions Failed to write the doc's sharing to the AI index err: %+v", err)
		}
	})

	return nil
}

// SearchUsersForDoc is the people a doc can be shared with. On the public demo
// its shared visitor (viewerEmail) finds only the demo's own people.
func SearchUsersForDoc(ctx context.Context, userDgraphUUID string, viewerEmail string, searchText string) (usersList []*dgraphStruct.DgraphUser, err error) {
	dgraphUsers, err := userDomain.GetUserListWithSearchText(ctx, userDgraphUUID, searchText)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/SearchUsersForDoc Failed to get users list err: %+v", err)
		return nil, err
	}

	return userDomain.DemoVisibleUsers(viewerEmail, dgraphUsers), nil
}

// GetDocPermissions reads who a doc is shared with, for the share dialog. The
// caller has decided already that the person may manage it (the controller's
// CanEdit, on the doc it read for its deletion): this read the doc a second
// time for the same check.
func GetDocPermissions(ctx context.Context, docUUID string) (dgraphDoc *dgraphStruct.DgraphDoc, err error) {
	return domain.GetDocPermissions(ctx, docUUID)
}

// contentAddedBy is the doc's creator as a comment records it, or none when
// the doc has no creator any more (a removed person's doc): that was a nil
// dereference, and the comment answered 500.
func contentAddedBy(doc *dgraphStruct.DgraphDoc) *dgraphStruct.DgraphUser {
	if doc == nil || doc.CreatedBy == nil || doc.CreatedBy.Uid == "" {
		return nil
	}
	return &dgraphStruct.DgraphUser{Uid: doc.CreatedBy.Uid}
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
				ContentAddedBy: contentAddedBy(rawDocDgraph),
				Attachments:    createTaskCommentInfoInput.Attachments,
				CreatedAt:      &currentTime,
				DeletedAt:      &zeroUnixTime,
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
		UserName:       createdByUser.UserDgraphInfo.DisplayName(),
		UserProfileKey: createdByUser.UserDgraphInfo.ProfileKey,
		Attachments:    createTaskCommentInfoInput.Attachments,
	}

	go mqttBusiness.PublishDocComment(&mqttDocComment, rawDocDgraph.Uuid)

	go PublishDocCommentActivity(commentUUID.String(), createTaskCommentInfoInput.CommentBody, currentTime, rawDocDgraph, createdByUser.UserDgraphInfo, mentionsDgraphUsersList)

	return
}

// Seams for tests: who of the people named may read a doc now (the Access
// Resolver's rule from the doc's side, docReaders.go), and where a live
// activity item goes.
var (
	commentActivityReaders = Readers
	commentActivityPublish = activityBusiness.PublishActivityToUser
)

// PublishDocCommentActivity puts a new comment in the activity of whoever it
// is for, live, as their feed lists it when it loads: the doc's maker, as a
// comment on what they made, unless they wrote it; and anyone it mentions, as
// a mention. Each only while they may read the doc, by the Access Resolver's
// rule, asked once (the feed keeps only those items too, business/Activity
// keepReadable): a maker it was taken from, or someone mentioned who can't
// open it, hears nothing, and with no answer nobody does. The maker was read
// without a check (a doc with no maker took the goroutine down), and a
// mention reached nobody live. Push and email are business/Comment's
// (sendNewDocCommentNotification).
func PublishDocCommentActivity(commentUUID string, commentBody string, currentTime time.Time, rawDocDgraph *dgraphStruct.DgraphDoc, author dgraphStruct.DgraphUser, mentions []*dgraphStruct.DgraphUser) {
	if rawDocDgraph == nil {
		return
	}
	ask := []string{}
	if rawDocDgraph.CreatedBy != nil {
		ask = append(ask, rawDocDgraph.CreatedBy.Uuid)
	}
	for _, m := range mentions {
		if m != nil {
			ask = append(ask, m.Uuid)
		}
	}
	can, err := commentActivityReaders(context.Background(), rawDocDgraph.Uuid, ask)
	if err != nil {
		helpers.LogErrorWithContext(context.Background(), "business/PublishDocCommentActivity: who may read doc %s couldn't be decided, so nobody hears of comment %s live: %+v", rawDocDgraph.Uuid, commentUUID, err)
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
	if owner != "" && owner != author.Uuid && can[owner] {
		commentActivityPublish(owner, &dgraphModels.UnifiedActivityItem{
			ActivityType: mqttStruct.MESSAGE_ACTIVITY_COMMENT,
			Time:         time.Now().UTC().Format(time.RFC3339Nano),
			Comment:      comment(),
		})
	}
	told := map[string]bool{author.Uuid: true, owner: true, "": true}
	for _, m := range mentions {
		if m == nil || told[m.Uuid] || !can[m.Uuid] {
			continue
		}
		told[m.Uuid] = true
		commentActivityPublish(m.Uuid, &dgraphModels.UnifiedActivityItem{
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

// GetSystemDocByUUID is a doc as the collaboration service opens it: with its
// saved Yjs state, read for no one in particular.
func GetSystemDocByUUID(ctx context.Context, docUUID string) (*dgraphStruct.DgraphDoc, error) {
	return domain.GetCollabDocByUUID(ctx, docUUID)
}
