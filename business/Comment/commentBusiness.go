package business

import (
	"context"
	"fmt"
	"slices"
	"time"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	userFCMtokenBusiness "github.com/akashc777/OneCamp/business/UserFCMToken"
	domain "github.com/akashc777/OneCamp/domain/Comment"
	docDomain "github.com/akashc777/OneCamp/domain/Doc"
	globalSearchDomain "github.com/akashc777/OneCamp/domain/GlobalSearch"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

func CreateCommentInTask(ctx context.Context, dgraphTask *dgraphStruct.DgraphTask, userInfo *userModels.UserInfo, commentUUID uuid.UUID, createdTime time.Time, rawDgraphTaskInfo *dgraphStruct.DgraphTask, mentionList []*dgraphStruct.DgraphUser) (commentUid string, err error) {

	err = domain.CreateComment(ctx, commentUUID, userInfo.UserPostgresInfo.Id, createdTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateCommentInTask Failed to create new comment err: %+v",
			err)
		return
	}

	commentUid, err = domain.CreateDgraphCommentInATask(ctx, dgraphTask, commentUUID.String())

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateCommentInTask Failed to create in post comment err: %+v",
			err)
		return
	}

	// add to openSearch
	plainText := helpers.HTMLToPlainText(dgraphTask.Comments[0].Text)
	openSearchComment := &openSearchStruct.OpenSearchComment{
		Uuid:                  commentUUID.String(),
		CommentBody:           plainText,
		CommentByUserUuid:     userInfo.UserDgraphInfo.Uuid,
		CommentProjectUuid:    rawDgraphTaskInfo.Project.Uuid,
		CommentProjectName:    rawDgraphTaskInfo.Project.Name,
		CommentTaskUuid:       rawDgraphTaskInfo.Uuid,
		CommentCreatedAt:      createdTime.Unix(),
		CommentByUserFullName: userInfo.UserDgraphInfo.DisplayName(),
		CommentByProfile:      userInfo.UserDgraphInfo.ProfileKey,
		CommentDeletedAt:      nil,
	}

	go domain.CreateTaskCommentWithAttachmentsInOpenSearch(openSearchComment, dgraphTask.Comments[0].Attachments)

	// embed comment for AI Second Brain (async) — task comment inherits project permission
	teamUUID := ""
	if rawDgraphTaskInfo.Project != nil && rawDgraphTaskInfo.Project.Team != nil {
		teamUUID = rawDgraphTaskInfo.Project.Team.Uuid
	}
	ai.EmbedTaskCommentContent(plainText, commentUUID.String(), userInfo.UserDgraphInfo.Uuid, userInfo.UserDgraphInfo.DisplayName(),
		rawDgraphTaskInfo.Project.Uuid, rawDgraphTaskInfo.Uuid, teamUUID)

	notificationTitle := fmt.Sprintf("Comment - %+v", userInfo.UserDgraphInfo.DisplayName())
	notificationBody := plainText
	go sendNewTaskCommentNotification(notificationTitle, notificationBody, commentUUID.String(), mentionList, rawDgraphTaskInfo, &userInfo.UserDgraphInfo)

	return

}

