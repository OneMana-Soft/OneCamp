package business

import (
	"context"
	"fmt"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Post"
	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	commentBusiness "github.com/akashc777/OneCamp/business/Comment"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	channelNotificationBusiness "github.com/akashc777/OneCamp/business/UserChannelNotification"
	userFCMtokenBusiness "github.com/akashc777/OneCamp/business/UserFCMToken"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	globalSearchDomain "github.com/akashc777/OneCamp/domain/GlobalSearch"
	domain "github.com/akashc777/OneCamp/domain/Post"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	models "github.com/akashc777/OneCamp/models/postgres/Post"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

type PostPagination struct {
	Posts   []*dgraphStruct.DgraphPost `json:"posts,omitempty"`
	HasMore bool                       `json:"has_more"`
}

// replyContextSnippetLen bounds the parent snippet prepended to a reply's
// contextual embedding so a long parent can't dominate the vector.
const replyContextSnippetLen = 200

// resolveReplyParentPost validates a Discord-style inline reply target and
// returns the parent post's Dgraph node (uid + author + text, for the edge,
// the MQTT preview, and the contextual embedding), or nil when the reference is
// invalid. A reply is SAME-SURFACE: the parent must exist, be non-deleted, and
// live in the SAME channel. On any failure it returns nil so the post is still
// created as a normal message and a bad/foreign uuid never leaks a parent's
// existence or content.
func resolveReplyParentPost(ctx context.Context, replyToUUID string, channelUUID uuid.UUID, userDgraphUID string) *dgraphStruct.DgraphPost {
	parentUUID, perr := uuid.Parse(replyToUUID)
	if perr != nil {
		return nil
	}
	// Authoritative same-channel + not-deleted check via Postgres (indexed).
	parentPG, err := GetPostByUUID(ctx, parentUUID)
	if err != nil || parentPG == nil || !parentPG.DeletedAt.IsZero() || parentPG.ChannelUUID != channelUUID {
		return nil
	}
	// Resolve the Dgraph node for the edge target + preview fields.
	parentDg, derr := domain.GetDgraphPostByUUID(ctx, replyToUUID, userDgraphUID)
	if derr != nil || parentDg == nil || parentDg.Uid == "" {
		return nil
	}
	return parentDg
}

// withReplyContext prepends a compact "Replying to <author>: <snippet>" prefix
// to a reply's text for the k-NN embedding, so short replies carry the meaning
// of the message they answer. Returns text unchanged when parent is nil.
func withReplyContext(parent *dgraphStruct.DgraphPost, text string) string {
	if parent == nil {
		return text
	}
	author := ""
	if parent.PostBy != nil {
		author = parent.PostBy.DisplayName()
	}
	snippet := helpers.HTMLToPlainText(parent.Text)
	if len(snippet) > replyContextSnippetLen {
		snippet = helpers.TruncateRunes(snippet, replyContextSnippetLen)
	}
	if author == "" && snippet == "" {
		return text
	}
	return fmt.Sprintf("Replying to %s: %s\n%s", author, snippet, text)
}

func CreatePost(ctx context.Context, postInfo *adapter.InputCreateOrUpdatePostInfo, userInfo *userModels.UserInfo, mentionsDgraphUsersList []*dgraphStruct.DgraphUser, dgraphChannelInfo *dgraphStruct.DgraphChannel) (createdPostInfo *adapter.OutputCreatePostForPost, err error) {

	// create channel uuid
	postUUID := uuid.New()

	// create channel in postgres
	err = domain.CreatePost(ctx, postUUID, userInfo.UserPostgresInfo.Id, postInfo.ChannelUUID)

	// CreateTimeOrNow honours the import-time backdating override when
	// it's in effect (Slack import) so Postgres + Dgraph + OpenSearch
	// all stamp the same historical timestamp. Falls back to
	// time.Now() for native posts. See helpers.WithImportTimestamp.
	currentTime := helpers.CreateTimeOrNow(ctx)
	zeroUnixTime := time.Time{}

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreatePost Failed to create new post err: %+v",
			err)
		return
	}

	channelPost := dgraphStruct.DgraphChannel{
		DType: []string{"Channel"},
		Uid:   dgraphChannelInfo.Uid,
		Posts: []*dgraphStruct.DgraphPost{
			{
				Uid:   "uid(po)",
				DType: []string{"Post"},
			},
		},
	}

	var dgraphMentions []*dgraphStruct.DgraphUser
	for _, mentionDgraph := range mentionsDgraphUsersList {
		dgraphMentions = append(dgraphMentions, &dgraphStruct.DgraphUser{
			Uid:   mentionDgraph.Uid,
			DType: []string{"User"},
		})
	}

	for _, mediaObj := range postInfo.MediaObj {
		mediaObj.DType = []string{"Attachment"}
		mediaObj.CreatedBy = &dgraphStruct.DgraphUser{
			Uid: userInfo.UserDgraphInfo.Uid,
		}
		mediaObj.CreatedAt = &currentTime
	}

	plainText := helpers.HTMLToPlainText(postInfo.HTMLText)

	// add channel in dgraph
	dgraphPost := dgraphStruct.DgraphPost{
		Uid:      "uid(po)",
		Uuid:     postUUID.String(),
		DType:    []string{"Post"},
		Text:     postInfo.HTMLText,
		MediaObj: postInfo.MediaObj,
		Mentions: &dgraphStruct.DgraphMentions{
			DType:     []string{"Mention"},
			Mentions:  dgraphMentions,
			CreatedAt: &currentTime,
			Post: &dgraphStruct.DgraphPost{
				Uid: "uid(po)",
			},
			PostUuid: postUUID.String(),
		},
		PostBy: &dgraphStruct.DgraphUser{
			DType: []string{"User"},
			Uid:   userInfo.UserDgraphInfo.Uid,
			Posts: []*dgraphStruct.DgraphPost{
				{
					Uid:   "uid(po)",
					DType: []string{"Post"},
				},
			},
		},
		Channel:   &channelPost,
		CreatedAt: &currentTime,
		DeletedAt: &zeroUnixTime,
	}

	// Inline reply (Discord-style): when this post replies to another post in
	// the SAME channel, set the reply edge to the validated parent. Invalid /
	// foreign / deleted targets resolve to nil and are silently dropped (the
	// post is still created as a normal message) — see resolveReplyParentPost.
	var replyParentPost *dgraphStruct.DgraphPost
	if postInfo.ReplyToUUID != "" {
		replyParentPost = resolveReplyParentPost(ctx, postInfo.ReplyToUUID, postInfo.ChannelUUID, userInfo.UserDgraphInfo.Uid)
		if replyParentPost != nil {
			dgraphPost.ReplyToPost = &dgraphStruct.DgraphPost{Uid: replyParentPost.Uid}
		}
	}

	_, err = domain.CreateOrUpdateDgraphPost(ctx, &dgraphPost)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreatePost Failed to create new post in dgraph err: %+v",
			err)

		// Reverse the Postgres row written above, and return the ORIGINAL error.
		//
		// This was previously `err = domain.HardDeletePostByUUIUD(...)`, which overwrote the
		// Dgraph failure with the rollback's own result. When the rollback SUCCEEDED — the
		// normal case — err became nil and the naked return below reported success, so the
		// controller replied 200 "created post successfully!" with a null payload for a post it
		// had just deleted. The client showed the message as sent and it vanished on refresh.
		//
		// The compensation's own error is deliberately discarded: the caller's question is
		// whether the post was created, which the Dgraph error answers. A failed rollback is a
		// different problem — an orphaned row — and CompensateOnFailure already logs that case
		// loudly with the word "orphan" for whoever greps for it.
		_ = helpers.CompensateOnFailure(ctx, "postgres post row "+postUUID.String(),
			func(compensateCtx context.Context) error {
				return domain.HardDeletePostByUUIUD(compensateCtx, postUUID)
			})

		return

	}

	// publish to MQTT
	mqttCreatePost := mqttStruct.MqttPost{
		Type:             mqttStruct.TYPE_CREATE,
		PostAttachments:  postInfo.MediaObj,
		PostHtmlText:     postInfo.HTMLText,
		PostCreatedAt:    &currentTime,
		PostByUserUuid:   userInfo.UserDgraphInfo.Uuid,
		PostByProfileKey: userInfo.UserDgraphInfo.ProfileKey,
		PostByUserName:   userInfo.UserDgraphInfo.DisplayName(),
		PostByIsBot:      userInfo.UserDgraphInfo.IsBot,
		PostChannelUuid:  postInfo.ChannelUuid,
		PostUuid:         postUUID.String(),
		PostReplyTo:      replyParentPost,
	}

	// Bulk imports (Slack, etc.) suppress notification fan-out so 100k
	// historical messages don't wake every channel member 100k times.
	// We still index OpenSearch because operators want imported content
	// searchable immediately; AI embedding can be redone in one batch
	// pass after the import. See helpers.IsBulkImport.
	bulk := helpers.IsBulkImport(ctx)

	if !bulk {
		go mqttBusiness.PublishPost(&mqttCreatePost, postInfo.ChannelUuid)
	}

	// add in openSearch
	openSearchPost := &openSearchStruct.OpenSearchPost{
		Uuid:               postUUID.String(),
		PostBody:           plainText,
		PostChannelUuid:    postInfo.ChannelUuid,
		PostCreatedAt:      currentTime.Unix(),
		PostByUserUuid:     userInfo.UserDgraphInfo.Uuid,
		PostByProfile:      userInfo.UserDgraphInfo.ProfileKey,
		PostByUserFullName: userInfo.UserDgraphInfo.DisplayName(),
		PostChannelName:    dgraphChannelInfo.Name,
		PostDeletedAt:      nil,
	}

	go domain.CreatePostWithAttachmentsInOpenSearch(openSearchPost, postInfo.MediaObj)

	if !bulk {
		// embed for AI Second Brain (async). Skipped during bulk imports;
		// a follow-up batch embed run handles imported content.
		// For a reply, embed the parent snippet + reply text (contextual
		// embedding) so terse replies carry the meaning of what they answer in
		// the k-NN vector. Same-surface guarantees no permission leak.
		/* AI call omitted in v1 */

		// send push
		pushTitle := fmt.Sprintf("#%s - %s", dgraphChannelInfo.Name, userInfo.UserDgraphInfo.DisplayName())
		// A Discord-style inline reply implicitly pings the parent's author, so
		// route them through the same mention pipeline (activity + push +
		// email). Self-replies are suppressed inside sendNewPostNotification,
		// and channel mute is respected exactly like a real mention.
		replyToAuthorUUID := ""
		if replyParentPost != nil && replyParentPost.PostBy != nil {
			replyToAuthorUUID = replyParentPost.PostBy.Uuid
		}
		go sendNewPostNotification(pushTitle, plainText, postInfo.ChannelUuid, mentionsDgraphUsersList, dgraphChannelInfo, postUUID.String(), &userInfo.UserDgraphInfo, replyToAuthorUUID)

		// Dispatch the workspace event (outgoing webhooks + in-process
		// listeners like the Workflow engine). Detached from the request ctx
		// so listener/dispatch work isn't cancelled when the handler returns.
		// Mention ids for in-process listeners (e.g. the AI coworker). Parsed
		// straight from the HTML so the set is independent of mention
		// resolution (which hides the bot from member lists) — a bot that was
		// explicitly @mentioned must still be detectable here. For native
		// mentions these are Dgraph node uids. The author is named by the one
		// name rule without its last step, never part of their address: the
		// Slack bridge carries the name to Slack, where a shared channel can
		// hold people from other organisations.
		mentionIDs, _ := helpers.GetMentions(postInfo.HTMLText)
		go webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "post.created", map[string]interface{}{
			"post_id":      postUUID.String(),
			"channel_id":   postInfo.ChannelUuid,
			"channel_name": dgraphChannelInfo.Name,
			"text":         plainText,
			"author_id":    userInfo.UserDgraphInfo.Uuid,
			"author_name":  helpers.PersonDisplayName(userInfo.UserDgraphInfo.UserName, userInfo.UserDgraphInfo.UserFullName, ""),
			"mention_ids":  mentionIDs,
			"source":       "user",
		})
	}

	var createdPostInfoRaw adapter.OutputCreatePostForPost
	createdPostInfoRaw.Uuid = postUUID.String()
	createdPostInfoRaw.PostCreated = currentTime

	createdPostInfo = &createdPostInfoRaw

	return
}

