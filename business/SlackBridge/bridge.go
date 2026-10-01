// Package business is the live Slack bridge: a linked Slack channel and
// OneCamp channel carry one conversation. Slack messages arrive through the
// Events API (POST /slack/events) and are posted by the "Slack" principal,
// each led by its sender's name; OneCamp messages go out with chat.postMessage
// under their author's name. Thread replies, edits and deletions follow.
//
// Loop safety is structural rather than filtered:
//   - Messages the bridge writes into OneCamp go through botpost, which emits
//     no workspace event, so they never reach the outbound listener.
//   - Messages the bridge writes into Slack carry our bot_id, and inbound
//     skips its own bot.
//   - An edit or deletion only ever travels AWAY from the side that wrote the
//     message (slack_bridge_messages.origin).
//
// Slack people take no OneCamp seat: they are never OneCamp users.
package business

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/SlackBridge"
	authService "github.com/akashc777/OneCamp/services/Auth"
	"github.com/google/uuid"
)

// EventsPath is where Slack delivers events. Public: the signature is the
// credential.
const EventsPath = "/slack/events"

// stateTTL bounds how stale another replica's view of the links can be.
const stateTTL = 30 * time.Second

// state is the decrypted configuration, cached because every workspace
// message consults it.
type state struct {
	bridge    model.Bridge
	token     string
	secret    string
	bySlack   map[string]model.Link
	byChannel map[uuid.UUID]model.Link
}

func (s *state) client() slackClient { return slackClient{token: s.token} }

var (
	stateMu     sync.Mutex
	cached      *state
	cachedAt    time.Time
	cachedEmpty bool
)

// loadState returns the bridge configuration, or nil when Slack is not
// connected.
func loadState(ctx context.Context) (*state, error) {
	stateMu.Lock()
	defer stateMu.Unlock()
	if time.Since(cachedAt) < stateTTL && (cached != nil || cachedEmpty) {
		return cached, nil
	}
	b, err := model.GetBridge(ctx)
	if err != nil {
		return nil, err
	}
	if b == nil {
		cached, cachedEmpty, cachedAt = nil, true, time.Now()
		return nil, nil
	}
	token, err := helpers.DecryptSecret(b.BotTokenEnc)
	if err != nil {
		return nil, fmt.Errorf("slack bridge: unseal token: %w", err)
	}
	secret, err := helpers.DecryptSecret(b.SigningSecretEnc)
	if err != nil {
		return nil, fmt.Errorf("slack bridge: unseal signing secret: %w", err)
	}
	links, err := model.ListLinks(ctx)
	if err != nil {
		return nil, err
	}
	s := &state{
		bridge: *b, token: token, secret: secret,
		bySlack:   make(map[string]model.Link, len(links)),
		byChannel: make(map[uuid.UUID]model.Link, len(links)),
	}
	for _, l := range links {
		s.bySlack[l.SlackChannelID] = l
		s.byChannel[l.ChannelUUID] = l
	}
	cached, cachedEmpty, cachedAt = s, false, time.Now()
	return s, nil
}

// invalidate drops the cached configuration after a change in this process.
func invalidate() {
	stateMu.Lock()
	cached, cachedEmpty, cachedAt = nil, false, time.Time{}
	stateMu.Unlock()
}

// Errors an admin can act on. The controller maps them to 400s.
var (
	ErrNotConnected   = errors.New("Slack is not connected")
	ErrBadToken       = errors.New("that is not a bot token: it starts with xoxb-")
	ErrBadSecret      = errors.New("paste the Signing Secret from the app's Basic Information page")
	ErrChannelMissing = errors.New("that OneCamp channel does not exist")
	ErrLinkTaken      = errors.New("one of those channels is already linked")
)

// UserError is a Slack refusal explained in words an admin can act on.
type UserError struct{ Msg string }

func (e *UserError) Error() string { return e.Msg }

// explain turns a Slack error code into what the admin should do about it.
func explain(err error, slackChannelName string) error {
	var se *SlackError
	if !errors.As(err, &se) {
		return err
	}
	where := "the channel"
	if slackChannelName != "" {
		where = "#" + slackChannelName
	}
	switch se.Code {
	case "invalid_auth", "not_authed", "token_revoked", "account_inactive", "token_expired":
		return &UserError{"Slack no longer accepts the bot token. Reinstall the app in Slack and connect again."}
	case "missing_scope":
		return &UserError{"The Slack app is missing a permission. Recreate it from the manifest shown here, or add the scopes it lists, then reinstall."}
	case "not_in_channel", "channel_not_found":
		return &UserError{"The OneCamp app is not in " + where + " in Slack. In that channel, type /invite @OneCamp."}
	case "is_archived":
		return &UserError{where + " is archived in Slack."}
	case "ratelimited":
		return &UserError{"Slack is rate limiting the app; messages will resume shortly."}
	}
	return &UserError{"Slack refused the request (" + se.Code + ")."}
}

