package business

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	model "github.com/akashc777/OneCamp/models/postgres/SlackBridge"
	"github.com/google/uuid"
)

func TestHTMLToMrkdwn(t *testing.T) {
	cases := map[string]string{
		`<p>Hello <strong>team</strong>, see <em>this</em></p>`:                           "Hello *team*, see _this_",
		`<p>a &lt; b &amp; c</p>`:                                                         "a &lt; b &amp; c",
		`<p>Read <a href="https://x.dev/a|b">the doc</a></p>`:                             "Read <https://x.dev/a%7Cb|the doc>",
		`<p><a href="https://x.dev">https://x.dev</a></p>`:                                "<https://x.dev>",
		`<p>run <code>make up</code></p><pre><code>line 1` + "\n" + `line 2</code></pre>`: "run `make up`\n```\nline 1\nline 2\n```",
		`<ul><li>one</li><li>two</li></ul><ol><li>first</li><li>second</li></ol>`:         "• one\n• two\n1. first\n2. second",
		`<p>one</p><p></p><p></p><p>two<br>three</p>`:                                     "one\ntwo\nthree",
		`<p><span class="mention" data-id="user@1">@Priya</span> ~no~ <s>gone</s></p>`:    "@Priya ~no~ ~gone~",
	}
	for in, want := range cases {
		if got := htmlToMrkdwn(in); got != want {
			t.Errorf("htmlToMrkdwn(%q)\n got %q\nwant %q", in, got, want)
		}
	}
	long := "<p>" + strings.Repeat("x", slackTextLimit+10) + "</p>"
	if got := htmlToMrkdwn(long); len(got) > slackTextLimit+100 || !strings.Contains(got, "full message is in OneCamp") {
		t.Errorf("a long message was not shortened with a note (len %d)", len(got))
	}
}

func TestParagraphs(t *testing.T) {
	if got := paragraphs("one\ntwo"); got != "<p>one<br/>two</p>" {
		t.Errorf("got %q", got)
	}
	code := "<pre><code>a\nb</code></pre>"
	if got := paragraphs(code); got != code {
		t.Errorf("a code block was rewrapped: %q", got)
	}
}

func TestManifestCarriesTheEventsURLAndEveryScopeUsed(t *testing.T) {
	var m struct {
		OAuth struct {
			Scopes struct {
				Bot []string `json:"bot"`
			} `json:"scopes"`
		} `json:"oauth_config"`
		Settings struct {
			Events struct {
				RequestURL string   `json:"request_url"`
				BotEvents  []string `json:"bot_events"`
			} `json:"event_subscriptions"`
		} `json:"settings"`
	}
	if err := json.Unmarshal([]byte(Manifest()), &m); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(m.Settings.Events.RequestURL, EventsPath) {
		t.Errorf("request_url = %q", m.Settings.Events.RequestURL)
	}
	// Every Slack method the client calls needs its scope here, or a fresh
	// app fails with missing_scope on first use.
	for _, s := range []string{"channels:history", "groups:history", "channels:read", "groups:read",
		"channels:join", "chat:write", "chat:write.customize", "users:read"} {
		if !strings.Contains(strings.Join(m.OAuth.Scopes.Bot, " "), s) {
			t.Errorf("manifest lacks scope %s", s)
		}
	}
	if !strings.HasPrefix(manifestURL(), "https://api.slack.com/apps?new_app=1&manifest_json=") {
		t.Errorf("manifestURL = %q", manifestURL())
	}
}

// withState pins the cached configuration for one test.
func withState(t *testing.T, st *state) {
	t.Helper()
	stateMu.Lock()
	cached, cachedEmpty, cachedAt = st, st == nil, time.Now()
	stateMu.Unlock()
	t.Cleanup(invalidate)
}

func sign(secret, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + ts + ":" + string(body)))
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