func sendNewPostNotification(title string, body string, channelId string, mentionsDgraphUsersList []*dgraphStruct.DgraphUser, dgraphChannelInfo *dgraphStruct.DgraphChannel, postID string, userDgraph *dgraphStruct.DgraphUser, replyToAuthorUUID string) {

	mentionsUUIDList := []string{}
	mentionsMap := make(map[string]bool)

	ctx := context.Background()

	for _, mention := range mentionsDgraphUsersList {
		mentionsUUIDList = append(mentionsUUIDList, mention.Uuid)
		mentionsMap[mention.Uuid] = true
	}

	// Treat the replied-to author as an implicit mention recipient so an inline
	// reply notifies them through the mention pipeline. Suppress self-replies
	// and de-dup with an explicit @mention of the same person.
	if replyToAuthorUUID != "" && replyToAuthorUUID != userDgraph.Uuid && !mentionsMap[replyToAuthorUUID] {
		mentionsUUIDList = append(mentionsUUIDList, replyToAuthorUUID)
		mentionsMap[replyToAuthorUUID] = true
	}

	// 1. Get eligible users for notifications (based on preferences)
	eligibleUserIDs, err := channelNotificationBusiness.GetEligibleUsersForChannelActivity(ctx, channelId, userDgraph.Uuid, mentionsUUIDList, false)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/sendNewPostNotification Failed to get eligible users err: %+v",
			err)
		return
	}

	if len(eligibleUserIDs) == 0 {
		return
	}

	// 2. Publish MQTT Activity Events
	eligibleMentionsUserIDs := []string{}

	for _, uid := range eligibleUserIDs {
		if mentionsMap[uid] {
			eligibleMentionsUserIDs = append(eligibleMentionsUserIDs, uid)
		}
	}
	for _, recipientID := range eligibleMentionsUserIDs {
		activityItem := &dgraphModels.UnifiedActivityItem{
			ActivityType: mqttStruct.MESSAGE_ACTIVITY_MENTION, // In channels, new posts are notifications for members
			Time:         time.Now().Format(time.RFC3339),
			Mention: &dgraphStruct.DgraphMentions{
				PostUuid: postID,
				Post: &dgraphStruct.DgraphPost{
					Uuid: postID,
					Text: body,
					PostBy: &dgraphStruct.DgraphUser{
						Uuid:     userDgraph.Uuid,
						UserName: userDgraph.DisplayName(),
					},
					Channel: &dgraphStruct.DgraphChannel{
						Uuid: channelId,
						Name: dgraphChannelInfo.Name,
					},
				},
				CreatedAt: helpers.TimePointer(time.Now()),
			},
		}
		activityBusiness.PublishActivityToUser(recipientID, activityItem)
	}

	// 3. Handle FCM Push Notifications
	tokens, err := userFCMtokenBusiness.GetFCMTokenByListOfUserId(ctx, eligibleUserIDs)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/sendNewPostNotification Failed to gets user's fcm token err: %+v",
			err)
		return
	}

	pushData := make(map[string]string)

	pushData[firebaseInit.FIREBASE_PUSH_DATA_TYPE] = firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHANNEL
	pushData[firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID] = channelId
	pushData[firebaseInit.FIREBASE_PUSH_DATA_TITLE] = title
	pushData[firebaseInit.FIREBASE_PUSH_DATA_BODY] = body
	pushData[firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID] = postID
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
				"business/sendNewPostNotification Failed to send push notification err: %+v",
				err)
			return
		}
	}

	// Email fan-out: only mentioned users — channel posts in general are
	// too noisy to email everyone. The dispatcher applies its own gates
	// (per-user pref, online check, suppressions).
	notificationBusiness.DispatchChannelMention(
		userDgraph.Uuid,
		userDgraph.DisplayName(),
		userBusiness.GetSignedProfileURL(ctx, userDgraph.ProfileKey),
		channelId,
		dgraphChannelInfo.Name,
		postID,
		body,
		eligibleMentionsUserIDs,
	)
}