func sendNewTaskCommentNotification(title string, body string, commentUUID string, mentionsDgraphUsersList []*dgraphStruct.DgraphUser, dgraphTask *dgraphStruct.DgraphTask, userDgraph *dgraphStruct.DgraphUser) {

	mentionsUUIDList := []string{}
	membersUUIDList := []string{}

	ctx := context.Background()

	for _, mention := range mentionsDgraphUsersList {
		mentionsUUIDList = append(mentionsUUIDList, mention.Uuid)
	}

	dgraphTaskComment, err := domain.GetDgraphTaskCommentUUIDsInfo(ctx, dgraphTask.Uuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/sendNewTaskCommentNotification Failed to get task comment err: %+v",
			err)
		return
	}

	for _, comment := range dgraphTaskComment.Comments {
		if comment.CommentBy.Uuid == userDgraph.Uuid {
			continue
		}
		membersUUIDList = append(membersUUIDList, comment.CommentBy.Uuid)
	}

	if dgraphTask.Assignee != nil && userDgraph.Uuid != dgraphTask.Assignee.Uuid {
		mentionsUUIDList = append(mentionsUUIDList, dgraphTask.Assignee.Uuid)
	}

	tokens, err := userFCMtokenBusiness.GetFCMTokensForNewProjectActivityExcludingUserId(ctx, userDgraph.Uuid, dgraphTask.Project.Uuid, membersUUIDList, mentionsUUIDList)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/sendNewTaskCommentNotification Failed to gets user's fcm token err: %+v",
			err)
		return
	}

	pushData := make(map[string]string)

	pushData[firebaseInit.FIREBASE_PUSH_DATA_TYPE] = firebaseInit.FIREBASE_PUSH_DATA_TYPE_TASK_COMMENT
	pushData[firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID] = dgraphTask.Project.Uuid
	pushData[firebaseInit.FIREBASE_PUSH_DATA_TITLE] = title
	pushData[firebaseInit.FIREBASE_PUSH_DATA_BODY] = body
	pushData[firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID] = dgraphTask.Uuid
	pushData[firebaseInit.FIREBASE_PUSH_DATA_USERNAME] = userDgraph.DisplayName()
	pushData[firebaseInit.FIREBASE_PUSH_DATA_ICON] = userBusiness.GetSignedProfileURL(ctx, userDgraph.ProfileKey)

	// Send notifications in batches of 500 tokens
	batchSize := 500
	for i := 0; i < len(tokens); i += batchSize {
		end := i + batchSize
		if end > len(tokens) {
			end = len(tokens)
		}
		tokenBatch := tokens[i:end]

		err = firebaseInit.FirebaseApp.MultiCastPush(ctx, pushData, tokenBatch)
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"business/sendNewPostCommentNotification Failed to send push notification err: %+v",
				err)
			return
		}
	}

	// Email fan-out for task comments. Recipients: previous commenters
	// (membersUUIDList), mentions, and the task assignee (already merged
	// into mentionsUUIDList above).
	emailRecipients := append([]string{}, membersUUIDList...)
	emailRecipients = append(emailRecipients, mentionsUUIDList...)
	taskName := dgraphTask.Name
	notificationBusiness.DispatchTaskComment(
		userDgraph.Uuid,
		userDgraph.DisplayName(),
		userBusiness.GetSignedProfileURL(ctx, userDgraph.ProfileKey),
		dgraphTask.Uuid,
		taskName,
		body,
		commentUUID,
		emailRecipients,
	)
}

func CreateCommentInADoc(ctx context.Context, dgraphDoc *dgraphStruct.DgraphDoc, userInfo *userModels.UserInfo, commentUUID uuid.UUID, createdTime time.Time, rawDgraphDocInfo *dgraphStruct.DgraphDoc, mentionList []*dgraphStruct.DgraphUser) (commentUid string, err error) {

	err = domain.CreateComment(ctx, commentUUID, userInfo.UserPostgresInfo.Id, createdTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateCommentInADoc Failed to create new comment err: %+v",
			err)
		return
	}

	commentUid, err = domain.CreateDgraphCommentInADoc(ctx, dgraphDoc, commentUUID.String())

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateCommentInADoc Failed to create in doc comment err: %+v",
			err)
		return
	}

	// add to openSearch
	plainText := helpers.HTMLToPlainText(dgraphDoc.Comments[0].Text)
	openSearchComment := docCommentEntry(rawDgraphDocInfo, commentUUID.String(), plainText, &userInfo.UserDgraphInfo, createdTime)

	go domain.CreateDocCommentWithAttachmentsInOpenSearch(openSearchComment, dgraphDoc.Comments[0].Attachments)

	// embed comment for AI Second Brain (async) — doc comment inherits doc
	// permissions. They are read from the doc: dgraphDoc is the mutation that
	// adds the comment and carries none of them, so the entry used to say the
	// doc was shared with nobody and made by no one, and AI search found a
	// comment on a private doc for no one, its owner included.
	access := rawDgraphDocInfo
	if perms, perr := docDomain.GetDocPermissions(ctx, rawDgraphDocInfo.Uuid); perr == nil && perms != nil {
		access = perms
	} else {
		helpers.LogErrorWithContext(ctx, "business/CreateCommentInADoc Failed to read the doc's sharing for AI search err: %+v", perr)
	}
	uuidsOf := func(users []*dgraphStruct.DgraphUser) []string {
		out := []string{}
		for _, u := range users {
			if u != nil {
				out = append(out, u.Uuid)
			}
		}
		return out
	}
	docCreatedBy := ""
	if access.CreatedBy != nil {
		docCreatedBy = access.CreatedBy.Uuid
	}
	// A doc that never had its privacy set is open to everyone signed in, as
	// reading it is (business/Doc CouldRead) and as the comment's search entry
	// says (docCommentEntry). Reading the flag through a nil pointer took the
	// request down after the comment was written, before anyone was told.
	docPrivate := rawDgraphDocInfo.IsPrivate != nil && *rawDgraphDocInfo.IsPrivate
	ai.EmbedDocCommentContent(plainText, commentUUID.String(), userInfo.UserDgraphInfo.Uuid, userInfo.UserDgraphInfo.DisplayName(),
		rawDgraphDocInfo.Uuid, docPrivate, docCreatedBy,
		uuidsOf(access.ReadingUser), uuidsOf(access.EditingUser), uuidsOf(access.CommentingUser))

	notificationTitle := fmt.Sprintf("Comment - %+v", userInfo.UserDgraphInfo.DisplayName())
	notificationBody := plainText
	author := userInfo.UserDgraphInfo
	helpers.GoSafeNamed("doc.comment-notify", func() {
		sendNewDocCommentNotification(notificationTitle, notificationBody, commentUUID.String(), rawDgraphDocInfo, &author, mentionList)
	})

	return
}

