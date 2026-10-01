// Package botpost is the single shared path for posting a message AS the
// workspace automation bot. It lives in its own leaf package so BOTH the
// webhook engine and the workflow engine can depend on it without an import
// cycle (business/Post imports business/Webhook, so neither of those can host
// this shared helper).
//
// Every automated message funnels through here so automation has ONE
// consistent identity:
//   - authored by the shared bot user (its avatar + name render), and
//   - optionally badged with a per-integration label ("[Support Bot]") layered
//     on top, mirroring Slack's bot-user + per-message username override.
package botpost

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"strings"
	"time"

	commandAdapter "github.com/akashc777/OneCamp/adapter/Command"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	commentDomain "github.com/akashc777/OneCamp/domain/Comment"
	postDomain "github.com/akashc777/OneCamp/domain/Post"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/mqttInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	"github.com/google/uuid"
	"github.com/microcosm-cc/bluemonday"
)

// botHTMLPolicy sanitizes bot-generated HTML (same UGC policy as webhooks).
var botHTMLPolicy = bluemonday.UGCPolicy()

// Result carries identifiers the caller may want (e.g. to record a run or
// dispatch a follow-up event).
type Result struct {
	PostUUID    string
	CommentUUID string // set only for a comment (in-thread reply); empty for a post
	BotUserUUID string
	BotDgraphID string
	ChannelName string
	HTMLText    string // the final, sanitized + badged HTML that was posted
}

// PostToChannel writes a post to channelUUID authored by the shared automation
// bot.
//
// label is an optional per-integration display name ("Support Bot"). When set,
// the message is prefixed with a bold [label] badge so different automations
// are distinguishable even though they share the one bot user. When empty, the
// bot's own name renders with no prefix.
//
// text may be plain or HTML; it is always sanitized. Callers are responsible
// for authorizing the human who configured the automation; this function
// enforces channel existence and writes as the bot. It does NOT emit a
// "post.created" workspace event — the caller decides whether to fan out (the
// workflow engine deliberately does not, to avoid trigger loops).
func PostToChannel(ctx context.Context, channelUUID uuid.UUID, text, label string) (*Result, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, fmt.Errorf("text is required")
	}
	displayName := strings.TrimSpace(label)
	bot := userBusiness.GetAutomationBot(ctx)
	if bot == nil || bot.DgraphUID == "" {
		return nil, fmt.Errorf("automation bot is not available")
	}
	// badgeHTML prefixes a bold [label] so different automations sharing the one
	// bot user stay distinguishable in the body.
	return postBotMessage(ctx, bot, channelUUID, badgeHTML(trimmed, label), displayName)
}

// PostToChannelAs posts a message authored by the shared bot but DISPLAYED under
// displayName (e.g. an AI agent's name), with NO in-body [label] prefix — the
// author name alone carries the identity. Used for AI-agent mention replies so
// each agent reads as its own named, badged teammate ("Standup Bot") rather than
// the generic bot or a redundant "[Standup Bot] …" body prefix. Empty
// displayName falls back to the bot's own name.
func PostToChannelAs(ctx context.Context, channelUUID uuid.UUID, text, displayName string) (*Result, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, fmt.Errorf("text is required")
	}
	bot := userBusiness.GetAutomationBot(ctx)
	if bot == nil || bot.DgraphUID == "" {
		return nil, fmt.Errorf("automation bot is not available")
	}
	// Sanitize the body (no badge prefix); the author name carries the identity.
	return postBotMessage(ctx, bot, channelUUID, badgeHTML(trimmed, ""), strings.TrimSpace(displayName))
}

// PostToChannelAsBot posts a message authored by a SPECIFIC bot principal
// (e.g. an agent's own per-agent principal) rather than the shared automation
// bot. The principal's own name + avatar render (no in-body [label] prefix), so
// each agent reads as its own distinct, badged AI teammate. Used by the agent
// runner's mention reply. Empty/zero bot is an error.
func PostToChannelAsBot(ctx context.Context, channelUUID uuid.UUID, text string, bot *userBusiness.BotIdentity) (*Result, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, fmt.Errorf("text is required")
	}
	if bot == nil || bot.DgraphUID == "" {
		return nil, fmt.Errorf("bot principal is not available")
	}
	return postBotMessage(ctx, bot, channelUUID, badgeHTML(trimmed, ""), bot.Name)
}

