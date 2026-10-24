package business

import (
	"context"
	"fmt"
	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	postgresStruct "github.com/akashc777/OneCamp/models/postgres"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Post"
	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	commentBusiness "github.com/akashc777/OneCamp/business/Comment"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	refsBusiness "github.com/akashc777/OneCamp/business/Refs"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	channelNotificationBusiness "github.com/akashc777/OneCamp/business/UserChannelNotification"
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
	ai "github.com/akashc777/OneCamp/services/AI"
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

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreatePost Failed to create new post err: %+v",
			err)
		return
	}

	replyParentPost, err := writePostNode(ctx, postUUID, postInfo, userInfo, mentionsDgraphUsersList, dgraphChannelInfo, currentTime)

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

	fanOutCreatedPost(ctx, postUUID, postInfo, userInfo, mentionsDgraphUsersList, dgraphChannelInfo, replyParentPost, currentTime)

	var createdPostInfoRaw adapter.OutputCreatePostForPost
	createdPostInfoRaw.Uuid = postUUID.String()
	createdPostInfoRaw.PostCreated = currentTime

	createdPostInfo = &createdPostInfoRaw

	return
}

// fanOutCreatedPost is today's side effects of a new channel post, after its
// node is written: what the durable pipeline's sinks do from the outbox
// instead (postSinks.go), through the same seams, so golden_test.go can hold
// the two to one answer.
func fanOutCreatedPost(ctx context.Context, postUUID uuid.UUID, postInfo *adapter.InputCreateOrUpdatePostInfo, userInfo *userModels.UserInfo, mentionsDgraphUsersList []*dgraphStruct.DgraphUser, dgraphChannelInfo *dgraphStruct.DgraphChannel, replyParentPost *dgraphStruct.DgraphPost, currentTime time.Time) {
	plainText := helpers.HTMLToPlainText(postInfo.HTMLText)

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
		helpers.GoSafeNamed("post.publish", func() { publishPost(&mqttCreatePost, postInfo.ChannelUuid) })
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

	helpers.GoSafeNamed("post.search", func() { createSearchEntry(openSearchPost, postInfo.MediaObj) })

	if !bulk {
		// embed for AI Second Brain (async). Skipped during bulk imports;
		// a follow-up batch embed run handles imported content.
		// For a reply, embed the parent snippet + reply text (contextual
		// embedding) so terse replies carry the meaning of what they answer in
		// the k-NN vector. Same-surface guarantees no permission leak.
		ai.EmbedPostContent(withReplyContext(replyParentPost, plainText), postUUID.String(), postInfo.ChannelUuid, dgraphChannelInfo.Name, userInfo.UserDgraphInfo.Uuid, userInfo.UserDgraphInfo.DisplayName())

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
		helpers.GoSafeNamed("post.notify", func() {
			notifyPost(context.Background(), pushTitle, plainText, postInfo.HTMLText, postInfo.ChannelUuid, mentionsDgraphUsersList, dgraphChannelInfo, postUUID.String(), &userInfo.UserDgraphInfo, replyToAuthorUUID, nil)
		})

		// Dispatch the workspace event (outgoing webhooks + in-process
		// listeners like the Workflow engine). Detached from the request ctx
		// so listener/dispatch work isn't cancelled when the handler returns.
		event := postCreatedEvent(postUUID.String(), postInfo.ChannelUuid, dgraphChannelInfo.Name, postInfo.HTMLText, &userInfo.UserDgraphInfo)
		detached := context.WithoutCancel(ctx)
		helpers.GoSafeNamed("post.events", func() { dispatchPost(detached, "post.created", event) })
	}
}