func UpdatePostComment(ctx context.Context, commentUUID uuid.UUID, commentInfo *adapter.InputCreateOrUpdateCommentToPost, mentionsDgraphUsersList []*dgraphStruct.DgraphUser, rawDgraphCommentInfo *dgraphStruct.DgraphComment) (err error) {
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
		Uuid:  commentInfo.Uuid,
		DType: []string{"Comment"},
		Text:  commentInfo.HTMLText,
		// Attachments: commentInfo.MediaObj,
		Mentions: &dgraphStruct.DgraphMentions{
			Uid: "uid(me)",
			Comment: &dgraphStruct.DgraphComment{
				Uid: "uid(co)",
			},
			CommentUuid: commentInfo.Uuid,
			Mentions:    commentMentions,
		},
		UpdatedAt: &currentTime,
	}

	err = commentBusiness.UpdateComment(ctx, &dgraphComment, commentUUID, currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/UpdatePostComment Failed to update comment on post err: %+v",
			err)
		return
	}

	mqttPostCommentCount := mqttStruct.MqttPostComment{
		Type:        mqttStruct.TYPE_UPDATE,
		PostUuid:    rawDgraphCommentInfo.Post.Uuid,
		CommentUuid: commentInfo.Uuid,
		HTMLText:    commentInfo.HTMLText,
		UserUuid:    rawDgraphCommentInfo.CommentBy.Uuid,
	}

	if helpers.IsBulkImport(ctx) {
		// Skip MQTT during a bulk Slack import — the underlying Dgraph
		// + Postgres update above is enough for the timeline; we just
		// don't want to push live edit events for historical content.
		return
	}

	go mqttBusiness.PublishPostComment(&mqttPostCommentCount, rawDgraphCommentInfo.Post.Channel.Uuid)

	return
}

