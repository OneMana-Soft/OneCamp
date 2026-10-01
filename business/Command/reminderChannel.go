package business

import (
	"context"
	"fmt"
	"strings"

	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	postBusiness "github.com/akashc777/OneCamp/business/Post"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// reminderChannel.go — channel-targeted /remind delivery.
//
// resolveChannelForReminder runs at command time (we have the invoking user's
// context): it resolves "#name" → channel UUID, verifies the user is a member
// or admin of that channel, and returns the UUID to persist on the job. We
// prefer the channel the command was invoked in when its name matches, so
// "/remind #here ..." behaves intuitively.
//
// postChannelReminder runs at fire time (inside the scheduler worker): it posts
// a visible message into the channel authored by the user who set the reminder,
// reusing the standard Post business path (PG + Dgraph + OpenSearch + MQTT +
// notifications). This is the Slack-parity behavior for /remind #channel.

func resolveChannelForReminder(ctx context.Context, cc CommandContext, channelName string) (string, error) {
	name := strings.TrimPrefix(strings.TrimSpace(channelName), "#")
	if name == "" {
		return "", fmt.Errorf("empty channel name")
	}

	// "#here"/"#channel"/"#this" → the channel the command was invoked in.
	if cc.ChannelID != nil && (strings.EqualFold(name, "here") || strings.EqualFold(name, "this") || strings.EqualFold(name, "channel")) {
		return cc.ChannelID.String(), nil
	}

	ch, err := channelDomain.GetChannelByName(ctx, name)
	if err != nil || ch == nil {
		return "", fmt.Errorf("channel not found")
	}

	// Verify the invoking user can post to this channel (member or admin).
	dgraphCh, err := channelDomain.GetBasicDgraphChannelInfoByUUID(ctx, ch.Id.String(), cc.User.UserDgraphInfo.Uid)
	if err != nil || dgraphCh == nil {
		return "", fmt.Errorf("channel access check failed")
	}
	if dgraphCh.IsMember == 0 && dgraphCh.IsAdmin == 0 {
		return "", fmt.Errorf("not a member of #%s", name)
	}
	return ch.Id.String(), nil
}

// postChannelReminder posts the reminder as a visible message into the target
// channel, authored by the reminder's creator.
func postChannelReminder(ctx context.Context, p reminderPayload) error {
	channelParsedUUID, err := uuid.Parse(p.TargetID)
	if err != nil {
		return fmt.Errorf("invalid channel id: %w", err)
	}

	// Build the full UserInfo (postgres + dgraph) for the creator.
	creatorUUID, err := uuid.Parse(p.CreatedBy)
	if err != nil {
		return fmt.Errorf("invalid creator id: %w", err)
	}
	postgresUser, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, creatorUUID)
	if err != nil || postgresUser == nil {
		return fmt.Errorf("creator not found")
	}
	dgraphUser, err := userDomain.GetDgraphUserInfoByUUID(ctx, p.CreatedBy)
	if err != nil || dgraphUser == nil {
		return fmt.Errorf("creator dgraph not found")
	}

	// Re-resolve channel against the creator so membership/permission is
	// re-checked at fire time (membership may have changed since scheduling).
	dgraphChannel, err := channelDomain.GetDgraphChannelInfoByUUID(ctx, channelParsedUUID.String(), dgraphUser.Uid)
	if err != nil || dgraphChannel == nil {
		return fmt.Errorf("channel not found at fire time")
	}

	safeText := helpers.RemoveHTMLTags(p.Text)
	htmlText := fmt.Sprintf("<p>⏰ <strong>Reminder:</strong> %s</p>", safeText)

	postInfo := &postAdapter.InputCreateOrUpdatePostInfo{
		HTMLText:    htmlText,
		ChannelUuid: channelParsedUUID.String(),
		ChannelUUID: channelParsedUUID,
	}

	userInfo := &userModels.UserInfo{
		UserPostgresInfo: *postgresUser,
		UserDgraphInfo:   *dgraphUser,
	}
	_, err = postBusiness.CreatePost(ctx, postInfo, userInfo, nil, dgraphChannel)
	return err
}