func TestHandleEvents(t *testing.T) {
	ctx := context.Background()
	now := strconv.FormatInt(time.Now().Unix(), 10)
	challenge := []byte(`{"type":"url_verification","challenge":"abc_123-XYZ"}`)

	t.Run("before connection, the URL check is answered", func(t *testing.T) {
		withState(t, nil)
		res := HandleEvents(ctx, challenge, "", "")
		if res.Status != 200 || res.Body != "abc_123-XYZ" || res.ContentType != "text/plain" {
			t.Fatalf("got %+v", res)
		}
		if res := HandleEvents(ctx, []byte(`{"type":"url_verification","challenge":"<script>"}`), "", ""); res.Status != 400 {
			t.Errorf("a challenge that is not a token was echoed: %+v", res)
		}
	})

	st := &state{
		bridge: model.Bridge{TeamID: "T1", BotID: "B1", BotUserID: "U1"},
		secret: "s3cret-signing-value", bySlack: map[string]model.Link{}, byChannel: map[uuid.UUID]model.Link{},
	}

	t.Run("once connected, the URL check must be signed", func(t *testing.T) {
		withState(t, st)
		if res := HandleEvents(ctx, challenge, now, "v0=bad"); res.Status != 401 {
			t.Errorf("unsigned challenge accepted: %+v", res)
		}
		if res := HandleEvents(ctx, challenge, now, sign(st.secret, now, challenge)); res.Status != 200 || res.Body != "abc_123-XYZ" {
			t.Errorf("signed challenge refused: %+v", res)
		}
	})

	t.Run("an event with a bad or stale signature is refused", func(t *testing.T) {
		withState(t, st)
		body := []byte(`{"type":"event_callback","team_id":"T1","event":{"type":"message","channel":"C1","text":"hi","ts":"1.1"}}`)
		if res := HandleEvents(ctx, body, now, sign("wrong", now, body)); res.Status != 401 {
			t.Errorf("wrong secret accepted: %+v", res)
		}
		old := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
		if res := HandleEvents(ctx, body, old, sign(st.secret, old, body)); res.Status != 401 {
			t.Errorf("replayed delivery accepted: %+v", res)
		}
		// Signed, but for an unlinked channel: acknowledged, nothing done.
		if res := HandleEvents(ctx, body, now, sign(st.secret, now, body)); res.Status != 200 {
			t.Errorf("signed delivery refused: %+v", res)
		}
	})
}

func TestFromUs(t *testing.T) {
	st := &state{bridge: model.Bridge{BotID: "B1", BotUserID: "U1"}}
	for _, m := range []messageEvent{{BotID: "B1"}, {User: "U1"}} {
		if !fromUs(st, &m) {
			t.Errorf("%+v not recognised as the bridge's own message", m)
		}
	}
	for _, m := range []messageEvent{{User: "U2"}, {BotID: "B2", Username: "CI"}, {}} {
		if fromUs(st, &m) {
			t.Errorf("%+v taken for the bridge's own message", m)
		}
	}
}

func TestExplainNamesTheFix(t *testing.T) {
	err := explain(&SlackError{Method: "chat.postMessage", Code: "not_in_channel"}, "general")
	var ue *UserError
	if !errors.As(err, &ue) || !strings.Contains(ue.Msg, "/invite @OneCamp") || !strings.Contains(ue.Msg, "#general") {
		t.Errorf("got %v", err)
	}
	if err := explain(&SlackError{Code: "token_revoked"}, ""); !strings.Contains(err.Error(), "Reinstall") {
		t.Errorf("got %v", err)
	}
	plain := errors.New("dial tcp: timeout")
	if explain(plain, "") != plain {
		t.Error("a network error was relabelled as a Slack refusal")
	}
}

func TestClientRetriesOnceWhenRateLimited(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer xoxb-t" {
			t.Errorf("token not sent: %q", r.Header.Get("Authorization"))
		}
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_ = r.ParseForm()
		if r.URL.Path == "/chat.postMessage" && r.Form.Get("username") != "Priya" {
			t.Errorf("username not sent: %v", r.Form)
		}
		_, _ = w.Write([]byte(`{"ok":true,"ts":"123.456"}`))
	}))
	defer srv.Close()
	prev := slackAPIBase
	slackAPIBase = srv.URL + "/"
	defer func() { slackAPIBase = prev }()

	ts, err := slackClient{token: "xoxb-t"}.postMessage(context.Background(), "C1", "hi", "Priya", "")
	if err != nil || ts != "123.456" || calls != 2 {
		t.Fatalf("ts=%q err=%v calls=%d", ts, err, calls)
	}
}

func TestClientReturnsSlackRefusals(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error":"channel_not_found"}`))
	}))
	defer srv.Close()
	prev := slackAPIBase
	slackAPIBase = srv.URL + "/"
	defer func() { slackAPIBase = prev }()

	err := slackClient{token: "x"}.delete(context.Background(), "C1", "1.1")
	var se *SlackError
	if !errors.As(err, &se) || se.Code != "channel_not_found" || se.Method != "chat.delete" {
		t.Fatalf("got %v", err)
	}
}
