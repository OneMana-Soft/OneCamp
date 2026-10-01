package business

// Slack → OneCamp.

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Post"
	botpost "github.com/akashc777/OneCamp/business/BotPost"
	commentBusiness "github.com/akashc777/OneCamp/business/Comment"
	postBusiness "github.com/akashc777/OneCamp/business/Post"
	slackImport "github.com/akashc777/OneCamp/business/SlackImport"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/SlackBridge"
	"github.com/google/uuid"
)

// Response is what the events endpoint answers Slack.
type Response struct {
	Status      int
	ContentType string
	Body        string
}

var reChallenge = regexp.MustCompile(`^[A-Za-z0-9_\-]{1,200}$`)

type envelope struct {
	Type      string          `json:"type"`
	Challenge string          `json:"challenge"`
	TeamID    string          `json:"team_id"`
	EventID   string          `json:"event_id"`
	Event     json.RawMessage `json:"event"`
}

type slackFile struct {
	Name      string `json:"name"`
	Title     string `json:"title"`
	Permalink string `json:"permalink"`
}

type messageEvent struct {
	Type            string        `json:"type"`
	Subtype         string        `json:"subtype"`
	Channel         string        `json:"channel"`
	User            string        `json:"user"`
	BotID           string        `json:"bot_id"`
	Username        string        `json:"username"`
	Text            string        `json:"text"`
	Ts              string        `json:"ts"`
	ThreadTs        string        `json:"thread_ts"`
	DeletedTs       string        `json:"deleted_ts"`
	Files           []slackFile   `json:"files"`
	Message         *messageEvent `json:"message"`
	PreviousMessage *messageEvent `json:"previous_message"`
}

// HandleEvents answers one Events API delivery. Verification happens here;
// the work runs after Slack has its 200, since Slack retries anything slower
// than three seconds.
func HandleEvents(ctx context.Context, body []byte, timestamp, signature string) Response {
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return Response{Status: 400}
	}
	st, err := loadState(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "SlackBridge events: load: %v", err)
		return Response{Status: 500}
	}

	if env.Type == "url_verification" {
		// Slack checks the Request URL while the admin is still creating the
		// app, before OneCamp can know its signing secret. Echoing the
		// challenge reveals nothing and changes nothing; once connected the
		// signature is required like everywhere else.
		if st != nil && !webhookBusiness.VerifySlackSignature(st.secret, body, timestamp, signature) {
			return Response{Status: 401}
		}
		if !reChallenge.MatchString(env.Challenge) {
			return Response{Status: 400}
		}
		return Response{Status: 200, ContentType: "text/plain", Body: env.Challenge}
	}

	if st == nil {
		// Not connected: acknowledge so Slack stops retrying a stale app.
		return Response{Status: 200}
	}
	if !webhookBusiness.VerifySlackSignature(st.secret, body, timestamp, signature) {
		return Response{Status: 401}
	}
	if env.Type != "event_callback" || env.TeamID != st.bridge.TeamID {
		return Response{Status: 200}
	}
	var ev messageEvent
	if err := json.Unmarshal(env.Event, &ev); err != nil || ev.Type != "message" {
		return Response{Status: 200}
	}
	link, linked := st.bySlack[ev.Channel]
	if !linked {
		return Response{Status: 200}
	}

	work := context.WithoutCancel(ctx)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				helpers.MessageLogs.ErrorLog.Printf("SlackBridge inbound panic: %v", r)
			}
		}()
		work, cancel := context.WithTimeout(work, 2*time.Minute)
		defer cancel()
		receive(work, st, link, &ev)
	}()
	return Response{Status: 200}
}

// fromUs reports whether a message was written by the bridge itself.
func fromUs(st *state, m *messageEvent) bool {
	return (m.BotID != "" && m.BotID == st.bridge.BotID) || (m.User != "" && m.User == st.bridge.BotUserID)
}

