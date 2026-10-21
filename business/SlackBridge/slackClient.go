package business

// The bridge's Slack calls, the eight methods it makes, through the one Slack
// Web API client (services/Slack): a 429 is tried once more after its
// Retry-After, so a listener goroutine is never parked for long.

import (
	"context"
	"net/url"
	"strings"

	slackapi "github.com/akashc777/OneCamp/services/Slack"
)

// SlackError is a Slack refusal ({"ok": false, "error": code}).
type SlackError = slackapi.Error

// slackOK is what every Slack reply carries.
type slackOK = slackapi.OK

type slackClient struct{ token string }

// call POSTs form to method and decodes the reply into out, which embeds
// slackOK. A refusal comes back as *SlackError.
func (c slackClient) call(ctx context.Context, method string, form url.Values, out slackapi.Reply) error {
	return slackapi.Client{Token: c.token, Retries: 1}.Call(ctx, method, form, out)
}

type authTestReply struct {
	slackOK
	TeamID string `json:"team_id"`
	Team   string `json:"team"`
	UserID string `json:"user_id"`
	BotID  string `json:"bot_id"`
}

func (c slackClient) authTest(ctx context.Context) (*authTestReply, error) {
	var r authTestReply
	if err := c.call(ctx, "auth.test", url.Values{}, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

type slackUser struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	IsBot   bool   `json:"is_bot"`
	Profile struct {
		DisplayName string `json:"display_name"`
		RealName    string `json:"real_name"`
	} `json:"profile"`
}

// displayName is what Slack itself shows for the person.
func (u slackUser) displayName() string {
	for _, n := range []string{u.Profile.DisplayName, u.Profile.RealName, u.Name} {
		if n = strings.TrimSpace(n); n != "" {
			return n
		}
	}
	return ""
}

func (c slackClient) userInfo(ctx context.Context, id string) (*slackUser, error) {
	var r struct {
		slackOK
		User slackUser `json:"user"`
	}
	if err := c.call(ctx, "users.info", url.Values{"user": {id}}, &r); err != nil {
		return nil, err
	}
	return &r.User, nil
}

// SlackChannel is a Slack channel the admin can link.
type SlackChannel struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsPrivate bool   `json:"is_private"`
	// IsMember says whether the app is in the channel. A public channel it can
	// join itself; a private one needs /invite in Slack first.
	IsMember bool `json:"is_member"`
}

// maxListedChannels bounds conversations.list paging for a very large
// workspace; the picker is for choosing, not browsing everything.
const maxListedChannels = 2000

func (c slackClient) listChannels(ctx context.Context) ([]SlackChannel, error) {
	var out []SlackChannel
	cursor := ""
	for len(out) < maxListedChannels {
		form := url.Values{
			"types":            {"public_channel,private_channel"},
			"exclude_archived": {"true"},
			"limit":            {"200"},
		}
		if cursor != "" {
			form.Set("cursor", cursor)
		}
		var r struct {
			slackOK
			Channels []SlackChannel `json:"channels"`
			Meta     struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := c.call(ctx, "conversations.list", form, &r); err != nil {
			return nil, err
		}
		out = append(out, r.Channels...)
		if r.Meta.NextCursor == "" {
			break
		}
		cursor = r.Meta.NextCursor
	}
	return out, nil
}

func (c slackClient) channelInfo(ctx context.Context, id string) (*SlackChannel, error) {
	var r struct {
		slackOK
		Channel SlackChannel `json:"channel"`
	}
	if err := c.call(ctx, "conversations.info", url.Values{"channel": {id}}, &r); err != nil {
		return nil, err
	}
	return &r.Channel, nil
}

func (c slackClient) join(ctx context.Context, channel string) error {
	var r slackOK
	return c.call(ctx, "conversations.join", url.Values{"channel": {channel}}, &r)
}

// postMessage sends text as username (chat:write.customize) and returns its ts.
// A non-empty threadTs makes it a thread reply.
func (c slackClient) postMessage(ctx context.Context, channel, text, username, threadTs string) (string, error) {
	form := url.Values{
		"channel":      {channel},
		"text":         {text},
		"unfurl_links": {"false"},
	}
	if username != "" {
		form.Set("username", username)
	}
	if threadTs != "" {
		form.Set("thread_ts", threadTs)
	}
	var r struct {
		slackOK
		Ts string `json:"ts"`
	}
	if err := c.call(ctx, "chat.postMessage", form, &r); err != nil {
		return "", err
	}
	return r.Ts, nil
}

func (c slackClient) update(ctx context.Context, channel, ts, text string) error {
	var r slackOK
	return c.call(ctx, "chat.update", url.Values{"channel": {channel}, "ts": {ts}, "text": {text}}, &r)
}

func (c slackClient) delete(ctx context.Context, channel, ts string) error {
	var r slackOK
	return c.call(ctx, "chat.delete", url.Values{"channel": {channel}, "ts": {ts}}, &r)
}