// PostCommentToPostAsBot adds an in-thread reply — a Comment on an existing
// post — authored by a SPECIFIC bot principal (e.g. an AI agent's own
// principal). This is the Slack-style "reply in thread" path: an agent that is
// @mentioned on a channel message answers as a threaded comment on that message
// rather than a new top-level post, so the exchange stays grouped under the
// original message.
//
// It mirrors postBotMessage's guarantees and shape: it writes the Postgres
// comment row, the Dgraph comment node under the target post, indexes the
// comment in OpenSearch, and emits the live MQTT comment event — but NO
// workspace event, so it can never re-trigger an agent (loop-safe). A hard
// failure to persist rolls back the orphan row and returns an error so the
// caller can fall back to a top-level post.
func PostCommentToPostAsBot(ctx context.Context, postUUID uuid.UUID, text string, bot *userBusiness.BotIdentity) (*Result, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, fmt.Errorf("text is required")
	}
	if bot == nil || bot.DgraphUID == "" {
		return nil, fmt.Errorf("bot principal is not available")
	}

	// Resolve the target post (author + channel) AS the bot principal so the
	// comment attaches to the correct graph node and inherits the post's
	// channel for search indexing. A missing post is a hard error so the caller
	// falls back to a top-level channel post rather than dropping the reply.
	postInfo, err := postDomain.GetDgraphPostByUUID(ctx, postUUID.String(), bot.DgraphUID)
	if err != nil || postInfo == nil || postInfo.PostBy == nil || postInfo.Channel == nil {
		if err == nil {
			err = fmt.Errorf("target post not found")
		}
		return nil, err
	}

	commentUUID := uuid.New()
	now := helpers.CreateTimeOrNow(ctx)
	zeroUnixTime := time.Time{}
	formatted := badgeHTML(trimmed, "")

	// 1. Postgres comment row, owned by the bot user.
	if err := commentDomain.CreateComment(ctx, commentUUID, bot.UserID, now); err != nil {
		helpers.LogErrorWithContext(ctx, "botpost: create pg comment failed: %+v", err)
		return nil, err
	}

	// 2. Dgraph comment node under the target post, authored by the bot. No
	//    mentions node — a bot reply carries no @mentions of its own.
	dgraphPost := dgraphStruct.DgraphPost{
		Uid:  "uid(po)",
		Uuid: postUUID.String(),
		Comments: []*dgraphStruct.DgraphComment{
			{
				DType: []string{"Comment"},
				Uid:   "uid(co)",
				Uuid:  commentUUID.String(),
				Text:  formatted,
				Post: &dgraphStruct.DgraphPost{
					Uid: "uid(po)",
				},
				ContentAddedBy: &dgraphStruct.DgraphUser{
					Uid: postInfo.PostBy.Uid,
				},
				CreatedAt: &now,
				DeletedAt: &zeroUnixTime,
				CommentBy: &dgraphStruct.DgraphUser{
					Uid: bot.DgraphUID,
				},
			},
		},
	}
	if _, err := commentDomain.CreateDgraphCommentInAPost(ctx, &dgraphPost, commentUUID.String()); err != nil {
		helpers.LogErrorWithContext(ctx, "botpost: create dgraph comment failed: %+v", err)
		_ = commentDomain.HardDeleteCommentByUUID(ctx, commentUUID) // roll back orphan row
		return nil, fmt.Errorf("failed to create comment in graph")
	}

	// 3. Real-time MQTT so the thread updates live for anyone viewing it. The
	//    author's name + avatar carry the agent identity (same shape as a human
	//    comment event the FE already renders).
	var profileKey *string
	if bot.ProfileKey != "" {
		pk := bot.ProfileKey
		profileKey = &pk
	}
	mqttComment := mqttStruct.MqttPostComment{
		Type:           mqttStruct.TYPE_CREATE,
		PostUuid:       postUUID.String(),
		CommentUuid:    commentUUID.String(),
		HTMLText:       formatted,
		ChannelUuid:    postInfo.Channel.Uuid,
		CreatedAt:      &now,
		UserUuid:       bot.UUID,
		UserName:       bot.Name,
		UserProfileKey: profileKey,
		IsBot:          true,
	}
	go mqttBusiness.PublishPostComment(&mqttComment, postInfo.Channel.Uuid)

	// 4. OpenSearch index (plain text), scoped to the post's channel so the
	//    comment is discoverable by the same permission model as a human one.
	plain := helpers.RemoveHTMLTags(formatted)
	osComment := &openSearchStruct.OpenSearchComment{
		Uuid:                  commentUUID.String(),
		CommentBody:           plain,
		CommentByUserUuid:     bot.UUID,
		CommentByUserFullName: bot.Name,
		CommentByProfile:      profileKey,
		CommentChannelUuid:    postInfo.Channel.Uuid,
		CommentChannelName:    postInfo.Channel.Name,
		CommentPostUuid:       postUUID.String(),
		CommentCreatedAt:      now.Unix(),
	}
	go commentDomain.CreatePostCommentWithAttachmentsInOpenSearch(osComment, nil)

	return &Result{
		PostUUID:    postUUID.String(),
		CommentUUID: commentUUID.String(),
		BotUserUUID: bot.UUID,
		BotDgraphID: bot.DgraphUID,
		ChannelName: postInfo.Channel.Name,
		HTMLText:    formatted,
	}, nil
}