// docCommentEntry is a new doc comment's entry in the comments index. It is
// live from the start: it was written as deleted (-1) and global search leaves
// out anything with a deletion time, so a comment on a doc could be found only
// once someone edited it. And it carries its doc's privacy, sharing and maker
// from the doc as read for the comment (doc), not from the mutation that adds
// the comment, which holds none of them: a comment on a private doc was found
// by nobody, its owner included, until the doc next changed. Pure.
func docCommentEntry(doc *dgraphStruct.DgraphDoc, commentUUID, plainText string, by *dgraphStruct.DgraphUser, createdTime time.Time) *openSearchStruct.OpenSearchComment {
	uuids := func(users []*dgraphStruct.DgraphUser) []string {
		out := []string{}
		for _, u := range users {
			if u != nil && u.Uuid != "" {
				out = append(out, u.Uuid)
			}
		}
		return out
	}
	private := doc.IsPrivate != nil && *doc.IsPrivate
	e := &openSearchStruct.OpenSearchComment{
		Uuid:                      commentUUID,
		CommentBody:               plainText,
		CommentByUserUuid:         by.Uuid,
		CommentByUserFullName:     by.DisplayName(),
		CommentByProfile:          by.ProfileKey,
		CommentDocUuid:            doc.Uuid,
		CommentDocTitle:           doc.Title,
		CommentDocPrivate:         &private,
		CommentDocReadingUsers:    uuids(doc.ReadingUser),
		CommentDocEditingUsers:    uuids(doc.EditingUser),
		CommentDocCommentingUsers: uuids(doc.CommentingUser),
		CommentCreatedAt:          createdTime.Unix(),
		CommentDeletedAt:          nil,
	}
	if doc.CreatedBy != nil {
		e.CommentDocCreatedByUserUuid = doc.CreatedBy.Uuid
	}
	return e
}

// Telling people about a member's new comment on a doc.
//
// It went wrong four ways: the push went to the commenter's own devices (it
// looked up the author's tokens); a push that failed, or a token lookup that
// did, returned before the email was sent; a comment by the doc's owner told
// nobody at all; and it read the author of nothing for a nil maker. Now:
//
//   - who: the people taking part in the doc (its owner, its editors, and
//     whoever commented on it before) and anyone the comment mentions, each
//     once, never its author, and only people who may read the doc now (a
//     comment is never sent to someone a private doc was taken away from);
//   - push and email are independent: either failing leaves the other;
//   - a push is held for anyone who paused notifications, is in quiet hours
//     or in focus time (the token lookup's gate), and an email is held or
//     deferred by their own settings. Nothing here goes through a pause,
//     whoever wrote the comment: only a person's own "notify anyway" does,
//     and an agent's never does (business/Notification urgent.go).