func CreatePostComment(ctx context.Context, commentInfo *adapter.InputCreateOrUpdateCommentToPost, userInfo *userModels.UserInfo, mentionsDgraphUsersList []*dgraphStruct.DgraphUser, dgraphPostInfo *dgraphStruct.DgraphPost) (createdCommentData *adapter.OutputCreatePostCommentForPost, err error) {

	commentUUID := uuid.New()
	// CreateTimeOrNow respects the Slack-import backdating override; see
	// helpers.WithImportTimestamp. Falls back to time.Now() natively.
	currentTime := helpers.CreateTimeOrNow(ctx)
	zeroUnixTime := time.Time{}

	var commentMentions []*dgraphStruct.DgraphUser

	for _, mention := range mentionsDgraphUsersList {
		commentMentions = append(commentMentions, &dgraphStruct.DgraphUser{
			DType: []string{"User"},
			Uid:   mention.Uid,
		})
	}

	for _, mediaObj := range commentInfo.MediaObj {
		mediaObj.DType = []string{"Attachment"}
		mediaObj.CreatedBy = &dgraphStruct.DgraphUser{
			Uid: userInfo.UserDgraphInfo.Uid,
		}
		mediaObj.CreatedAt = &currentTime
	}

	dgraphPost := dgraphStruct.DgraphPost{
		Uid:  "uid(po)",
		Uuid: commentInfo.PostUuid,
		Comments: []*dgraphStruct.DgraphComment{
			{
				DType: []string{"Comment"},
				Uid:   "uid(co)",
				Uuid:  commentUUID.String(),
				Text:  commentInfo.HTMLText,
				Post: &dgraphStruct.DgraphPost{
					Uid: "uid(po)",
				},
				ContentAddedBy: &dgraphStruct.DgraphUser{
					Uid: dgraphPostInfo.PostBy.Uid,
				},
				Attachments: commentInfo.MediaObj,
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
					Uid: userInfo.UserDgraphInfo.Uid,
				},
			},
		},
	}

	_, err = commentBusiness.CreateCommentInAPost(ctx, &dgraphPost, userInfo, commentUUID, currentTime, dgraphPostInfo, mentionsDgraphUsersList)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreatePostComment Failed to create comment on post err: %+v",
			err)
		return
	}

	createdCommentData = &adapter.OutputCreatePostCommentForPost{
		Uuid:             commentUUID.String(),
		CommentCreatedAt: currentTime,
	}

	mqttPostCommentCount := mqttStruct.MqttPostComment{
		Type:           mqttStruct.TYPE_CREATE,
		PostUuid:       commentInfo.PostUuid,
		CommentUuid:    commentUUID.String(),
		HTMLText:       commentInfo.HTMLText,
		ChannelUuid:    dgraphPostInfo.Channel.Uuid,
		CreatedAt:      &currentTime,
		UserUuid:       userInfo.UserDgraphInfo.Uuid,
		UserName:       userInfo.UserDgraphInfo.DisplayName(),
		UserProfileKey: userInfo.UserDgraphInfo.ProfileKey,
		Attachments:    commentInfo.MediaObj,
	}

	if !helpers.IsBulkImport(ctx) {
		go mqttBusiness.PublishPostComment(&mqttPostCommentCount, dgraphPostInfo.Channel.Uuid)

		go PublishPostCommentActivity(commentUUID.String(), commentInfo.HTMLText, commentInfo.PostUuid, currentTime, mentionsDgraphUsersList, dgraphPostInfo, &userInfo.UserDgraphInfo)

		// Notify listeners so an AI teammate can CONTINUE a thread it is part of:
		// a person can @mention the agent again, answer its needs_human question,
		// or just say "retry" right in the thread and it picks the work back up.
		// The post_id is the PARENT post (the thread), so the agent's reply lands
		// as another comment in the same thread. Loop-safe: an agent's own reply
		// comments are written via botpost (not this path) and never dispatch;
		// the event bus also skips listeners for workflow-generated writes.
		// The author is named as for post.created, for the Slack bridge.
		mentionIDs := make([]string, 0, len(mentionsDgraphUsersList))
		for _, m := range mentionsDgraphUsersList {
			if m != nil && m.Uid != "" {
				mentionIDs = append(mentionIDs, m.Uid)
			}
		}
		go webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "post.comment.created", map[string]interface{}{
			"post_id":      commentInfo.PostUuid,
			"channel_id":   dgraphPostInfo.Channel.Uuid,
			"channel_name": dgraphPostInfo.Channel.Name,
			"author_id":    userInfo.UserDgraphInfo.Uuid,
			"author_name":  helpers.PersonDisplayName(userInfo.UserDgraphInfo.UserName, userInfo.UserDgraphInfo.UserFullName, ""),
			"comment_uuid": commentUUID.String(),
			"text":         helpers.HTMLToPlainText(commentInfo.HTMLText),
			"mention_ids":  mentionIDs,
		})
	}

	return
}

