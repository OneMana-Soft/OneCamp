package business

import (
	"context"
	"fmt"
	"time"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	userFCMtokenBusiness "github.com/akashc777/OneCamp/business/UserFCMToken"
	domain "github.com/akashc777/OneCamp/domain/Comment"
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
	openSearchComment := &openSearchStruct.OpenSearchComment{
		Uuid:                  commentUUID.String(),
		CommentBody:           plainText,
		CommentByUserUuid:     userInfo.UserDgraphInfo.Uuid,
		CommentByUserFullName: userInfo.UserDgraphInfo.DisplayName(),
		CommentDocUuid:        rawDgraphDocInfo.Uuid,
		CommentDocTitle:       rawDgraphDocInfo.Title,
		CommentDocPrivate:     *rawDgraphDocInfo.IsPrivate,
		CommentCreatedAt:      createdTime.Unix(),
		CommentDeletedAt:      helpers.Int64Pointer(-1),
	}

	for _, u := range dgraphDoc.ReadingUser {
		openSearchComment.CommentDocReadingUsers = append(openSearchComment.CommentDocReadingUsers, u.Uuid)
	}
	for _, u := range dgraphDoc.EditingUser {
		openSearchComment.CommentDocEditingUsers = append(openSearchComment.CommentDocEditingUsers, u.Uuid)
	}
	for _, u := range dgraphDoc.CommentingUser {
		openSearchComment.CommentDocCommentingUsers = append(openSearchComment.CommentDocCommentingUsers, u.Uuid)
	}
	if dgraphDoc.CreatedBy != nil {
		openSearchComment.CommentDocCreatedByUserUuid = dgraphDoc.CreatedBy.Uuid
	}
	if userInfo.UserDgraphInfo.ProfileKey != nil {
		openSearchComment.CommentByProfile = userInfo.UserDgraphInfo.ProfileKey
	}

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
	docCreatedBy := ""
	if dgraphDoc.CreatedBy != nil {
		docCreatedBy = dgraphDoc.CreatedBy.Uuid
	}
	ai.EmbedDocCommentContent(plainText, commentUUID.String(), userInfo.UserDgraphInfo.Uuid, userInfo.UserDgraphInfo.DisplayName(),
		rawDgraphDocInfo.Uuid, *rawDgraphDocInfo.IsPrivate, docCreatedBy, readUUIDs, editUUIDs, commentUUIDs)

	notificationTitle := fmt.Sprintf("Comment - %+v", userInfo.UserDgraphInfo.DisplayName())
	notificationBody := plainText
	go sendNewDocCommentNotification(notificationTitle, notificationBody, commentUUID.String(), rawDgraphDocInfo, &userInfo.UserDgraphInfo)

	return
}

func sendNewDocCommentNotification(title string, body string, commentUUID string, dgraphDoc *dgraphStruct.DgraphDoc, userDgraph *dgraphStruct.DgraphUser) {

	ctx := context.Background()

	if dgraphDoc.CreatedBy.Uuid == userDgraph.Uuid {
		return
	}

	tokens, err := userFCMtokenBusiness.GetFCMTokenByUserId(ctx, userDgraph.Uuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/sendNewDocCommentNotification Failed to gets user's fcm token err: %+v",
			err)
		return
	}

	pushData := make(map[string]string)

	pushData[firebaseInit.FIREBASE_PUSH_DATA_TYPE] = firebaseInit.FIREBASE_PUSH_DATA_TYPE_DOC_COMMENT
	pushData[firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID] = dgraphDoc.Uuid
	pushData[firebaseInit.FIREBASE_PUSH_DATA_TITLE] = title
	pushData[firebaseInit.FIREBASE_PUSH_DATA_BODY] = body
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
				"business/sendNewDocCommentNotification Failed to send push notification err: %+v",
				err)
			return
		}
	}

	// Email fan-out for doc comments. Recipients are the doc owner +
	// reading/editing/commenting permission lists; we collect them here
	// to keep the policy in one place.
	emailRecipients := collectDocPermissionUUIDs(dgraphDoc, userDgraph.Uuid)
	docTitle := dgraphDoc.Title
	if docTitle == "" {
		docTitle = "a document"
	}
	notificationBusiness.DispatchDocComment(
		userDgraph.Uuid,
		userDgraph.DisplayName(),
		userBusiness.GetSignedProfileURL(ctx, userDgraph.ProfileKey),
		dgraphDoc.Uuid,
		docTitle,
		body,
		commentUUID,
		emailRecipients,
	)
}