// Seams for tests: where participants, devices, pushes and emails go.
var (
	docCommentParticipants = docDomain.GetDocCommentParticipants
	docCommentDevices      = userFCMtokenBusiness.GetFCMTokenByListOfUserId
	docCommentPush         = func(ctx context.Context, data map[string]string, tokens []string) error {
		return firebaseInit.FirebaseApp.MultiCastPush(ctx, data, tokens)
	}
	docCommentEmail = notificationBusiness.DispatchDocComment
	docCommentIcon  = userBusiness.GetSignedProfileURL
)

func sendNewDocCommentNotification(title string, body string, commentUUID string, dgraphDoc *dgraphStruct.DgraphDoc, author *dgraphStruct.DgraphUser, mentions []*dgraphStruct.DgraphUser) {
	ctx := context.Background()
	if dgraphDoc == nil || author == nil {
		return
	}
	// Who takes part, read afresh: a comment can't tell anyone the doc can't
	// be shown to, so with no answer it tells nobody.
	doc, err := docCommentParticipants(ctx, dgraphDoc.Uuid)
	if err != nil || doc == nil {
		helpers.LogErrorWithContext(ctx, "business/sendNewDocCommentNotification Failed to read who takes part in doc %s err: %+v", dgraphDoc.Uuid, err)
		return
	}
	mentioned := make([]string, 0, len(mentions))
	for _, m := range mentions {
		if m != nil {
			mentioned = append(mentioned, m.Uuid)
		}
	}
	recipients := docCommentRecipients(doc, mentioned, author.Uuid)
	if len(recipients) == 0 {
		return
	}
	icon := docCommentIcon(ctx, author.ProfileKey)

	// Push: to the recipients' devices, held by the lookup for anyone paused.
	if tokens, err := docCommentDevices(ctx, recipients); err != nil {
		helpers.LogErrorWithContext(ctx, "business/sendNewDocCommentNotification Failed to get the recipients' devices err: %+v", err)
	} else if len(tokens) > 0 {
		pushData := map[string]string{
			firebaseInit.FIREBASE_PUSH_DATA_TYPE:     firebaseInit.FIREBASE_PUSH_DATA_TYPE_DOC_COMMENT,
			firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID:  doc.Uuid,
			firebaseInit.FIREBASE_PUSH_DATA_TITLE:    title,
			firebaseInit.FIREBASE_PUSH_DATA_BODY:     body,
			firebaseInit.FIREBASE_PUSH_DATA_USERNAME: author.DisplayName(),
			firebaseInit.FIREBASE_PUSH_DATA_ICON:     icon,
		}
		// 500 tokens a call is Firebase's limit.
		for batch := range slices.Chunk(tokens, 500) {
			if err := docCommentPush(ctx, pushData, batch); err != nil {
				helpers.LogErrorWithContext(ctx, "business/sendNewDocCommentNotification Failed to send push notification err: %+v", err)
				break
			}
		}
	}

	// Email (and its own holds), whatever became of the push.
	docTitle := doc.Title
	if docTitle == "" {
		docTitle = "a document"
	}
	docCommentEmail(author.Uuid, author.DisplayName(), icon, doc.Uuid, docTitle, body, commentUUID, recipients)
}

// docCommentRecipients is who hears about a new comment on a doc: its owner,
// its editors, whoever commented on it before, and anyone the comment
// mentions, in that order and each once, never its author, and only people
// who may read the doc now. Its readers who haven't taken part aren't told
// of every comment. doc is read as GetDocCommentParticipants reads it. Pure.
func docCommentRecipients(doc *dgraphStruct.DgraphDoc, mentioned []string, author string) []string {
	if doc == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{author: true, "": true}
	add := func(id string) {
		if !seen[id] && docDomain.ReadableBy(doc, id) {
			out = append(out, id)
		}
		seen[id] = true
	}
	if doc.CreatedBy != nil {
		add(doc.CreatedBy.Uuid)
	}
	for _, u := range doc.EditingUser {
		if u != nil {
			add(u.Uuid)
		}
	}
	for _, c := range doc.Comments {
		if c != nil && c.CommentBy != nil {
			add(c.CommentBy.Uuid)
		}
	}
	for _, id := range mentioned {
		add(id)
	}
	return out
}