func PublishPostCommentActivity(commentUUID string, commentBody string, postUUID string, currentTime time.Time, mentionsDgraphUsersList []*dgraphStruct.DgraphUser, dgraphPostInfo *dgraphStruct.DgraphPost, userDgraph *dgraphStruct.DgraphUser) {

	ctx := context.Background()
	mentionUUIDs := []string{}
	for _, m := range mentionsDgraphUsersList {
		mentionUUIDs = append(mentionUUIDs, m.Uuid)
	}

	// 1. Notify Post Owner (if not the commenter)
	if dgraphPostInfo.PostBy.Uuid != userDgraph.Uuid {
		activityItem := &dgraphModels.UnifiedActivityItem{
			ActivityType: mqttStruct.MESSAGE_ACTIVITY_COMMENT,
			Time:         time.Now().Format(time.RFC3339),
			Comment: &dgraphStruct.DgraphComment{
				Uuid: commentUUID,
				Text: commentBody,
				CommentBy: &dgraphStruct.DgraphUser{
					Uuid:     userDgraph.Uuid,
					UserName: userDgraph.DisplayName(),
				},
				Post: &dgraphStruct.DgraphPost{
					Uuid: postUUID,
					Channel: &dgraphStruct.DgraphChannel{
						Uuid: dgraphPostInfo.Channel.Uuid,
					},
				},
				CreatedAt: &currentTime,
			},
		}
		activityBusiness.PublishActivityToUser(dgraphPostInfo.PostBy.Uuid, activityItem)
	}

	// 2. Notify Mentions

	eligibleUserIDs, err := channelNotificationBusiness.GetEligibleUsersForChannelActivity(ctx, dgraphPostInfo.Channel.Uuid, userDgraph.Uuid, mentionUUIDs, true)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/PublishPostCommentActivity Failed to get eligible users err: %+v",
			err)
		return
	}

	for _, mentionUUID := range eligibleUserIDs {
		if mentionUUID == dgraphPostInfo.PostBy.Uuid {
			continue
		}
		activityItem := &dgraphModels.UnifiedActivityItem{
			ActivityType: mqttStruct.MESSAGE_ACTIVITY_MENTION,
			Time:         time.Now().Format(time.RFC3339),
			Mention: &dgraphStruct.DgraphMentions{
				CommentUuid: commentUUID,
				Comment: &dgraphStruct.DgraphComment{
					Uuid: commentUUID,
					Text: commentBody,
					CommentBy: &dgraphStruct.DgraphUser{
						Uuid:     userDgraph.Uuid,
						UserName: userDgraph.DisplayName(),
					},
					Post: &dgraphStruct.DgraphPost{
						Uuid: postUUID,
						Channel: &dgraphStruct.DgraphChannel{
							Uuid: dgraphPostInfo.Channel.Uuid,
						},
					},
				},
				CreatedAt: &currentTime,
			},
		}
		activityBusiness.PublishActivityToUser(mentionUUID, activityItem)
	}

}