// collectDocPermissionUUIDs returns the union of the doc creator + reading/
// editing/commenting permission lists, minus the actor. Used for email fan-out.
func collectDocPermissionUUIDs(d *dgraphStruct.DgraphDoc, actorUUID string) []string {
	if d == nil {
		return nil
	}
	seen := map[string]struct{}{}
	add := func(u string) {
		if u == "" || u == actorUUID {
			return
		}
		seen[u] = struct{}{}
	}
	if d.CreatedBy != nil {
		add(d.CreatedBy.Uuid)
	}
	for _, u := range d.ReadingUser {
		add(u.Uuid)
	}
	for _, u := range d.EditingUser {
		add(u.Uuid)
	}
	for _, u := range d.CommentingUser {
		add(u.Uuid)
	}
	out := make([]string, 0, len(seen))
	for u := range seen {
		out = append(out, u)
	}
	return out
}

func CreateCommentInAPost(ctx context.Context, dgraphPost *dgraphStruct.DgraphPost, userInfo *userModels.UserInfo, commentUUID uuid.UUID, createdTime time.Time, rawDgraphPostInfo *dgraphStruct.DgraphPost, mentionList []*dgraphStruct.DgraphUser) (commentUid string, err error) {

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

	notificationTitle := fmt.Sprintf("Comment - %+v", userInfo.UserDgraphInfo.DisplayName())
	notificationBody := plainText
	go sendNewPostCommentNotification(notificationTitle, notificationBody, commentUUID.String(), mentionList, rawDgraphPostInfo, &userInfo.UserDgraphInfo)

	return
}