// CreateCommentInAPost writes a reply in a channel thread: Postgres, the graph,
// search and the AI's index. Who hears of it is the caller's to say
// (NotifyThreadReply), as a bulk import tells nobody.
func CreateCommentInAPost(ctx context.Context, dgraphPost *dgraphStruct.DgraphPost, userInfo *userModels.UserInfo, commentUUID uuid.UUID, createdTime time.Time, rawDgraphPostInfo *dgraphStruct.DgraphPost) (commentUid string, err error) {

	err = domain.CreateComment(ctx, commentUUID, userInfo.UserPostgresInfo.Id, createdTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateCommentOnPost Failed to create new comment err: %+v",
			err)
		return
	}

	commentUid, err = domain.CreateDgraphCommentInAPost(ctx, dgraphPost, commentUUID.String())

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateCommentInAPost Failed to create in post comment err: %+v",
			err)
		return
	}

	// add to openSearch
	plainText := helpers.HTMLToPlainText(dgraphPost.Comments[0].Text)
	openSearchComment := &openSearchStruct.OpenSearchComment{
		Uuid:                  commentUUID.String(),
		CommentBody:           plainText,
		CommentByUserUuid:     userInfo.UserDgraphInfo.Uuid,
		CommentChannelUuid:    rawDgraphPostInfo.Channel.Uuid,
		CommentChannelName:    rawDgraphPostInfo.Channel.Name,
		CommentPostUuid:       dgraphPost.Uuid,
		CommentCreatedAt:      createdTime.Unix(),
		CommentByUserFullName: userInfo.UserDgraphInfo.DisplayName(),
		CommentByProfile:      userInfo.UserDgraphInfo.ProfileKey,
		CommentDeletedAt:      nil,
	}

	go domain.CreatePostCommentWithAttachmentsInOpenSearch(openSearchComment, dgraphPost.Comments[0].Attachments)

	// embed comment for AI Second Brain (async) — post comment inherits channel permission
	ai.EmbedPostCommentContent(plainText, commentUUID.String(), userInfo.UserDgraphInfo.Uuid, userInfo.UserDgraphInfo.DisplayName(),
		rawDgraphPostInfo.Channel.Uuid, rawDgraphPostInfo.Uuid)

	return
}

func UpdateComment(ctx context.Context, dgraphComment *dgraphStruct.DgraphComment, commentUUID uuid.UUID, currentTime time.Time) (err error) {
	err = domain.UpdateCommentByUUID(ctx, commentUUID, currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/UpdateComment Failed to update comment err: %+v",
			err)
		return
	}

	err = domain.UpdateDgraphCommentAndResetMentions(ctx, dgraphComment, commentUUID.String())

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/UpdateComment Failed to update dgraph comment err: %+v",
			err)
		return
	}

	plainText := helpers.HTMLToPlainText(dgraphComment.Text)
	openSearchComment := openSearchStruct.OpenSearchComment{
		Uuid:             commentUUID.String(),
		CommentBody:      plainText,
		CommentUpdatedAt: currentTime.Unix(),
	}

	go domain.UpdateCommentInOpenSearch(openSearchComment)

	// Re-embed updated comment text for AI Second Brain (upsert by content_uuid).
	// Called in line: StoreEmbeddingAsync is asynchronous itself, and queues the
	// write behind the earlier ones to this comment's entry in the order asked.
	ai.StoreEmbeddingAsync(ai.EmbeddingDoc{
		ContentText: plainText,
		ContentType: "comment",
		ContentUUID: commentUUID.String(),
		CreatedDate: time.Now().Unix(),
	})

	// Reconcile any workspace-memory items captured from this comment:
	// refresh the stored snapshot to the edited text, or drop it if the
	// edit emptied the comment. (Mirrors UpdatePost/UpdateChat.)
	ai.InvalidateMemoryForEditedSourceAsync("comment", commentUUID.String(), plainText)

	return

}