func CreateOrUpdatePostCommentReaction(ctx context.Context, reactionInfo *adapter.InputUpdateReactionForCommentInPost, dgraphCommentRaw *dgraphStruct.DgraphComment, userDgraph *dgraphStruct.DgraphUser) (reactionUUID string, err error) {
	currentTime := time.Now()

	dgraphComment := &dgraphStruct.DgraphComment{
		Uid:   "uid(co)",
		Uuid:  reactionInfo.Uuid,
		DType: []string{"Comment"},
		Reactions: []*dgraphStruct.DgraphReaction{
			{
				Uid:       reactionInfo.ReactionDgraphUid,
				DType:     []string{"Reaction"},
				EmojiUuid: reactionInfo.EmojiUuid,
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

	reactionUUID, err = commentBusiness.CreateOrUpdateDgraphCommentReaction(ctx, dgraphComment)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateOrUpdatePostCommentReaction Failed to update comment reaction in post err: %+v",
			err)
		return
	}

	mqttPostCommentReaction := mqttStruct.MqttPostCommentReaction{
		Type:            mqttStruct.TYPE_CREATE,
		EmojiReactionId: reactionInfo.EmojiUuid,
		CommentUuid:     reactionInfo.Uuid,
		AddedByUserName: userDgraph.DisplayName(),
		AddedByUuid:     userDgraph.Uuid,
		ReactionUuid:    reactionUUID,
		PostUuid:        dgraphCommentRaw.Post.Uuid,
	}

	if len(mqttPostCommentReaction.ReactionUuid) == 0 {
		mqttPostCommentReaction.Type = mqttStruct.TYPE_UPDATE
		mqttPostCommentReaction.ReactionUuid = reactionInfo.ReactionDgraphUid
	}

	// Suppress MQTT + activity fan-out during a bulk Slack import.
	// Without this gate, importing a workspace with 50k historical
	// comment reactions would push that many notifications/activity
	// rows in a tight loop. The Dgraph reaction node is still written
	// above so the data lands; only the live UI side-effects are skipped.
	if helpers.IsBulkImport(ctx) {
		return
	}

	go mqttBusiness.PublishPostCommentReaction(&mqttPostCommentReaction, dgraphCommentRaw.Post.Channel.Uuid)

	// --- Activity Notification for Comment Owner/Post Owner ---
	go func() {
		recipients := make(map[string]bool)
		// 1. Notify Comment Owner (if not the reactor)
		if dgraphCommentRaw.CommentBy.Uuid != userDgraph.Uuid {
			recipients[dgraphCommentRaw.CommentBy.Uuid] = true
		}
		// 2. Notify Post Owner (if not the reactor and not the comment owner)
		if dgraphCommentRaw.Post.PostBy.Uuid != userDgraph.Uuid && dgraphCommentRaw.Post.PostBy.Uuid != dgraphCommentRaw.CommentBy.Uuid {
			recipients[dgraphCommentRaw.Post.PostBy.Uuid] = true
		}

		for recipientID := range recipients {
			activityItem := &dgraphModels.UnifiedActivityItem{
				ActivityType: "REACTION",
				Time:         time.Now().Format(time.RFC3339),
				Reaction: &dgraphModels.ReactionsActivity{
					DgraphReaction: dgraphStruct.DgraphReaction{
						EmojiUuid: reactionInfo.EmojiUuid,
						AddedBy: &dgraphStruct.DgraphUser{
							Uuid:     userDgraph.Uuid,
							UserName: userDgraph.DisplayName(),
						},
						AddedAt: &currentTime,
					},
					Comment: &dgraphStruct.DgraphComment{
						Uuid: reactionInfo.Uuid,
						Post: &dgraphStruct.DgraphPost{
							Uuid: dgraphCommentRaw.Post.Uuid,
							Channel: &dgraphStruct.DgraphChannel{
								Uuid: dgraphCommentRaw.Post.Channel.Uuid,
							},
						},
					},
				},
			}
			activityBusiness.PublishActivityToUser(recipientID, activityItem)
		}
	}()

	return
}

func CreateOrUpdatePostReaction(ctx context.Context, reactionInfo *adapter.InputUpdateReactionForPost, postCreatedByDgraphUID string, postOwnerUUID string, userDgraphUID string, userUUID string, userName string, channelId string) (reactionUid string, err error) {

	currentTime := time.Now()

	dgraphPost := dgraphStruct.DgraphPost{
		Uid:   "uid(po)",
		Uuid:  reactionInfo.Uuid,
		DType: []string{"Post"},
		Reactions: []*dgraphStruct.DgraphReaction{
			{
				Uid:       reactionInfo.ReactionDgraphUid,
				DType:     []string{"Reaction"},
				EmojiUuid: reactionInfo.EmojiUuid,
				AddedAt:   &currentTime,
				ContentAddedBy: &dgraphStruct.DgraphUser{
					Uid: postCreatedByDgraphUID,
				},
				AddedBy: &dgraphStruct.DgraphUser{
					Uid: userDgraphUID,
				},
			},
		},
	}

	reactionUid, err = domain.CreateOrUpdateDgraphPostReaction(ctx, &dgraphPost)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateOrUpdatePostReaction Failed to update reaction in post err: %+v",
			err)
		return
	}

	bulk := helpers.IsBulkImport(ctx)

	mqttPostReaction := mqttStruct.MqttPostReaction{
		Type:            mqttStruct.TYPE_CREATE,
		EmojiReactionId: reactionInfo.EmojiUuid,
		PostUuid:        reactionInfo.Uuid,
		AddedByUuid:     userUUID,
		AddedByUserName: userName,
		ChannelUuid:     channelId,
		ReactionUuid:    reactionUid,
	}

	if len(mqttPostReaction.ReactionUuid) == 0 {
		mqttPostReaction.Type = mqttStruct.TYPE_UPDATE
		mqttPostReaction.ReactionUuid = reactionInfo.ReactionDgraphUid
	}

	if !bulk {
		go mqttBusiness.PublishPostReaction(&mqttPostReaction, channelId)
	}

	// --- Activity Notification for Post Owner ---
	if !bulk && postOwnerUUID != userUUID {
		go func() {
			activityItem := &dgraphModels.UnifiedActivityItem{
				ActivityType: "POST_REACTION",
				Time:         time.Now().Format(time.RFC3339),
				Reaction: &dgraphModels.ReactionsActivity{
					DgraphReaction: dgraphStruct.DgraphReaction{
						EmojiUuid: reactionInfo.EmojiUuid,
						AddedBy: &dgraphStruct.DgraphUser{
							Uuid:     userUUID,
							UserName: userName,
						},
						AddedAt: &currentTime,
					},
					Post: &dgraphStruct.DgraphPost{
						Uuid: reactionInfo.Uuid,
						Channel: &dgraphStruct.DgraphChannel{
							Uuid: channelId,
						},
					},
				},
			}
			activityBusiness.PublishActivityToUser(postOwnerUUID, activityItem)
		}()
	}

	return
}

func GetPostByUUID(ctx context.Context, postUUID uuid.UUID) (postInfo *models.Post, err error) {
	postInfo, err = domain.GetPostByUUID(ctx, postUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetPostByUUID Failed to get post info from potgres err: %+v",
			err)
		return
	}

	return
}