func sendNewPostCommentNotification(title string, body string, commentUUID string, mentionsDgraphUsersList []*dgraphStruct.DgraphUser, dgraphPost *dgraphStruct.DgraphPost, userDgraph *dgraphStruct.DgraphUser) {

	mentionsUUIDList := []string{}
	membersUUIDList := []string{}

	ctx := context.Background()

	for _, mention := range mentionsDgraphUsersList {
		mentionsUUIDList = append(mentionsUUIDList, mention.Uuid)
	}

	dgraphPostComment, err := domain.GetDgraphPostCommentsInfoByUUID(ctx, dgraphPost.Uuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/sendNewPostCommentNotification Failed to gets post's comment err: %+v",
			err)
		return
	}

	for _, comment := range dgraphPostComment.Comments {
		if comment.CommentBy.Uuid == userDgraph.Uuid {
			continue
		}
		membersUUIDList = append(membersUUIDList, comment.CommentBy.Uuid)
	}

	if userDgraph.Uuid != dgraphPost.PostBy.Uuid {
		mentionsUUIDList = append(mentionsUUIDList, dgraphPost.PostBy.Uuid)
	}

	tokens, err := userFCMtokenBusiness.GetFCMTokensForNewChannelActivityExcludingUserId(ctx, userDgraph.Uuid, dgraphPost.Channel.Uuid, membersUUIDList, mentionsUUIDList)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/sendNewPostCommentNotification Failed to gets user's fcm token err: %+v",
			err)
		return
	}

	pushData := make(map[string]string)

	pushData[firebaseInit.FIREBASE_PUSH_DATA_TYPE] = firebaseInit.FIREBASE_PUSH_DATA_TYPE_POST_COMMENT
	pushData[firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID] = dgraphPost.Channel.Uuid
	pushData[firebaseInit.FIREBASE_PUSH_DATA_TITLE] = title
	pushData[firebaseInit.FIREBASE_PUSH_DATA_BODY] = body
	pushData[firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID] = dgraphPost.Uuid
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

	// Email fan-out for post comments. Same recipient pool as FCM:
	// previous commenters + mentions + post author.
	emailRecipients := append([]string{}, membersUUIDList...)
	emailRecipients = append(emailRecipients, mentionsUUIDList...)
	notificationBusiness.DispatchPostComment(
		userDgraph.Uuid,
		userDgraph.DisplayName(),
		userBusiness.GetSignedProfileURL(ctx, userDgraph.ProfileKey),
		dgraphPost.Channel.Uuid,
		dgraphPost.Uuid,
		body,
		commentUUID,
		emailRecipients,
	)
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
	go ai.StoreEmbeddingAsync(ai.EmbeddingDoc{
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

	go domain.DeleteCommentWithAttachmentsInOpenSearch(opensearchComment, rawDgraphComment.Attachments)
	go globalSearchDomain.SyncCascadingDeletionInOpenSearch(
		[]string{"attachment_comment_id"},
		commentUUID.String(),
		currentTime.Unix(),
		[]string{"attachments"},
		"cascade",
	)
	// Cascade soft-delete to AI embedding for this comment
	go globalSearchDomain.SyncCascadingDeletionInOpenSearch(
		[]string{"content_uuid"},
		commentUUID.String(),
		currentTime.Unix(),
		[]string{"ai_embeddings"},
		"cascade",
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

	notificationTitle := fmt.Sprintf("Comment - %+v", userInfo.UserDgraphInfo.DisplayName())
	notificationBody := plainText

	go sendNewChatCommentNotification(notificationTitle, notificationBody, userInfo.UserDgraphInfo.Uuid, dgraphChat.Comments[0].ChatGroupingId, mentionsDgraphUsersList, chatDgraphInfo.Uuid, userInfo.UserDgraphInfo.DisplayName(), userInfo.UserDgraphInfo.ProfileKey)

	return
}

func sendNewChatCommentNotification(title string, body string, userId string, grpId string, mentions []*dgraphStruct.DgraphUser, chatUUID string, username string, profileKey *string) {

	ctx := context.Background()

	var mentionUUDs []string

	for _, m := range mentions {
		mentionUUDs = append(mentionUUDs, m.Uuid)
	}

	tokens, err := userFCMtokenBusiness.GetFCMTokensForNewChatActivityExcludingUserId(ctx, userId, grpId, mentionUUDs)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/sendNewChatCommentNotification Failed to get user's fcm token err: %+v",
			err)
		return
	}

	pushData := make(map[string]string)

	pushData[firebaseInit.FIREBASE_PUSH_DATA_TYPE] = firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHAT_COMMENT
	// type_id is the CHAT GROUPING id, matching chat / chat_reaction /
	// chat_comment_reaction. It used to be the comment author's uuid, which the
	// clients then fed through the same "is this a DM pair or a group id?" test as
	// every other chat notification — so a comment in a GROUP chat resolved to a
	// one-to-one DM with the author instead of the group thread. grpId was already
	// a parameter here and only used for the token lookup; sending it keeps the
	// whole chat family on one rule instead of teaching each client an exception.
	pushData[firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID] = grpId
	pushData[firebaseInit.FIREBASE_PUSH_DATA_TITLE] = title
	pushData[firebaseInit.FIREBASE_PUSH_DATA_BODY] = body
	pushData[firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID] = chatUUID
	pushData[firebaseInit.FIREBASE_PUSH_DATA_USERNAME] = username
	pushData[firebaseInit.FIREBASE_PUSH_DATA_ICON] = userBusiness.GetSignedProfileURL(ctx, profileKey)

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
				"business/sendNewChatCommentNotification Failed to send push notification err: %+v",
				err)
			return
		}
	}

}