// postCreatedEvent is the workspace event a channel post raises (outgoing
// webhooks and the in-process listeners: workflows, the Slack bridge, the AI
// coworker, agents). Mention ids are parsed straight from the HTML so the set
// is independent of mention resolution (which hides the bot from member
// lists): a bot that was explicitly @mentioned must still be detectable here.
// For native mentions these are Dgraph node uids. The author is named by the
// one name rule without its last step, never part of their address: the
// Slack bridge carries the name to Slack, where a shared channel can hold
// people from other organisations. Pure.
func postCreatedEvent(postID, channelID, channelName, html string, author *dgraphStruct.DgraphUser) map[string]interface{} {
	mentionIDs, _ := helpers.GetMentions(html)
	return map[string]interface{}{
		"post_id":      postID,
		"channel_id":   channelID,
		"channel_name": channelName,
		"text":         helpers.HTMLToPlainText(html),
		"author_id":    author.Uuid,
		"author_name":  helpers.PersonDisplayName(author.UserName, author.UserFullName, ""),
		"mention_ids":  mentionIDs,
		"source":       "user",
	}
}

// writePostNode writes a new post's graph node, its channel's and author's
// edges to it, its Mention node, and its quote of a post in the same channel,
// and answers the post it quotes (nil for none). Shared by today's CreatePost
// and the durable write (durable.go), so the two write the same node.
func writePostNode(ctx context.Context, postUUID uuid.UUID, postInfo *adapter.InputCreateOrUpdatePostInfo, userInfo *userModels.UserInfo, mentionsDgraphUsersList []*dgraphStruct.DgraphUser, dgraphChannelInfo *dgraphStruct.DgraphChannel, currentTime time.Time) (*dgraphStruct.DgraphPost, error) {
	zeroUnixTime := time.Time{}

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

	// New nodes from the upload's own fields: never a node the request names.
	postInfo.MediaObj = dgraphStruct.NewAttachments(postInfo.MediaObj, userInfo.UserDgraphInfo.Uid, currentTime)

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

	_, err := domain.CreateOrUpdateDgraphPost(ctx, &dgraphPost)
	if err == nil {
		// What it links to joins the work graph (DG6).
		refsBusiness.Saved(ctx, refsBusiness.Message, postUUID.String(), postInfo.HTMLText, true)
	}
	return replyParentPost, err
}

func sendNewPostNotification(title string, body string, channelId string, mentionsDgraphUsersList []*dgraphStruct.DgraphUser, dgraphChannelInfo *dgraphStruct.DgraphChannel, postID string, userDgraph *dgraphStruct.DgraphUser, replyToAuthorUUID string) {
	notifyNewPost(context.Background(), title, body, "", channelId, mentionsDgraphUsersList, dgraphChannelInfo, postID, userDgraph, replyToAuthorUUID, nil)
}