func CreateOrUpdateDgraphCommentReaction(ctx context.Context, dgraphComment *dgraphStruct.DgraphComment) (reactionUid string, err error) {

	reactionUid, err = domain.CreateOrUpdateDgraphCommentReaction(ctx, dgraphComment)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateOrUpdateDgraphPostCommentReaction Failed to update reaction on comment in post err: %+v",
			err)
		return
	}

	return
}

func GetDgraphCommentInfoByUUID(ctx context.Context, commentUUID string) (dgraphComment *dgraphStruct.DgraphComment, err error) {
	dgraphComment, err = domain.GetDgraphCommentInfoByUUID(ctx, commentUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/UpdateComment Failed to update dgraph comment err: %+v",
			err)
		return
	}
	return
}

// GetDgraphCommentInfoForUser also says whether the user administers the
// project of the task the comment is on.
func GetDgraphCommentInfoForUser(ctx context.Context, commentUUID, userDgraphUID string) (*dgraphStruct.DgraphComment, error) {
	return domain.GetDgraphCommentInfoForUser(ctx, commentUUID, userDgraphUID)
}

func GetDgraphChatCommentInfoByUUID(ctx context.Context, commentUUID string, userDgraphUID string) (dgraphComment *dgraphStruct.DgraphComment, err error) {
	dgraphComment, err = domain.GetDgraphChatCommentInfoByUUID(ctx, commentUUID, userDgraphUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/UpdateComment Failed to get dgraph comment err: %+v",
			err)
		return
	}
	return
}

func GetDgraphTaskCommentInfoByUUID(ctx context.Context, commentUUID string, userDgraphUID string) (dgraphComment *dgraphStruct.DgraphComment, err error) {
	dgraphComment, err = domain.GetDgraphTaskCommentInfoByUUID(ctx, commentUUID, userDgraphUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphTaskCommentInfoByUUID Failed to get dgraph comment err: %+v",
			err)
		return
	}
	return
}

func GetDgraphDocCommentInfoByUUID(ctx context.Context, commentUUID string, userDraphUid string) (dgraphComment *dgraphStruct.DgraphComment, err error) {
	dgraphComment, err = domain.GetDgraphDocCommentInfoByUUID(ctx, commentUUID, userDraphUid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphDocCommentInfoByUUID Failed to get dgraph comment err: %+v",
			err)
		return
	}
	return
}

func GetDgraphPostCommentInfoByUUID(ctx context.Context, commentUUID string, userDgraphUID string) (dgraphComment *dgraphStruct.DgraphComment, err error) {
	dgraphComment, err = domain.GetDgraphPostCommentInfoByUUID(ctx, commentUUID, userDgraphUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/UpdateComment Failed to get dgraph comment err: %+v",
			err)
		return
	}
	return
}

func DeleteComment(ctx context.Context, dgraphComment *dgraphStruct.DgraphComment, currentTime time.Time, commentUUID uuid.UUID, rawDgraphComment *dgraphStruct.DgraphComment) (err error) {

	err = domain.SoftDeletePostByUUIUD(ctx, commentUUID, currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/DeleteComment Failed to soft delete post err: %+v",
			err)
		return
	}

	err = domain.UpdateDgraphCommentInAPost(ctx, dgraphComment)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/DeleteComment Failed to update post comment err: %+v",
			err)
		return
	}

	opensearchComment := &openSearchStruct.OpenSearchComment{
		Uuid:             commentUUID.String(),
		CommentDeletedAt: helpers.Int64Pointer(currentTime.Unix()),
	}

	// The comment's files and its AI entry are marked deleted by a person
	// ("user"), never "cascade": restoring the doc, post, chat or task the
	// comment was on clears what was marked "cascade", and brought a deleted
	// comment back to AI search.
	go domain.DeleteCommentWithAttachmentsInOpenSearch(opensearchComment, rawDgraphComment.Attachments)
	go globalSearchDomain.SyncCascadingDeletionInOpenSearch(
		[]string{"attachment_comment_id"},
		commentUUID.String(),
		currentTime.Unix(),
		[]string{"attachments"},
		"user",
	)
	// Cascade soft-delete to AI embedding for this comment
	go globalSearchDomain.SyncCascadingDeletionInOpenSearch(
		[]string{"content_uuid"},
		commentUUID.String(),
		currentTime.Unix(),
		[]string{"ai_embeddings"},
		"user",
	)

	// Remove any workspace-memory items a user captured FROM this comment so
	// deleted content can't linger in memory or resurface via AI/graph.
	ai.DeleteMemoryBySourceAsync("comment", commentUUID.String())

	return

}

