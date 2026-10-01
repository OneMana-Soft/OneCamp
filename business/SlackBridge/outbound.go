package business

// OneCamp → Slack.

import (
	"context"
	"errors"
	"strings"
	"sync"

	postBusiness "github.com/akashc777/OneCamp/business/Post"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/SlackBridge"
	"github.com/google/uuid"
)

var startOnce sync.Once

// Start subscribes the bridge to workspace events. Inert until Slack is
// connected and a channel linked; safe to call more than once.
func Start() {
	startOnce.Do(func() { webhookBusiness.RegisterEventListener(handleEvent) })
}

func str(data map[string]interface{}, key string) string {
	s, _ := data[key].(string)
	return s
}

func handleEvent(ctx context.Context, eventType string, data map[string]interface{}) {
	switch eventType {
	case "post.created", "post.updated", "post.deleted", "post.comment.created":
	default:
		return
	}
	channelUUID, err := uuid.Parse(str(data, "channel_id"))
	if err != nil {
		return
	}
	st, err := loadState(ctx)
	if err != nil || st == nil {
		return
	}
	link, linked := st.byChannel[channelUUID]
	if !linked {
		return
	}
	postUUID, err := uuid.Parse(str(data, "post_id"))
	if err != nil {
		return
	}
	switch eventType {
	case "post.created":
		sendPost(ctx, st, link, postUUID, authorOf(data))
	case "post.updated":
		sendEdit(ctx, st, link, postUUID)
	case "post.deleted":
		sendDelete(ctx, st, link, postUUID)
	case "post.comment.created":
		commentUUID, err := uuid.Parse(str(data, "comment_uuid"))
		if err != nil {
			return
		}
		sendReply(ctx, st, link, postUUID, commentUUID, authorOf(data), str(data, "text"))
	}
}

// authorOf is the name a OneCamp message is shown under in Slack.
func authorOf(data map[string]interface{}) string {
	for _, k := range []string{"author_name", "bot_name"} {
		if s := strings.TrimSpace(str(data, k)); s != "" {
			return s
		}
	}
	return "OneCamp"
}

// postMrkdwn is a post's current text as Slack mrkdwn.
func postMrkdwn(ctx context.Context, postUUID uuid.UUID) string {
	p, err := postBusiness.GetDgraphPostOnlyTextDgraph(ctx, postUUID.String())
	if err != nil || p == nil {
		return ""
	}
	return htmlToMrkdwn(p.Text)
}

func sendPost(ctx context.Context, st *state, link model.Link, postUUID uuid.UUID, author string) {
	text := postMrkdwn(ctx, postUUID)
	if text == "" {
		return
	}
	ts, err := st.client().postMessage(ctx, link.SlackChannelID, text, author, "")
	noteDelivery(ctx, err, link.SlackChannelName)
	if err != nil {
		return
	}
	if err := model.RecordOneCampMessage(ctx, model.Message{
		SlackChannelID: link.SlackChannelID, SlackTs: ts, ChannelUUID: link.ChannelUUID, PostUUID: &postUUID,
	}); err != nil {
		helpers.LogErrorWithContext(ctx, "SlackBridge outbound: record: %v", err)
	}
}

func sendReply(ctx context.Context, st *state, link model.Link, postUUID, commentUUID uuid.UUID, author, plain string) {
	parent, err := model.MessageByPost(ctx, postUUID)
	if err != nil || parent == nil {
		// The thread started before the channels were linked: Slack has no
		// message to hang the reply on.
		return
	}
	text := slackEscape(strings.TrimSpace(plain))
	if text == "" {
		return
	}
	ts, err := st.client().postMessage(ctx, link.SlackChannelID, text, author, parent.SlackTs)
	noteDelivery(ctx, err, link.SlackChannelName)
	if err != nil {
		return
	}
	_ = model.RecordOneCampMessage(ctx, model.Message{
		SlackChannelID: link.SlackChannelID, SlackTs: ts, ChannelUUID: link.ChannelUUID,
		PostUUID: &postUUID, CommentUUID: &commentUUID,
	})
}

func sendEdit(ctx context.Context, st *state, link model.Link, postUUID uuid.UUID) {
	m, err := model.MessageByPost(ctx, postUUID)
	if err != nil || m == nil || m.Origin != model.OriginOneCamp {
		return
	}
	text := postMrkdwn(ctx, postUUID)
	if text == "" {
		return
	}
	err = st.client().update(ctx, m.SlackChannelID, m.SlackTs, text)
	noteDelivery(ctx, err, link.SlackChannelName)
}

func sendDelete(ctx context.Context, st *state, link model.Link, postUUID uuid.UUID) {
	m, err := model.MessageByPost(ctx, postUUID)
	if err != nil || m == nil {
		return
	}
	_ = model.DeleteMessage(ctx, m.SlackChannelID, m.SlackTs)
	if m.Origin != model.OriginOneCamp {
		// A Slack person's message removed in OneCamp stays in Slack: the app
		// cannot delete what someone else wrote there.
		return
	}
	err = st.client().delete(ctx, m.SlackChannelID, m.SlackTs)
	var se *SlackError
	if err != nil && errors.As(err, &se) && se.Code == "message_not_found" {
		err = nil
	}
	noteDelivery(ctx, err, link.SlackChannelName)
}