// EditCommentToPostAsBot edits an EXISTING in-thread comment authored by a bot
// principal, replacing its body with text. This is the "evolving status comment"
// primitive (async-mentions spec Task 1): a durable run posts one comment via
// PostCommentToPostAsBot, then edits it in place ("On it → working… → result")
// as the run advances, instead of trailing a new comment per stage.
//
// It mirrors UpdatePostComment's datastore shape (PG timestamp + Dgraph text +
// OpenSearch body) and emits an MQTT TYPE_UPDATE the FE already renders
// (updateChannelCommentByCommentUUID). Like the create path it emits NO
// workspace event, so an edit can never re-trigger an agent (loop-safe). The
// context is workflow-tagged defensively. Best-effort caller contract: on any
// error the caller falls back to a fresh comment, so a leftover status comment
// is at worst cosmetic.
func EditCommentToPostAsBot(ctx context.Context, postUUID, commentUUID uuid.UUID, text string, bot *userBusiness.BotIdentity) (*Result, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, fmt.Errorf("text is required")
	}
	if bot == nil || bot.DgraphUID == "" {
		return nil, fmt.Errorf("bot principal is not available")
	}
	if commentUUID == uuid.Nil || postUUID == uuid.Nil {
		return nil, fmt.Errorf("post and comment ids are required")
	}

	// Resolve the target post AS the bot principal to get the channel for MQTT
	// routing. A missing post is a hard error so the caller can fall back.
	postInfo, err := postDomain.GetDgraphPostByUUID(ctx, postUUID.String(), bot.DgraphUID)
	if err != nil || postInfo == nil || postInfo.Channel == nil {
		if err == nil {
			err = fmt.Errorf("target post not found")
		}
		return nil, err
	}

	// Never let a bot edit re-trigger workflows/agents (belt-and-suspenders;
	// the update path already emits no workspace event).
	ctx = helpers.WithWorkflowGenerated(ctx)

	now := helpers.CreateTimeOrNow(ctx)
	formatted := badgeHTML(trimmed, "")

	// 1. Postgres updated_at bump.
	if err := commentDomain.UpdateCommentByUUID(ctx, commentUUID, now); err != nil {
		helpers.LogErrorWithContext(ctx, "botpost: edit pg comment failed: %+v", err)
		return nil, err
	}

	// 2. Dgraph comment text (reset mentions — a bot status comment has none).
	dgraphComment := dgraphStruct.DgraphComment{
		Uid:   "uid(co)",
		Uuid:  commentUUID.String(),
		DType: []string{"Comment"},
		Text:  formatted,
		Mentions: &dgraphStruct.DgraphMentions{
			Uid:         "uid(me)",
			Comment:     &dgraphStruct.DgraphComment{Uid: "uid(co)"},
			CommentUuid: commentUUID.String(),
			Mentions:    nil,
		},
		UpdatedAt: &now,
	}
	if err := commentDomain.UpdateDgraphCommentAndResetMentions(ctx, &dgraphComment, commentUUID.String()); err != nil {
		helpers.LogErrorWithContext(ctx, "botpost: edit dgraph comment failed: %+v", err)
		return nil, fmt.Errorf("failed to update comment in graph")
	}

	// 3. OpenSearch body (keep the indexed text current).
	plain := helpers.RemoveHTMLTags(formatted)
	go commentDomain.UpdateCommentInOpenSearch(openSearchStruct.OpenSearchComment{
		Uuid:             commentUUID.String(),
		CommentBody:      plain,
		CommentUpdatedAt: now.Unix(),
	})

	// 4. Live MQTT edit event (same shape the FE already renders for a human edit).
	var profileKey *string
	if bot.ProfileKey != "" {
		pk := bot.ProfileKey
		profileKey = &pk
	}
	go mqttBusiness.PublishPostComment(&mqttStruct.MqttPostComment{
		Type:           mqttStruct.TYPE_UPDATE,
		PostUuid:       postUUID.String(),
		CommentUuid:    commentUUID.String(),
		HTMLText:       formatted,
		ChannelUuid:    postInfo.Channel.Uuid,
		UpdatedAt:      &now,
		UserUuid:       bot.UUID,
		UserName:       bot.Name,
		UserProfileKey: profileKey,
		IsBot:          true,
	}, postInfo.Channel.Uuid)

	return &Result{
		PostUUID:    postUUID.String(),
		CommentUUID: commentUUID.String(),
		BotUserUUID: bot.UUID,
		BotDgraphID: bot.DgraphUID,
		ChannelName: postInfo.Channel.Name,
		HTMLText:    formatted,
	}, nil
}