func DeleteCommentReaction(ctx context.Context, commentDgraphUID string, reactionDgraphUID string) (err error) {
	err = domain.DeleteCommentReaction(ctx, commentDgraphUID, reactionDgraphUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/DeleteCommentReaction Failed to delete reaction on comment err: %+v",
			err)
		return
	}
	return
}

func CreateCommentInChat(ctx context.Context, dgraphChat *dgraphStruct.DgraphChat, userInfo *userModels.UserInfo, commentUUID uuid.UUID, createdTime time.Time, chatDgraphInfo *dgraphStruct.DgraphChat, mentionsDgraphUsersList []*dgraphStruct.DgraphUser) (commentUid string, err error) {

	err = domain.CreateComment(ctx, commentUUID, userInfo.UserPostgresInfo.Id, createdTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateCommentInChat Failed to create new comment err: %+v",
			err)
		return
	}

	commentUid, err = domain.CreateOrUpdateCommentInChat(ctx, dgraphChat, commentUUID.String())

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateOrUpdateCommentInChat Failed to create/update comment in chat err: %+v",
			err)
		return
	}

	plainText := helpers.HTMLToPlainText(dgraphChat.Comments[0].Text)

	var openSearchChatParticipants []*openSearchStruct.OpenSearchChatParticipants

	for _, p := range chatDgraphInfo.DM.Participants {
		openSearchChatParticipants = append(openSearchChatParticipants, &openSearchStruct.OpenSearchChatParticipants{
			Uuid:       p.Uuid,
			ProfileKey: p.ProfileKey,
			Name:       p.DisplayName(),
		})
	}

	// add to openSearch
	openSearchComment := &openSearchStruct.OpenSearchComment{
		Uuid:                    commentUUID.String(),
		CommentBody:             plainText,
		CommentByUserUuid:       userInfo.UserDgraphInfo.Uuid,
		CommentChatGrpId:        dgraphChat.Comments[0].ChatGroupingId,
		CommentChatUuid:         dgraphChat.Comments[0].Uuid,
		CommentCreatedAt:        createdTime.Unix(),
		CommentByUserFullName:   userInfo.UserDgraphInfo.DisplayName(),
		CommentByProfile:        userInfo.UserDgraphInfo.ProfileKey,
		CommentChatFromUserUuid: userInfo.UserDgraphInfo.Uuid,
		CommentChatParticipants: openSearchChatParticipants,
		CommentDeletedAt:        nil,
	}

	go domain.CreateChatCommentWithAttachmentsInOpenSearch(openSearchComment, dgraphChat.Comments[0].Attachments)

	// embed comment for AI Second Brain (async) — chat comment inherits participant permission
	var chatParticipantUUIDs []string
	for _, p := range chatDgraphInfo.DM.Participants {
		chatParticipantUUIDs = append(chatParticipantUUIDs, p.Uuid)
	}
	ai.EmbedChatCommentContent(plainText, commentUUID.String(), userInfo.UserDgraphInfo.Uuid, userInfo.UserDgraphInfo.DisplayName(),
		chatDgraphInfo.Uuid, dgraphChat.Comments[0].ChatGroupingId, userInfo.UserDgraphInfo.Uuid, "", chatParticipantUUIDs)

	// Who hears of it, and how, is the chat's to decide (business/Chat
	// notifyChatThreadReply): one audience for push, notice, Activity and email.

	return
}

// ChatThreadRepliers is who has replied in the thread of the DM or group
// message chatUUID, by user id.
func ChatThreadRepliers(ctx context.Context, chatUUID string) ([]string, error) {
	return domain.GetChatThreadRepliers(ctx, chatUUID)
}