func receive(ctx context.Context, st *state, link model.Link, ev *messageEvent) {
	switch ev.Subtype {
	case "", "thread_broadcast", "file_share", "me_message", "bot_message":
		if !fromUs(st, ev) {
			receiveNew(ctx, st, link, ev)
		}
	case "message_changed":
		// Slack also sends message_changed when it unfurls a link or a
		// reply count moves; only a change of text is an edit.
		if ev.PreviousMessage != nil && ev.Message != nil && ev.PreviousMessage.Text == ev.Message.Text {
			return
		}
		if ev.Message != nil && !fromUs(st, ev.Message) {
			receiveEdit(ctx, st, link, ev.Message)
		}
	case "message_deleted":
		if ev.DeletedTs != "" {
			receiveDelete(ctx, link, ev.DeletedTs)
		}
	}
}

// messageHTML renders a Slack message as the body of a OneCamp post, led by
// its sender's name.
func messageHTML(ctx context.Context, st *state, m *messageEvent) (string, string) {
	body := paragraphs(slackImport.RenderMrkdwn(ctx, m.Text, func(id string) string {
		return senderName(ctx, st, id)
	}))
	var files []string
	for _, f := range m.Files {
		label := f.Title
		if label == "" {
			label = f.Name
		}
		if label == "" {
			label = "file"
		}
		if strings.HasPrefix(f.Permalink, "https://") {
			files = append(files, `<a href="`+helpers.EscapeHTML(f.Permalink)+`" target="_blank" rel="noopener noreferrer">`+helpers.EscapeHTML(label)+`</a>`)
		} else {
			files = append(files, helpers.EscapeHTML(label))
		}
	}
	if len(files) > 0 {
		body += "<p>Shared in Slack: " + strings.Join(files, ", ") + "</p>"
	}
	name := ""
	if m.User != "" {
		name = senderName(ctx, st, m.User)
	}
	if name == "" {
		name = strings.TrimSpace(m.Username)
	}
	if name == "" {
		name = "Someone in Slack"
	}
	return body, name
}

func receiveNew(ctx context.Context, st *state, link model.Link, ev *messageEvent) {
	body, name := messageHTML(ctx, st, ev)
	if strings.TrimSpace(helpers.RemoveHTMLTags(body)) == "" {
		return
	}
	bot, err := userBusiness.EnsureSlackBridgeBot(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "SlackBridge inbound: principal: %v", err)
		return
	}
	claimed, err := model.ClaimSlackMessage(ctx, ev.Channel, ev.Ts, link.ChannelUUID)
	if err != nil || !claimed {
		if err != nil {
			helpers.LogErrorWithContext(ctx, "SlackBridge inbound: claim: %v", err)
		}
		return
	}
	html := botpost.LabelledHTML(body, name)

	// A thread reply becomes a comment on the post its thread started from,
	// when that post came across the bridge.
	if ev.ThreadTs != "" && ev.ThreadTs != ev.Ts {
		if parent, _ := model.MessageBySlackTs(ctx, ev.Channel, ev.ThreadTs); parent != nil && parent.PostUUID != nil {
			res, err := botpost.PostCommentToPostAsBot(ctx, *parent.PostUUID, html, bot)
			if err == nil {
				if commentUUID, perr := uuid.Parse(res.CommentUUID); perr == nil {
					_ = model.CompleteClaim(ctx, ev.Channel, ev.Ts, *parent.PostUUID, &commentUUID)
				}
				return
			}
			helpers.LogErrorWithContext(ctx, "SlackBridge inbound: thread reply, posting at top level: %v", err)
		}
	}

	res, err := botpost.PostToChannelAsBot(ctx, link.ChannelUUID, html, bot)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "SlackBridge inbound: post: %v", err)
		_ = model.ReleaseClaim(ctx, ev.Channel, ev.Ts)
		return
	}
	if postUUID, perr := uuid.Parse(res.PostUUID); perr == nil {
		_ = model.CompleteClaim(ctx, ev.Channel, ev.Ts, postUUID, nil)
	}
}