// postBotMessage is the shared core: persists + broadcasts a post authored by
// the given bot principal, displayed under displayName (falling back to the
// principal's own name). formattedHTML is the already-sanitized body.
func postBotMessage(ctx context.Context, bot *userBusiness.BotIdentity, channelUUID uuid.UUID, formattedHTML, displayName string) (*Result, error) {
	if bot == nil || bot.DgraphUID == "" {
		return nil, fmt.Errorf("automation bot is not available")
	}
	if strings.TrimSpace(formattedHTML) == "" {
		return nil, fmt.Errorf("text is required")
	}
	if displayName == "" {
		displayName = bot.Name
	}

	postUUID := uuid.New()
	now := helpers.CreateTimeOrNow(ctx)
	zeroUnixTime := time.Time{}

	// 1. Postgres row, owned by the bot user.
	if err := postDomain.CreatePost(ctx, postUUID, bot.UserID, channelUUID); err != nil {
		helpers.LogErrorWithContext(ctx, "botpost: create pg post failed: %+v", err)
		return nil, err
	}

	// 2. Resolve channel Dgraph node (using the bot's own Dgraph uid).
	channelDgraph, err := channelDomain.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID.String(), bot.DgraphUID)
	if err != nil || channelDgraph == nil {
		helpers.LogErrorWithContext(ctx, "botpost: channel lookup failed: %+v", err)
		_ = postDomain.HardDeletePostByUUIUD(ctx, postUUID) // roll back orphan row
		return nil, fmt.Errorf("channel not found")
	}

	formatted := formattedHTML

	// 3. Dgraph post node, authored by the bot.
	dgraphPost := dgraphStruct.DgraphPost{
		Uid:   "uid(po)",
		Uuid:  postUUID.String(),
		DType: []string{"Post"},
		Text:  formatted,
		PostBy: &dgraphStruct.DgraphUser{
			DType: []string{"User"},
			Uid:   bot.DgraphUID,
			Posts: []*dgraphStruct.DgraphPost{{Uid: "uid(po)", DType: []string{"Post"}}},
		},
		Channel: &dgraphStruct.DgraphChannel{
			DType: []string{"Channel"},
			Uid:   channelDgraph.Uid,
			Posts: []*dgraphStruct.DgraphPost{{Uid: "uid(po)", DType: []string{"Post"}}},
		},
		CreatedAt: &now,
		DeletedAt: &zeroUnixTime,
	}
	if _, err = postDomain.CreateOrUpdateDgraphPost(ctx, &dgraphPost); err != nil {
		helpers.LogErrorWithContext(ctx, "botpost: create dgraph post failed: %+v", err)
		_ = postDomain.HardDeletePostByUUIUD(ctx, postUUID)
		return nil, fmt.Errorf("failed to create message in graph")
	}

	// 4. Real-time MQTT — display name carries the identity; avatar is the
	//    bot's user_profile_object_key; is_bot drives the FE "AI" badge.
	var profileKey *string
	if bot.ProfileKey != "" {
		pk := bot.ProfileKey
		profileKey = &pk
	}
	mqttPost := mqttStruct.MqttPost{
		Type:             mqttStruct.TYPE_CREATE,
		PostHtmlText:     formatted,
		PostCreatedAt:    &now,
		PostByUserUuid:   bot.UUID,
		PostByProfileKey: profileKey,
		PostByUserName:   displayName,
		PostByIsBot:      true,
		PostChannelUuid:  channelUUID.String(),
		PostUuid:         postUUID.String(),
	}
	go mqttBusiness.PublishPost(&mqttPost, channelUUID.String())

	// 5. OpenSearch index.
	plain := helpers.RemoveHTMLTags(formatted)
	osPost := &openSearchStruct.OpenSearchPost{
		Uuid:               postUUID.String(),
		PostBody:           plain,
		PostChannelUuid:    channelUUID.String(),
		PostCreatedAt:      now.Unix(),
		PostByUserUuid:     bot.UUID,
		PostByProfile:      profileKey,
		PostByUserFullName: displayName,
		PostChannelName:    channelDgraph.Name,
	}
	go postDomain.CreatePostWithAttachmentsInOpenSearch(osPost, nil)

	return &Result{
		PostUUID:    postUUID.String(),
		BotUserUUID: bot.UUID,
		BotDgraphID: bot.DgraphUID,
		ChannelName: channelDgraph.Name,
		HTMLText:    formatted,
	}, nil
}

