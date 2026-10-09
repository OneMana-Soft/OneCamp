package business

import (
	"context"
	"fmt"
	"strings"

	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// reminderChannel.go — channel-targeted /remind delivery.
//
// A channel reminder is a post, written in its owner's name, so it obeys the
// rules every post does (business/Send.PrepareChannelPost: the channel is
// live, the owner is in it, and only its admins post in an announcement
// channel). They are checked when the reminder is set and again each time it
// fires, by the same code: someone who has left the channel, or whose channel
// was archived or made announcement-only since, doesn't post into it.
//
// resolveChannelForReminder runs at command time: it resolves "#name", or
// "#here" (the channel the command was typed in), to a channel the person may
// post in, and returns its UUID to persist on the job.
//
// postChannelReminder runs at fire time (inside the scheduler worker): it posts
// a visible message into the channel authored by the user who set the reminder,
// through the same path as a post sent from the composer (PG + Dgraph +
// OpenSearch + MQTT + notifications). This is the Slack-parity behavior for
// /remind #channel.

func resolveChannelForReminder(ctx context.Context, cc CommandContext, channelName string) (string, error) {
	name := strings.TrimPrefix(strings.TrimSpace(channelName), "#")
	if name == "" {
		return "", fmt.Errorf("empty channel name")
	}

	// "#here"/"#channel"/"#this" → the channel the command was invoked in. The
	// id comes with the request, so it is checked like any other channel.
	var channelID string
	if cc.ChannelID != nil && (strings.EqualFold(name, "here") || strings.EqualFold(name, "this") || strings.EqualFold(name, "channel")) {
		channelID = cc.ChannelID.String()
	} else {
		ch, err := channelDomain.GetChannelByName(ctx, name)
		if err != nil || ch == nil {
			return "", fmt.Errorf("channel not found")
		}
		channelID = ch.Id.String()
	}

	if _, err := prepareChannelReminder(ctx, &cc.User, channelID, ""); err != nil {
		return "", err
	}
	return channelID, nil
}

// prepareChannelReminder runs the rules for posting text as user in the
// channel. Nothing is written until the result is committed.
func prepareChannelReminder(ctx context.Context, user *userModels.UserInfo, channelID, text string) (*sendBusiness.ChannelPost, error) {
	safeText := helpers.RemoveHTMLTags(text)
	return sendBusiness.PrepareChannelPost(ctx, user, &postAdapter.InputCreateOrUpdatePostInfo{
		HTMLText:    fmt.Sprintf("<p>⏰ <strong>Reminder:</strong> %s</p>", safeText),
		ChannelUuid: channelID,
	})
}

// postChannelReminder posts the reminder as a visible message into the target
// channel, authored by the reminder's creator, if the rules still let them
// post there. A refusal is a sendBusiness.Rejection.
func postChannelReminder(ctx context.Context, p reminderPayload) error {
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
	userInfo := &userModels.UserInfo{
		UserPostgresInfo: *postgresUser,
		UserDgraphInfo:   *dgraphUser,
	}

	post, err := prepareChannelReminder(ctx, userInfo, p.TargetID, p.Text)
	if err != nil {
		return err
	}
	_, err = post.Commit(ctx)
	return err
}
