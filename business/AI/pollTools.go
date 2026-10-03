package business

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	pollBusiness "github.com/akashc777/OneCamp/business/Poll"
	ai "github.com/akashc777/OneCamp/services/AI"
)

func init() {
	ai.RegisterExecutor("create_poll", executeCreatePoll)
	ai.RegisterExecutor("read_poll", executeReadPoll)
}

// executeCreatePoll posts a poll as the acting user, under the same channel
// rules as send_message (the poll business layer checks them).
func executeCreatePoll(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	user, err := getUserInfoForExecutor(ctx, userUUID)
	if err != nil {
		return "", nil, fmt.Errorf("failed to look up user: %w", err)
	}
	hours := 0
	if h := strings.TrimSpace(action.Params["open_hours"]); h != "" {
		if hours, err = strconv.Atoi(h); err != nil {
			return "", nil, fmt.Errorf("open_hours must be a whole number of hours")
		}
	}
	v, err := pollBusiness.Create(ctx, user, pollBusiness.NewPoll{
		ChannelUUID: strings.TrimSpace(action.Params["channel_uuid"]),
		Question:    action.Params["question"],
		Options:     pollBusiness.SplitOptions(action.Params["options"]),
		Multiple:    strings.EqualFold(strings.TrimSpace(action.Params["multiple"]), "true"),
		OpenHours:   hours,
		AIGenerated: true,
	})
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("✅ Poll posted: %s (poll_uuid: %s)", v.Question, v.ID),
		map[string]string{"tool": "create_poll", "poll_uuid": v.ID, "channel_uuid": v.Channel, "post_uuid": v.PostID}, nil
}

// executeReadPoll reports a poll's results, if the user can see its channel.
func executeReadPoll(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	user, err := getUserInfoForExecutor(ctx, userUUID)
	if err != nil {
		return "", nil, fmt.Errorf("failed to look up user: %w", err)
	}
	v, err := pollBusiness.Get(ctx, user, strings.TrimSpace(action.Params["poll_uuid"]))
	if err != nil {
		return "", nil, err
	}
	return pollBusiness.Summary(v), nil, nil
}