// Status is what the admin page shows.
type Status struct {
	Connected   bool       `json:"connected"`
	TeamName    string     `json:"team_name,omitempty"`
	TeamID      string     `json:"team_id,omitempty"`
	EventsURL   string     `json:"events_url"`
	ManifestURL string     `json:"manifest_url"`
	Manifest    string     `json:"manifest"`
	Links       []LinkView `json:"links"`
	LastError   string     `json:"last_error,omitempty"`
	LastErrorAt *time.Time `json:"last_error_at,omitempty"`
}

// LinkView is one link as the admin page shows it.
type LinkView struct {
	ID               string    `json:"id"`
	SlackChannelID   string    `json:"slack_channel_id"`
	SlackChannelName string    `json:"slack_channel_name"`
	ChannelUUID      string    `json:"channel_uuid"`
	ChannelName      string    `json:"channel_name"`
	CreatedAt        time.Time `json:"created_at"`
}

// eventsURL is the Request URL the Slack app must deliver events to.
func eventsURL() string { return authService.BackendBaseURL() + EventsPath }

// Manifest is a Slack app manifest with exactly the scopes and events the
// bridge uses, so the admin creates the app in one step instead of ticking
// eight scopes by hand.
func Manifest() string {
	m := map[string]interface{}{
		"display_information": map[string]interface{}{
			"name":        "OneCamp",
			"description": "Keeps a Slack channel and a OneCamp channel in one conversation.",
		},
		"features": map[string]interface{}{
			"bot_user": map[string]interface{}{"display_name": "OneCamp", "always_online": true},
		},
		"oauth_config": map[string]interface{}{
			"scopes": map[string]interface{}{
				"bot": []string{
					"channels:history", "groups:history", // read linked channels
					"channels:read", "groups:read", // list channels to link
					"channels:join",                      // join a public channel when linked
					"chat:write", "chat:write.customize", // post under the OneCamp author's name
					"users:read", // name the Slack sender
				},
			},
		},
		"settings": map[string]interface{}{
			"event_subscriptions": map[string]interface{}{
				"request_url": eventsURL(),
				"bot_events":  []string{"message.channels", "message.groups"},
			},
			"org_deploy_enabled":     false,
			"socket_mode_enabled":    false,
			"token_rotation_enabled": false,
		},
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	return string(b)
}

// manifestURL opens Slack's "create app" flow with the manifest filled in.
func manifestURL() string {
	return "https://api.slack.com/apps?new_app=1&manifest_json=" + url.QueryEscape(Manifest())
}

// GetStatus returns the connection, its links and the setup material.
func GetStatus(ctx context.Context) (*Status, error) {
	st := &Status{EventsURL: eventsURL(), Manifest: Manifest(), ManifestURL: manifestURL(), Links: []LinkView{}}
	b, err := model.GetBridge(ctx)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return st, nil
	}
	st.Connected, st.TeamName, st.TeamID = true, b.TeamName, b.TeamID
	st.LastError, st.LastErrorAt = b.LastError, b.LastErrorAt
	links, err := model.ListLinks(ctx)
	if err != nil {
		return nil, err
	}
	for _, l := range links {
		st.Links = append(st.Links, LinkView{
			ID: l.ID.String(), SlackChannelID: l.SlackChannelID, SlackChannelName: l.SlackChannelName,
			ChannelUUID: l.ChannelUUID.String(), ChannelName: channelName(ctx, l.ChannelUUID),
			CreatedAt: l.CreatedAt,
		})
	}
	return st, nil
}

// channelName resolves a OneCamp channel's name, "" when it is gone.
func channelName(ctx context.Context, channelUUID uuid.UUID) string {
	bot, err := userBusiness.EnsureSlackBridgeBot(ctx)
	if err != nil {
		return ""
	}
	ch, err := channelDomain.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID.String(), bot.DgraphUID)
	if err != nil || ch == nil {
		return ""
	}
	return ch.Name
}

