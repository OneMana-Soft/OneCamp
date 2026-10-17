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
	taskDomain "github.com/akashc777/OneCamp/domain/Task"
	userProjectNotificationDomain "github.com/akashc777/OneCamp/domain/UserProjectNotification"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
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

	/* AI call omitted in v1 */

	notificationTitle := fmt.Sprintf("Comment - %+v", userInfo.UserDgraphInfo.DisplayName())
	notificationBody := plainText
	go sendNewTaskCommentNotification(notificationTitle, notificationBody, commentUUID.String(), mentionList, rawDgraphTaskInfo, &userInfo.UserDgraphInfo)

	return

}

// Seams for tests: a task's thread, who hears of its comments by their
// setting, who may read it, and how they're told.
var (
	taskCommentThread   = domain.GetDgraphTaskCommentUUIDsInfo
	taskCommentAudience = userProjectNotificationDomain.GetTaskCommentAudience
	taskCommentReaders  = taskDomain.Readers
	taskCommentDeliver  = notificationBusiness.Deliver
	taskCommentEmail    = notificationBusiness.DispatchTaskComment
	taskCommentIcon     = userBusiness.GetSignedProfileURL
)

// sendNewTaskCommentNotification tells of a comment on a task: a push, with the
// desktop app's notice beside it, and an email.
func sendNewTaskCommentNotification(title string, body string, commentUUID string, mentionsDgraphUsersList []*dgraphStruct.DgraphUser, dgraphTask *dgraphStruct.DgraphTask, userDgraph *dgraphStruct.DgraphUser) {
	ctx := context.Background()
	if dgraphTask == nil || dgraphTask.Project == nil || userDgraph == nil {
		return
	}

	// Whom it's for: whom it mentions, and whom the task is given to.
	forThem := []string{}
	for _, mention := range mentionsDgraphUsersList {
		if mention != nil && mention.Uuid != "" {
			forThem = append(forThem, mention.Uuid)
		}
	}
	if dgraphTask.Assignee != nil && dgraphTask.Assignee.Uuid != "" && userDgraph.Uuid != dgraphTask.Assignee.Uuid {
		forThem = append(forThem, dgraphTask.Assignee.Uuid)
	}

	// The thread: whoever commented on the task before, but its writer.
	dgraphTaskComment, err := taskCommentThread(ctx, dgraphTask.Uuid)
	if err != nil || dgraphTaskComment == nil {
		helpers.LogErrorWithContext(ctx,
			"business/sendNewTaskCommentNotification Failed to get task comment err: %+v",
			err)
		return
	}
	thread := []string{}
	for _, comment := range dgraphTaskComment.Comments {
		if comment == nil || comment.CommentBy == nil || comment.CommentBy.Uuid == "" || comment.CommentBy.Uuid == userDgraph.Uuid {
			continue
		}
		thread = append(thread, comment.CommentBy.Uuid)
	}
	icon := taskCommentIcon(ctx, userDgraph.ProfileKey)

	// The push, as every push goes (Deliver): through the push gate, with the
	// desktop app's notice beside it. It looked devices up for itself, so a
	// desktop-only reader heard of no task comment.
	if told := taskCommentPushedTo(ctx, dgraphTask, userDgraph.Uuid, thread, forThem); len(told) > 0 {
		taskCommentDeliver(ctx, told, map[string]string{
			firebaseInit.FIREBASE_PUSH_DATA_TYPE:      firebaseInit.FIREBASE_PUSH_DATA_TYPE_TASK_COMMENT,
			firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID:   dgraphTask.Project.Uuid,
			firebaseInit.FIREBASE_PUSH_DATA_TITLE:     title,
			firebaseInit.FIREBASE_PUSH_DATA_BODY:      body,
			firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID: dgraphTask.Uuid,
			firebaseInit.FIREBASE_PUSH_DATA_USERNAME:  userDgraph.DisplayName(),
			firebaseInit.FIREBASE_PUSH_DATA_ICON:      icon,
		}, firebaseInit.FIREBASE_PUSH_DATA_TYPE_TASK_COMMENT+":"+dgraphTask.Uuid+":"+commentUUID)
	}

	// Email, whatever became of the push, to the thread and whom it's for: the
	// dispatcher keeps it to who may read the task, by their email settings.
	emailRecipients := append(append([]string{}, thread...), forThem...)
	taskCommentEmail(userDgraph.Uuid, userDgraph.DisplayName(), icon, dgraphTask.Uuid, dgraphTask.Name, body, commentUUID, emailRecipients)
}

// taskCommentPushedTo is who is pushed a comment on the task: those of its
// project's people who hear of it by their setting (GetTaskCommentAudience)
// and may read the task now. A setting outlives someone's place in the
// project (taking someone off a team takes them off its projects and leaves
// their settings), so with no answer of who may read it, nobody is pushed.
func taskCommentPushedTo(ctx context.Context, task *dgraphStruct.DgraphTask, writerUUID string, thread, forThem []string) []string {
	audience, err := taskCommentAudience(ctx, task.Project.Uuid, writerUUID, thread, forThem)
	if err != nil || len(audience) == 0 {
		return nil
	}
	readers, err := taskCommentReaders(ctx, task.Uuid, audience)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/sendNewTaskCommentNotification could not check who may read task %s, pushing nobody: %+v", task.Uuid, err)
		return nil
	}
	told := make([]string, 0, len(audience))
	for _, id := range audience {
		if readers[id] {
			told = append(told, id)
		}
	}
	return told
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

	// embed comment for AI Second Brain (async) — doc comment inherits doc permissions
	var readUUIDs, editUUIDs, commentUUIDs []string
	for _, u := range dgraphDoc.ReadingUser {
		readUUIDs = append(readUUIDs, u.Uuid)
	}
	for _, u := range dgraphDoc.EditingUser {
		editUUIDs = append(editUUIDs, u.Uuid)
	}
	for _, u := range dgraphDoc.CommentingUser {
		commentUUIDs = append(commentUUIDs, u.Uuid)
	}
	/* AI call omitted in v1 */

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
	/* AI call omitted in v1 */

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

	// Re-embed updated comment text for AI Second Brain (upsert by content_uuid)
	/* AI call omitted in v1 */

	// Reconcile any workspace-memory items captured from this comment:
	// refresh the stored snapshot to the edited text, or drop it if the
	// edit emptied the comment. (Mirrors UpdatePost/UpdateChat.)
	/* AI call omitted in v1 */

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
	/* AI call omitted in v1 */

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
	/* AI call omitted in v1 */

	// Who hears of it, and how, is the chat's to decide (business/Chat
	// notifyChatThreadReply): one audience for push, notice, Activity and email.

	return
}

// ChatThreadRepliers is who has replied in the thread of the DM or group
// message chatUUID, by user id.
func ChatThreadRepliers(ctx context.Context, chatUUID string) ([]string, error) {
	return domain.GetChatThreadRepliers(ctx, chatUUID)
}