func UpdatePost(ctx context.Context, inputPostInfo *adapter.InputCreateOrUpdatePostInfo, mentions []*dgraphStruct.DgraphUser, postUUID uuid.UUID, channelId string, postByUUID string) (err error) {

	currentTime := time.Now()
	err = domain.UpdatePostByUUID(ctx, postUUID, currentTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/UpdatePost Failed to create new post err: %+v",
			err)
		return
	}

	var dgraphMentions []*dgraphStruct.DgraphUser
	for _, mention := range mentions {
		dgraphMentions = append(dgraphMentions, &dgraphStruct.DgraphUser{
			Uid:   mention.Uid,
			DType: []string{"User"},
		})
	}
	plainText := helpers.HTMLToPlainText(inputPostInfo.HTMLText)
	// add channel in dgraph
	dgraphPost := dgraphStruct.DgraphPost{
		Uid:   "uid(po)",
		Uuid:  postUUID.String(),
		DType: []string{"Post"},
		Text:  inputPostInfo.HTMLText,
		Mentions: &dgraphStruct.DgraphMentions{
			Uid:       "uid(me)",
			Mentions:  dgraphMentions,
			UpdatedAt: &currentTime,
			PostUuid:  postUUID.String(),
		},
		UpdatedAt: &currentTime,
	}

	_, err = domain.CreateOrUpdateDgraphPostWithMentions(ctx, &dgraphPost)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/UpdatePost Failed to update post in dgraph err: %+v",
			err)

		return

	}

	mqttPost := mqttStruct.MqttPost{
		Type:            mqttStruct.TYPE_UPDATE,
		PostUuid:        postUUID.String(),
		PostHtmlText:    inputPostInfo.HTMLText,
		PostUpdatedAt:   &currentTime,
		PostChannelUuid: channelId,
		PostByUserUuid:  postByUUID,
	}

	openSearchPost := openSearchStruct.OpenSearchPost{
		Uuid:          postUUID.String(),
		PostBody:      plainText,
		PostUpdatedAt: currentTime.Unix(),
	}
	go domain.UpdatePostInOpenSearch(&openSearchPost)

	// Suppress MQTT, AI re-embedding, and webhook dispatch during bulk
	// Slack import. The Postgres + Dgraph + OpenSearch writes above are
	// preserved so search and timelines stay consistent; only the live
	// fan-out is skipped to avoid pumping every channel member with
	// "post edited" events for thousands of historical edits.
	if helpers.IsBulkImport(ctx) {
		return
	}

	go mqttBusiness.PublishPost(&mqttPost, channelId)

	// Re-embed for AI Second Brain with updated content (async)
	/* AI call omitted in v1 */

	// Reconcile any workspace-memory items captured from this post: refresh
	// the stored snapshot to the edited text (privacy + freshness), or drop
	// it if the edit emptied the message. Preserves the user's save.
	/* AI call omitted in v1 */

	go webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "post.updated", map[string]interface{}{
		"post_id":    postUUID.String(),
		"channel_id": channelId,
	})

	return
}

func DeletePost(ctx context.Context, rawDgraphPost *dgraphStruct.DgraphPost, channelId string) (err error) {

	postUUID, err := uuid.Parse(rawDgraphPost.Uuid)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/UpdatePost Failed to parse post UUID err: %+v",
			err)
		return
	}

	currentTime := time.Now()

	err = domain.SoftDeletePostByUUIUD(ctx, postUUID, currentTime)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/UpdatePost Failed to soft delete post err: %+v",
			err)
		return
	}

	// add delete time in dgraph
	dgraphPost := dgraphStruct.DgraphPost{
		Uid:       "uid(po)",
		Uuid:      postUUID.String(),
		DType:     []string{"Post"},
		DeletedAt: &currentTime,
	}

	_, err = domain.CreateOrUpdateDgraphPost(ctx, &dgraphPost)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreatePost Failed to update post in dgraph err: %+v",
			err)

		return

	}

	mqttPost := mqttStruct.MqttPost{
		PostUuid:        postUUID.String(),
		Type:            mqttStruct.TYPE_DELETE,
		PostChannelUuid: channelId,
		PostByUserUuid:  rawDgraphPost.PostBy.Uuid,
	}

	openSearchPost := &openSearchStruct.OpenSearchPost{
		Uuid:          rawDgraphPost.Uuid,
		PostDeletedAt: helpers.Int64Pointer(currentTime.Unix()),
	}

	go domain.DeletePostWithAttachmentsInOpenSearch(openSearchPost, rawDgraphPost.MediaObj)
	go globalSearchDomain.SyncCascadingDeletionInOpenSearch(
		[]string{"comment_post_id", "attachment_post_id"},
		rawDgraphPost.Uuid,
		currentTime.Unix(),
		[]string{"comments", "attachments"},
		"cascade",
	)

	// Skip live MQTT/AI/webhook fan-out during bulk Slack import.
	// Postgres + Dgraph + OpenSearch are already updated; we just don't
	// want to push delete events for thousands of historical messages
	// or trigger AI embedding deletions one-by-one (a re-embed pass
	// after the import handles cleanup more efficiently).
	if helpers.IsBulkImport(ctx) {
		return
	}

	go mqttBusiness.PublishPost(&mqttPost, channelId)

	/* AI call omitted in v1 */
	// Cascade soft-delete to AI embeddings (post + its comments)
	go globalSearchDomain.SyncCascadingDeletionInOpenSearch(
		[]string{"content_uuid", "post_uuid"},
		rawDgraphPost.Uuid,
		currentTime.Unix(),
		[]string{"ai_embeddings"},
		"cascade",
	)

	go webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "post.deleted", map[string]interface{}{
		"post_id":    rawDgraphPost.Uuid,
		"channel_id": channelId,
	})

	return
}

func GetOldPosts(ctx context.Context, channelId string, lastPostTime time.Time) (posts PostPagination, err error) {

	dgraphPosts, err := domain.GetDgraphOldPostFromDgraph(ctx, channelId, lastPostTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetOldPosts Failed to get posts form dgraph err: %+v",
			err)

		return
	}

	if len(dgraphPosts) > domain.POST_COUNT {
		posts.Posts = dgraphPosts[:domain.POST_COUNT]
	} else {
		posts.Posts = dgraphPosts
	}

	posts.HasMore = len(dgraphPosts) > domain.POST_COUNT

	return
}

func GetDgraphNewPostIncludiongPostFromDgraph(ctx context.Context, channelUUID string, lastPostTime *time.Time) (posts PostPagination, err error) {
	dgraphPosts, err := domain.GetDgraphNewPostIncludiongPostFromDgraph(ctx, channelUUID, lastPostTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphNewPostIncludiongPostFromDgraph Failed to get posts form dgraph err: %+v",
			err)

		return
	}

	if len(dgraphPosts) > domain.POST_COUNT {
		posts.Posts = dgraphPosts[:domain.POST_COUNT]
	} else {
		posts.Posts = dgraphPosts
	}
	posts.HasMore = len(dgraphPosts) > domain.POST_COUNT

	return
}