// badgeHTML sanitizes text and prefixes a bold [label] badge when label is
// non-empty. Handles both plain text and pre-formed HTML input.
//
// For unbadged plain text (the AI-agent / coworker reply path) it renders
// through streamHTML, the shared chart-aware plain->HTML renderer: every
// paragraph is escaped and wrapped in <p> (single newlines -> <br/>), and any
// closed, valid ```chart block becomes an inline chart embed node — identical
// to how the STREAMED reply path renders, so a non-streamed reply draws charts
// and multi-paragraph text the same way. The output is safe by construction
// (all non-chart text is escaped; a chart div carries only attribute-escaped
// JSON the FE reads as data), so it is not re-sanitized.
func badgeHTML(text, label string) string {
	label = strings.TrimSpace(label)
	if label == "" {
		if strings.HasPrefix(text, "<") {
			return botHTMLPolicy.Sanitize(text)
		}
		if rendered := streamHTML(text, false); rendered != "" {
			return rendered
		}
		return fmt.Sprintf("<p>%s</p>", html.EscapeString(text))
	}
	escapedLabel := html.EscapeString(label)
	if strings.HasPrefix(text, "<") {
		return fmt.Sprintf(`<p><strong>[%s]</strong></p>%s`, escapedLabel, botHTMLPolicy.Sanitize(text))
	}
	clean := botHTMLPolicy.Sanitize(helpers.RemoveHTMLTags(text))
	return fmt.Sprintf(`<p><strong>[%s]</strong> %s</p>`, escapedLabel, clean)
}

// PostEphemeralToUser delivers a bot message visible ONLY to the target user.
// Nothing is persisted (no posts row, no Dgraph node, no OpenSearch index) — it
// rides the same real-time ephemeral card channel that slash-command responses
// use, so the FE renders it with the existing handler and it disappears on
// reload. This is the right delivery for welcomes and moderation warnings,
// which should not clutter channel history.
//
// label badges the card with the per-integration name. Best-effort: if the
// user has no live client the card is simply not seen (ephemeral by
// definition). Returns an error only for invalid input or a hard
// publish-setup failure.
func PostEphemeralToUser(ctx context.Context, targetUserUUID, text, label string) error {
	if strings.TrimSpace(targetUserUUID) == "" {
		return fmt.Errorf("target user is required")
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return fmt.Errorf("text is required")
	}
	if mqttInit.MqttClient == nil {
		return fmt.Errorf("realtime transport unavailable")
	}

	// Resolve the bot identity for the card's display name/badge. The bot need
	// not exist as a poster here (nothing is persisted), but we keep the label
	// consistent with persisted bot posts.
	displayLabel := strings.TrimSpace(label)
	if displayLabel == "" {
		if bot := userBusiness.GetAutomationBot(ctx); bot != nil {
			displayLabel = bot.Name
		}
	}

	resp := &commandAdapter.CommandResponse{
		ResponseType: "ephemeral",
		Ephemeral:    true,
		Text:         badgeHTML(trimmed, displayLabel),
	}

	msg := mqttStruct.Message{
		Type: mqttStruct.MESSAGE_COMMAND_EPHEMERAL,
		Data: resp,
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "botpost: ephemeral marshal failed: %+v", err)
		return err
	}

	topic := helpers.GetMqttTopicForUserActivity(targetUserUUID)
	token := mqttInit.MqttClient.Publish(topic, 1, false, payload)
	go func() {
		defer func() { _ = recover() }()
		_ = token.Wait()
		if token.Error() != nil {
			helpers.MessageLogs.ErrorLog.Printf("botpost: ephemeral publish failed: %+v", token.Error())
		}
	}()
	return nil
}

// LabelledHTML renders text (plain or HTML) as a bot message body led by a
// bold [label]: how a relayed message names the person it came from while the
// bot principal stays its author.
func LabelledHTML(text, label string) string {
	return badgeHTML(strings.TrimSpace(text), label)
}
