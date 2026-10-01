package business

// Moderation actions for the workflow engine: delete_message and
// flag_to_channel. These are privileged, so each RE-VERIFIES the workflow
// owner's authority at execution time — owning a workflow never grants a
// capability the owner doesn't personally have. This mirrors the trust model
// used everywhere else (the bot posts as itself, but moderation acts on the
// owner's authority).

import (
	"context"
	"fmt"
	"html"
	"strings"

	botpost "github.com/akashc777/OneCamp/business/BotPost"
	postBusiness "github.com/akashc777/OneCamp/business/Post"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	postDomain "github.com/akashc777/OneCamp/domain/Post"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// moderateDeleteMessage soft-deletes the triggering post IFF the workflow owner
// is a moderator/admin of the post's channel. The deletion runs through the
// standard post-delete path (MQTT, search, AI-memory cascade), so a
// workflow-removed message behaves exactly like an admin removing it by hand.
func (cw *compiledWorkflow) moderateDeleteMessage(ctx context.Context, postUUID, channelID string) error {
	// Resolve the owner's Dgraph uid (the post/channel queries are uid-keyed).
	owner, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, cw.createdBy)
	if err != nil || owner == nil || owner.Uid == "" {
		return fmt.Errorf("delete_message: cannot resolve workflow owner")
	}

	// Fetch the post WITH the owner's channel-membership/moderator flags.
	post, err := postDomain.GetDgraphPostByUUIDWithChannelMembers(ctx, postUUID, owner.Uid)
	if err != nil {
		return fmt.Errorf("delete_message: load post failed: %w", err)
	}
	if post == nil || post.Uuid == "" {
		// Already gone (deleted/raced) — nothing to do.
		return nil
	}
	if post.Channel == nil {
		return fmt.Errorf("delete_message: post has no channel context")
	}
	if post.PostBy == nil {
		return fmt.Errorf("delete_message: post has no author context")
	}

	// PERMISSION GATE: the owner must be a channel moderator/admin. ch_is_admin
	// is a count of moderator edges matching the owner uid.
	if post.Channel.IsAdmin == 0 {
		return fmt.Errorf("delete_message: workflow owner is not a moderator of this channel")
	}

	// Guard: don't try to re-delete an already soft-deleted post. The fetch
	// above doesn't filter deleted posts, so check the timestamp if present.
	if post.DeletedAt != nil && !post.DeletedAt.IsZero() && post.DeletedAt.Year() > 1971 {
		return nil
	}

	if err := postBusiness.DeletePost(ctx, post, channelID); err != nil {
		return fmt.Errorf("delete_message: delete failed: %w", err)
	}
	helpers.LogInfoWithContext(ctx, "Workflow %s deleted message %s in channel %s (owner-moderated)", cw.id, postUUID, channelID)
	return nil
}

// moderateFlagToChannel posts a copy + backlink of the flagged message into a
// review channel, authored by the automation bot. The workflow owner must be a
// member/moderator of the target review channel (so a member can't route
// content into a channel they can't see).
func (cw *compiledWorkflow) moderateFlagToChannel(ctx context.Context, targetChannelID string, ev triggerEvent) error {
	targetUUID, perr := uuid.Parse(strings.TrimSpace(targetChannelID))
	if perr != nil {
		return fmt.Errorf("flag_to_channel: invalid target channel id %q", targetChannelID)
	}

	// Permission: owner must be able to access the review channel.
	owner, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, cw.createdBy)
	if err != nil || owner == nil || owner.Uid == "" {
		return fmt.Errorf("flag_to_channel: cannot resolve workflow owner")
	}
	reviewCh, err := userChannelAccess(ctx, owner.Uid, targetUUID.String())
	if err != nil {
		return fmt.Errorf("flag_to_channel: channel lookup failed: %w", err)
	}
	if !reviewCh {
		return fmt.Errorf("flag_to_channel: workflow owner has no access to the review channel")
	}

	// Build the flag notice as HTML so the quote + backlink render cleanly.
	// botpost sanitizes via bluemonday (allows <a>, <blockquote>, <br>), so we
	// must HTML-escape the user-supplied original text ourselves before
	// embedding it.
	original := strings.TrimSpace(ev.text)
	if original == "" {
		original = "(no text content)"
	}
	quoted := html.EscapeString(truncate(stripNewlines(original), 500))

	var sb strings.Builder
	sb.WriteString("<p>🚩 <strong>Flagged message</strong></p>")
	sb.WriteString("<blockquote>")
	sb.WriteString(quoted)
	sb.WriteString("</blockquote>")
	if ev.postUUID != "" && ev.channelID != "" {
		link := fmt.Sprintf("/app/channel/%s/%s", ev.channelID, ev.postUUID)
		sb.WriteString(fmt.Sprintf(`<p><a href="%s">View original message</a></p>`, link))
	}

	label := cw.botName
	if label == "" {
		label = cw.name
	}
	_, err = botpost.PostToChannel(ctx, targetUUID, sb.String(), label)
	return err
}

// stripNewlines collapses newlines so a flagged quote stays on one block line.
func stripNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return s
}

// userChannelAccess reports whether the user (by Dgraph uid) is a member or
// moderator of the channel.
func userChannelAccess(ctx context.Context, userDgraphUID, channelUUID string) (bool, error) {
	ch, err := channelDomain.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, userDgraphUID)
	if err != nil {
		return false, err
	}
	if ch == nil {
		return false, nil
	}
	return ch.IsMember > 0 || ch.IsAdmin > 0, nil
}