func GetDgraphPostOnlyTextDgraph(ctx context.Context, postUUID string) (dgraphPost *dgraphStruct.DgraphPost, err error) {

	dgraphPost, err = domain.GetDgraphPostOnlyTextDgraph(ctx, postUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphPostOnlyTextDgraph Failed to get post form dgraph err: %+v",
			err)

		return
	}

	return
}

func GetNewPosts(ctx context.Context, channelId string, lastPostTime time.Time) (posts PostPagination, err error) {

	dgraphPosts, err := domain.GetDgraphNewPostFromDgraph(ctx, channelId, lastPostTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetNewPosts Failed to get posts form dgraph err: %+v",
			err)

		return
	}

	if len(dgraphPosts) > domain.POST_COUNT {
		posts.Posts = dgraphPosts[:domain.POST_COUNT]
	} else {
		posts.Posts = dgraphPosts
	}
	posts.HasMore = len(dgraphPosts) > domain.POST_COUNT

	return
}

func GetLatestPosts(ctx context.Context, channelId string) (posts PostPagination, err error) {
	dgraphPosts, err := domain.GetDgraphLatestPostFromDgraph(ctx, channelId)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetLatestPosts Failed to get posts form dgraph err: %+v",
			err)

		return
	}

	if len(dgraphPosts) > domain.POST_COUNT {
		posts.Posts = dgraphPosts[:domain.POST_COUNT]
	} else {
		posts.Posts = dgraphPosts
	}

	posts.HasMore = false

	return
}

func GetDgraphPostByUUID(ctx context.Context, postDgraphUUID string, userDgraphUID string) (dgraphPost *dgraphStruct.DgraphPost, err error) {
	dgraphPost, err = domain.GetDgraphPostByUUID(ctx, postDgraphUUID, userDgraphUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphPostByUUID Failed to get post form dgraph err: %+v",
			err)

		return
	}

	return
}

func GetUnDeletedDgraphPostChannelBasicInfo(ctx context.Context, postUUID string) (dgraphPost *dgraphStruct.DgraphPost, err error) {
	dgraphPost, err = domain.GetUnDeletedDgraphPostChannelBasicInfo(ctx, postUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphPostByUUIDWithAllComments Failed to get post form dgraph err: %+v",
			err)

		return
	}

	return
}

func GetDgraphPostByUUIDWithAllComments(ctx context.Context, postDgraphUUID string, userDgraphUUID string) (dgraphPost *dgraphStruct.DgraphPost, err error) {
	dgraphPost, err = domain.GetDgraphPostByUUIDWithAllComments(ctx, postDgraphUUID, userDgraphUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphPostByUUIDWithAllComments Failed to get post form dgraph err: %+v",
			err)

		return
	}

	return
}

func DeletePostReaction(ctx context.Context, postDgraphUUID string, reactionDgraphUUID string, postCreatedByUUID string, userDgraphUID string, postID string, userID string, channelID string) (err error) {
	err = domain.DeletePostReaction(ctx, postDgraphUUID, reactionDgraphUUID, postCreatedByUUID, userDgraphUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/DeletePostReaction Failed to delete reaction form dgraph err: %+v",
			err)

		return
	}

	mqttPostReaction := mqttStruct.MqttPostReaction{
		Type:         mqttStruct.TYPE_DELETE,
		PostUuid:     postID,
		AddedByUuid:  userID,
		ChannelUuid:  channelID,
		ReactionUuid: reactionDgraphUUID,
	}

	go mqttBusiness.PublishPostReaction(&mqttPostReaction, channelID)

	return
}

func DeleteCommentOnPost(ctx context.Context, commentUUID uuid.UUID, rawDgraphComment *dgraphStruct.DgraphComment, userId string) (err error) {

	currentTime := time.Now()
	dgraphComment := &dgraphStruct.DgraphComment{
		Uid:       "uid(co)",
		Uuid:      commentUUID.String(),
		DeletedAt: &currentTime,
		DType:     []string{"Comment"},
	}
	err = commentBusiness.DeleteComment(ctx, dgraphComment, currentTime, commentUUID, rawDgraphComment)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/DeleteCommentOnPost Failed to delete comment from post err: %+v",
			err)

		return
	}

	mqttPostCommentCount := mqttStruct.MqttPostComment{
		Type:        mqttStruct.TYPE_DELETE,
		PostUuid:    rawDgraphComment.Post.Uuid,
		ChannelUuid: rawDgraphComment.Post.Channel.Uuid,
		UserUuid:    userId,
		CommentUuid: commentUUID.String(),
	}

	if helpers.IsBulkImport(ctx) {
		// Skip live MQTT during bulk Slack import.
		return
	}

	go mqttBusiness.PublishPostComment(&mqttPostCommentCount, rawDgraphComment.Post.Channel.Uuid)

	return
}

func DeleteReactionOnCommentPost(ctx context.Context, commentDgraph *dgraphStruct.DgraphComment, reactionDgraphUUID string) (err error) {
	err = commentBusiness.DeleteCommentReaction(ctx, commentDgraph.Uid, reactionDgraphUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/DeleteReactionOnCommentPost Failed to delete reaction on comment in post err: %+v",
			err)

		return
	}

	mqttPostCommentReaction := mqttStruct.MqttPostCommentReaction{
		Type:         mqttStruct.TYPE_DELETE,
		CommentUuid:  commentDgraph.Uuid,
		ReactionUuid: reactionDgraphUUID,
		PostUuid:     commentDgraph.Post.Uuid,
	}

	go mqttBusiness.PublishPostCommentReaction(&mqttPostCommentReaction, commentDgraph.Post.Channel.Uuid)

	return
}