// Connect validates the credentials with Slack and stores them sealed.
func Connect(ctx context.Context, botToken, signingSecret string, by *uuid.UUID) (*Status, error) {
	botToken, signingSecret = strings.TrimSpace(botToken), strings.TrimSpace(signingSecret)
	if !strings.HasPrefix(botToken, "xoxb-") {
		return nil, ErrBadToken
	}
	if len(signingSecret) < 16 || strings.ContainsAny(signingSecret, " \t\n") {
		return nil, ErrBadSecret
	}
	who, err := slackClient{token: botToken}.authTest(ctx)
	if err != nil {
		return nil, explain(err, "")
	}
	if who.BotID == "" {
		return nil, ErrBadToken
	}
	tokenEnc, err := helpers.EncryptSecret(botToken)
	if err != nil {
		return nil, err
	}
	secretEnc, err := helpers.EncryptSecret(signingSecret)
	if err != nil {
		return nil, err
	}
	if err := model.SaveBridge(ctx, model.Bridge{
		TeamID: who.TeamID, TeamName: who.Team, BotUserID: who.UserID, BotID: who.BotID,
		BotTokenEnc: tokenEnc, SigningSecretEnc: secretEnc, CreatedBy: by,
	}); err != nil {
		return nil, err
	}
	invalidate()
	return GetStatus(ctx)
}

// Disconnect forgets the Slack workspace and every link. Bridged messages
// stay in both apps.
func Disconnect(ctx context.Context) error {
	if err := model.DeleteBridge(ctx); err != nil {
		return err
	}
	invalidate()
	return nil
}

// SlackChannels lists the channels the admin can link, with the ones already
// linked left out.
func SlackChannels(ctx context.Context) ([]SlackChannel, error) {
	st, err := loadState(ctx)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, ErrNotConnected
	}
	all, err := st.client().listChannels(ctx)
	if err != nil {
		return nil, explain(err, "")
	}
	out := make([]SlackChannel, 0, len(all))
	for _, c := range all {
		if _, linked := st.bySlack[c.ID]; !linked {
			out = append(out, c)
		}
	}
	return out, nil
}

// Link joins a Slack channel to a OneCamp channel. The app joins a public
// Slack channel itself; a private one must have invited it already.
func Link(ctx context.Context, slackChannelID string, channelUUID uuid.UUID, by *uuid.UUID) (*LinkView, error) {
	st, err := loadState(ctx)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, ErrNotConnected
	}
	name := channelName(ctx, channelUUID)
	if name == "" {
		return nil, ErrChannelMissing
	}
	c := st.client()
	info, err := c.channelInfo(ctx, slackChannelID)
	if err != nil {
		return nil, explain(err, "")
	}
	if !info.IsMember {
		if info.IsPrivate {
			return nil, explain(&SlackError{Code: "not_in_channel"}, info.Name)
		}
		if err := c.join(ctx, slackChannelID); err != nil {
			return nil, explain(err, info.Name)
		}
	}
	l := model.Link{
		ID: uuid.New(), SlackChannelID: slackChannelID, SlackChannelName: info.Name,
		ChannelUUID: channelUUID, CreatedBy: by,
	}
	if err := model.CreateLink(ctx, l); err != nil {
		if errors.Is(err, model.ErrLinkTaken) {
			return nil, ErrLinkTaken
		}
		return nil, err
	}
	invalidate()
	return &LinkView{
		ID: l.ID.String(), SlackChannelID: l.SlackChannelID, SlackChannelName: l.SlackChannelName,
		ChannelUUID: channelUUID.String(), ChannelName: name, CreatedAt: time.Now(),
	}, nil
}

// Unlink stops bridging a pair. Reports whether the link existed.
func Unlink(ctx context.Context, id uuid.UUID) (bool, error) {
	ok, err := model.DeleteLink(ctx, id)
	if err == nil {
		invalidate()
	}
	return ok, err
}

// noteDelivery records the outcome of a delivery so the admin sees a broken
// bridge. Success only writes when there was an error to clear.
func noteDelivery(ctx context.Context, err error, slackChannelName string) {
	if err == nil {
		if st, _ := loadState(ctx); st != nil && st.bridge.LastError != "" {
			_ = model.RecordError(ctx, "")
			invalidate()
		}
		return
	}
	helpers.LogErrorWithContext(ctx, "SlackBridge delivery: %v", err)
	msg := explain(err, slackChannelName).Error()
	if rerr := model.RecordError(ctx, msg); rerr == nil {
		invalidate()
	}
}

// PointAtSlackForTest sends Slack Web API calls to base (a local fake) and
// returns the function that restores the real endpoint.
func PointAtSlackForTest(base string) (restore func()) {
	prev := slackAPIBase
	slackAPIBase = base
	return func() { slackAPIBase = prev }
}

// ForgetStateForTest drops the cached configuration after a test writes the
// tables directly.
func ForgetStateForTest() { invalidate() }
