package business

// A minimal Slack Web API client: only the eight methods the bridge calls.
// Every call is a form POST with the bot token in the Authorization header,
// which every Slack method accepts. A 429 is retried once after the
// Retry-After Slack names, capped so a listener goroutine is never parked for
// long.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var (
	// slackAPIBase is a var so tests can point it at a fake server.
	slackAPIBase = "https://slack.com/api/"
	slackHTTP    = &http.Client{Timeout: 10 * time.Second}
	// maxRetryAfter caps how long one rate-limited call waits before its retry.
	maxRetryAfter = 30 * time.Second
)

// SlackError is a Slack refusal ({"ok": false, "error": code}).
type SlackError struct {
	Method string
	Code   string
}

func (e *SlackError) Error() string { return "slack " + e.Method + ": " + e.Code }

type slackClient struct{ token string }

// call POSTs form to method and decodes the reply into out, which must embed
// slackOK. A refusal comes back as *SlackError.
func (c slackClient) call(ctx context.Context, method string, form url.Values, out interface{ okErr() (bool, string) }) error {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, slackAPIBase+method, strings.NewReader(form.Encode()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
		req.Header.Set("Authorization", "Bearer "+c.token)
		resp, err := slackHTTP.Do(req)
		if err != nil {
			return fmt.Errorf("slack %s: %w", method, err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("slack %s: %w", method, err)
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			if attempt > 0 {
				return &SlackError{Method: method, Code: "ratelimited"}
			}
			wait := time.Second
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
				wait = time.Duration(s) * time.Second
			}
			if wait > maxRetryAfter {
				wait = maxRetryAfter
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("slack %s: http %d", method, resp.StatusCode)
		}
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("slack %s: %w", method, err)
		}
		if ok, code := out.okErr(); !ok {
			if code == "" {
				code = "unknown_error"
			}
			return &SlackError{Method: method, Code: code}
		}
		return nil
	}
}

type slackOK struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

func (s slackOK) okErr() (bool, string) { return s.OK, s.Error }

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