func receiveEdit(ctx context.Context, st *state, link model.Link, m *messageEvent) {
	mapped, err := model.MessageBySlackTs(ctx, link.SlackChannelID, m.Ts)
	if err != nil || mapped == nil || mapped.Origin != model.OriginSlack || mapped.PostUUID == nil {
		return
	}
	body, name := messageHTML(ctx, st, m)
	if strings.TrimSpace(helpers.RemoveHTMLTags(body)) == "" {
		return
	}
	html := botpost.LabelledHTML(body, name)
	bot, err := userBusiness.EnsureSlackBridgeBot(ctx)
	if err != nil {
		return
	}
	if mapped.CommentUUID != nil {
		if _, err := botpost.EditCommentToPostAsBot(ctx, *mapped.PostUUID, *mapped.CommentUUID, html, bot); err != nil {
			helpers.LogErrorWithContext(ctx, "SlackBridge inbound: edit reply: %v", err)
		}
		return
	}
	if err := postBusiness.UpdatePost(ctx, &adapter.InputCreateOrUpdatePostInfo{HTMLText: html},
		nil, *mapped.PostUUID, link.ChannelUUID.String(), bot.UUID); err != nil {
		helpers.LogErrorWithContext(ctx, "SlackBridge inbound: edit: %v", err)
	}
}

func receiveDelete(ctx context.Context, link model.Link, ts string) {
	mapped, err := model.MessageBySlackTs(ctx, link.SlackChannelID, ts)
	if err != nil || mapped == nil || mapped.PostUUID == nil {
		return
	}
	// Forget it first, so the post.deleted this causes finds nothing to send
	// back to Slack.
	_ = model.DeleteMessage(ctx, link.SlackChannelID, ts)
	if mapped.Origin != model.OriginSlack {
		return
	}
	bot, err := userBusiness.EnsureSlackBridgeBot(ctx)
	if err != nil {
		return
	}
	if mapped.CommentUUID != nil {
		raw, err := commentBusiness.GetDgraphCommentInfoByUUID(ctx, mapped.CommentUUID.String())
		if err != nil || raw == nil || raw.Post == nil || raw.Post.Channel == nil {
			return
		}
		if err := postBusiness.DeleteCommentOnPost(ctx, *mapped.CommentUUID, raw, bot.UUID); err != nil {
			helpers.LogErrorWithContext(ctx, "SlackBridge inbound: delete reply: %v", err)
		}
		return
	}
	raw := &dgraphStruct.DgraphPost{Uuid: mapped.PostUUID.String(), PostBy: &dgraphStruct.DgraphUser{Uuid: bot.UUID}}
	if err := postBusiness.DeletePost(ctx, raw, link.ChannelUUID.String()); err != nil {
		helpers.LogErrorWithContext(ctx, "SlackBridge inbound: delete: %v", err)
	}
}

// Slack names are looked up once an hour per person, not per message.
const nameTTL = time.Hour

type cachedName struct {
	name string
	at   time.Time
}

var (
	namesMu sync.Mutex
	names   = map[string]cachedName{}
)

// senderName is the Slack display name for a user id, "" when unknown.
func senderName(ctx context.Context, st *state, id string) string {
	namesMu.Lock()
	if c, ok := names[id]; ok && time.Since(c.at) < nameTTL {
		namesMu.Unlock()
		return c.name
	}
	namesMu.Unlock()
	u, err := st.client().userInfo(ctx, id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "SlackBridge users.info %s: %v", id, err)
		return ""
	}
	name := u.displayName()
	namesMu.Lock()
	if len(names) > 10000 {
		names = map[string]cachedName{}
	}
	names[id] = cachedName{name: name, at: time.Now()}
	namesMu.Unlock()
	return name
}