// notifyNewPost is a channel post's notices: the Activity item for each
// eligible person it mentions (or whose post it quotes), the push and desktop
// notice for everyone eligible, and the email for those it names. html is the
// post as written: whom its own chips name. Someone a broadcast or a team's
// mention reached (business/Reach) is told in the Inbox and by push, as
// anyone mentioned, and isn't emailed: one @channel in a large channel would
// otherwise email everyone in it who is away.
func notifyNewPost(ctx context.Context, title string, body string, html string, channelId string, mentionsDgraphUsersList []*dgraphStruct.DgraphUser, dgraphChannelInfo *dgraphStruct.DgraphChannel, postID string, userDgraph *dgraphStruct.DgraphUser, replyToAuthorUUID string, tell outboxBusiness.Told) {

	mentionsUUIDList := []string{}
	mentionsMap := make(map[string]bool)

	for _, mention := range mentionsDgraphUsersList {
		mentionsUUIDList = append(mentionsUUIDList, mention.Uuid)
		mentionsMap[mention.Uuid] = true
	}
	named := namedIn(html, mentionsDgraphUsersList)

	// Treat the replied-to author as an implicit mention recipient so an inline
	// reply notifies them through the mention pipeline. Suppress self-replies
	// and de-dup with an explicit @mention of the same person.
	if replyToAuthorUUID != "" && replyToAuthorUUID != userDgraph.Uuid {
		if !mentionsMap[replyToAuthorUUID] {
			mentionsUUIDList = append(mentionsUUIDList, replyToAuthorUUID)
			mentionsMap[replyToAuthorUUID] = true
		}
		named[replyToAuthorUUID] = true
	}

	// Members whose keywords it holds hear of it as of a mention (TN-53),
	// never by email (they aren't in named).
	for _, id := range keywordReached(ctx, channelId, body, userDgraph.Uuid, mentionsMap) {
		mentionsUUIDList = append(mentionsUUIDList, id)
		mentionsMap[id] = true
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
	eligibleMentionsUserIDs, eligibleNamedUserIDs := []string{}, []string{}

	for _, uid := range eligibleUserIDs {
		if mentionsMap[uid] {
			eligibleMentionsUserIDs = append(eligibleMentionsUserIDs, uid)
		}
		if named[uid] {
			eligibleNamedUserIDs = append(eligibleNamedUserIDs, uid)
		}
	}
	for _, recipientID := range tell.Only("activity", eligibleMentionsUserIDs) {
		activityItem := &dgraphModels.UnifiedActivityItem{
			ActivityType: mqttStruct.MESSAGE_ACTIVITY_MENTION, // In channels, new posts are notifications for members
			Time:         time.Now().UTC().Format(time.RFC3339Nano),
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

	// 3. The push, and the desktop app's notice, to those of them the push
	// gate lets through (business/Notification Deliver).
	pushData := make(map[string]string)

	pushData[firebaseInit.FIREBASE_PUSH_DATA_TYPE] = firebaseInit.FIREBASE_PUSH_DATA_TYPE_CHANNEL
	pushData[firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID] = channelId
	pushData[firebaseInit.FIREBASE_PUSH_DATA_TITLE] = title
	pushData[firebaseInit.FIREBASE_PUSH_DATA_BODY] = body
	pushData[firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID] = postID
	pushData[firebaseInit.FIREBASE_PUSH_DATA_USERNAME] = userDgraph.DisplayName()
	pushData[firebaseInit.FIREBASE_PUSH_DATA_ICON] = userBusiness.GetSignedProfileURL(ctx, userDgraph.ProfileKey)

	if pushTo := tell.Only("push", eligibleUserIDs); len(pushTo) > 0 {
		notificationBusiness.DeliverFrom(ctx, userDgraph.Uuid, pushTo, pushData, "channel:"+channelId+":"+postID)
	}

	// Email fan-out: only the people it names — channel posts in general,
	// and a broadcast's whole room, are too noisy to email. The dispatcher
	// applies its own gates (per-user pref, online check, suppressions).
	notificationBusiness.DispatchChannelMention(
		userDgraph.Uuid,
		userDgraph.DisplayName(),
		userBusiness.GetSignedProfileURL(ctx, userDgraph.ProfileKey),
		channelId,
		dgraphChannelInfo.Name,
		postID,
		body,
		tell.Only("email", eligibleNamedUserIDs),
	)
}

// namedIn is the people of mentions (by user uuid) whom html names by their
// own chip: its "<uuid>@<uid>" chips, matched by the graph uid, never by the
// uuid the sender's chip claims (MC-91). Pure.
func namedIn(html string, mentions []*dgraphStruct.DgraphUser) map[string]bool {
	named := map[string]bool{}
	uids, _ := helpers.GetMentions(html)
	if len(uids) == 0 {
		return named
	}
	chips := make(map[string]bool, len(uids))
	for _, uid := range uids {
		chips[uid] = true
	}
	for _, m := range mentions {
		if m != nil && m.Uid != "" && chips[m.Uid] {
			named[m.Uuid] = true
		}
	}
	return named
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

	// A reply sent to its channel too: the copy there says what it now says.
	mirrorReplyEdit(ctx, commentUUID.String(), commentInfo.HTMLText)

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

	helpers.GoSafeNamed("mqttBusiness.PublishPostComment", func() { mqttBusiness.PublishPostComment(&mqttPostCommentCount, rawDgraphCommentInfo.Post.Channel.Uuid) })

	return
}

func CreatePostComment(ctx context.Context, commentInfo *adapter.InputCreateOrUpdateCommentToPost, userInfo *userModels.UserInfo, mentionsDgraphUsersList []*dgraphStruct.DgraphUser, dgraphPostInfo *dgraphStruct.DgraphPost) (createdCommentData *adapter.OutputCreatePostCommentForPost, err error) {
	commentUUID := uuid.New()
	// CreateTimeOrNow respects the Slack-import backdating override; see
	// helpers.WithImportTimestamp. Falls back to time.Now() natively.
	currentTime := helpers.CreateTimeOrNow(ctx)
	zeroUnixTime := time.Time{}

	// A reply keeps only its writer's own uploads to the channel (F4).
	if commentInfo.MediaObj, err = attachmentBusiness.OwnFiles(ctx, userInfo.UserPostgresInfo.Id.String(), postgresStruct.ATTACHMENT_SRC_CHANNEL, dgraphPostInfo.Channel.Uuid, commentInfo.MediaObj); err != nil {
		return nil, err
	}

	var commentMentions []*dgraphStruct.DgraphUser

	for _, mention := range mentionsDgraphUsersList {
		commentMentions = append(commentMentions, &dgraphStruct.DgraphUser{
			DType: []string{"User"},
			Uid:   mention.Uid,
		})
	}

	// New nodes from the upload's own fields: never a node the request names.
	commentInfo.MediaObj = dgraphStruct.NewAttachments(commentInfo.MediaObj, userInfo.UserDgraphInfo.Uid, currentTime)

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

	_, err = commentBusiness.CreateCommentInAPost(ctx, &dgraphPost, userInfo, commentUUID, currentTime, dgraphPostInfo)

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
		helpers.GoSafeNamed("mqttBusiness.PublishPostComment", func() { mqttBusiness.PublishPostComment(&mqttPostCommentCount, dgraphPostInfo.Channel.Uuid) })

		// Push, email and the live Activity items, to one audience: the
		// thread's people who are in the channel now. A bulk import, which
		// writes years of replies, tells nobody.
		go commentBusiness.NotifyThreadReply(commentBusiness.ThreadReply{
			By:          &userInfo.UserDgraphInfo,
			Post:        dgraphPostInfo,
			CommentUUID: commentUUID.String(),
			HTML:        commentInfo.HTMLText,
			PlainText:   helpers.HTMLToPlainText(commentInfo.HTMLText),
			Mentioned:   mentionsDgraphUsersList,
			At:          currentTime,
		})

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
	refsBusiness.Saved(ctx, refsBusiness.Message, postUUID.String(), inputPostInfo.HTMLText, false)

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
	helpers.GoSafeNamed("domain.UpdatePostInOpenSearch", func() { domain.UpdatePostInOpenSearch(&openSearchPost) })

	// Suppress MQTT, AI re-embedding, and webhook dispatch during bulk
	// Slack import. The Postgres + Dgraph + OpenSearch writes above are
	// preserved so search and timelines stay consistent; only the live
	// fan-out is skipped to avoid pumping every channel member with
	// "post edited" events for thousands of historical edits.
	if helpers.IsBulkImport(ctx) {
		return
	}

	helpers.GoSafeNamed("mqttBusiness.PublishPost", func() { mqttBusiness.PublishPost(&mqttPost, channelId) })

	// Re-embed for AI Second Brain with updated content (async)
	ai.EmbedPostContent(plainText, postUUID.String(), channelId, "", postByUUID, "")

	// Reconcile any workspace-memory items captured from this post: refresh
	// the stored snapshot to the edited text (privacy + freshness), or drop
	// it if the edit emptied the message. Preserves the user's save.
	ai.InvalidateMemoryForEditedSourceAsync("post", postUUID.String(), plainText)

	helpers.GoSafeNamed("webhookBusiness.DispatchEvent", func() {
		webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "post.updated", map[string]interface{}{
			"post_id":    postUUID.String(),
			"channel_id": channelId,
		})
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

	helpers.GoSafeNamed("domain.DeletePostWithAttachmentsInOpenSearch", func() { domain.DeletePostWithAttachmentsInOpenSearch(openSearchPost, rawDgraphPost.MediaObj) })
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

	helpers.GoSafeNamed("mqttBusiness.PublishPost", func() { mqttBusiness.PublishPost(&mqttPost, channelId) })

	// Delete AI embedding (async)
	helpers.GoSafeNamed("business/Post/postBusiness:DeletePost", func() {
		bgCtx := context.Background()
		if err := ai.DeleteEmbedding(bgCtx, "post", rawDgraphPost.Uuid); err != nil {
			helpers.LogErrorWithContext(bgCtx, "business/DeletePost failed to delete AI embedding: %v", err)
		}
	})
	// Cascade into the Workspace Memory layer: drop any decisions /
	// commitments / questions the extractor derived from this post so
	// deleted content can't linger in memory or resurface via AI.
	ai.DeleteMemoryBySourceAsync("post", rawDgraphPost.Uuid)
	// Cascade soft-delete to AI embeddings (post + its comments)
	go globalSearchDomain.SyncCascadingDeletionInOpenSearch(
		[]string{"content_uuid", "post_uuid"},
		rawDgraphPost.Uuid,
		currentTime.Unix(),
		[]string{"ai_embeddings"},
		"cascade",
	)

	helpers.GoSafeNamed("webhookBusiness.DispatchEvent", func() {
		webhookBusiness.DispatchEvent(context.WithoutCancel(ctx), "post.deleted", map[string]interface{}{
			"post_id":    rawDgraphPost.Uuid,
			"channel_id": channelId,
		})
	})

	return
}

// GetOldPosts is the page of a channel's posts before edge (helpers.PageEdge).
func GetOldPosts(ctx context.Context, channelId string, edge helpers.PageEdge) (posts PostPagination, err error) {

	dgraphPosts, err := domain.GetDgraphOldPostFromDgraph(ctx, channelId, edge.Before())

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetOldPosts Failed to get posts form dgraph err: %+v",
			err)

		return
	}

	posts.Posts, posts.HasMore = helpers.PageFrom(dgraphPosts, postUUIDOf, edge, domain.POST_COUNT)

	return
}

func postUUIDOf(p *dgraphStruct.DgraphPost) string { return p.Uuid }

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

// GetNewPosts is the page of a channel's posts after edge (helpers.PageEdge).
func GetNewPosts(ctx context.Context, channelId string, edge helpers.PageEdge) (posts PostPagination, err error) {

	dgraphPosts, err := domain.GetDgraphNewPostFromDgraph(ctx, channelId, edge.After())

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetNewPosts Failed to get posts form dgraph err: %+v",
			err)

		return
	}

	posts.Posts, posts.HasMore = helpers.PageFrom(dgraphPosts, postUUIDOf, edge, domain.POST_COUNT)

	return
}

// GetLatestPosts is a channel's newest page. The query reads one post past
// the page, so whether there is more is what the graph holds: the page always
// said there was none, and a long channel looked as if it began at its tenth
// newest post.
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

	posts.HasMore = len(dgraphPosts) > domain.POST_COUNT

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

	// A reply sent to its channel too: its copy there goes with it.
	mirrorReplyDelete(ctx, commentUUID.String())

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

	helpers.GoSafeNamed("mqttBusiness.PublishPostComment", func() { mqttBusiness.PublishPostComment(&mqttPostCommentCount, rawDgraphComment.Post.Channel.Uuid) })

	return
}
