package slack

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// pointAt has Base at srv for the test.
func pointAt(t *testing.T, srv *httptest.Server) {
	t.Helper()
	was := Base
	Base = srv.URL + "/api/"
	t.Cleanup(func() { Base = was })
}

// A refusal carries Slack's code and, for a missing scope, the scope it needed.
func TestARefusalSaysTheScopeItNeeded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer xoxp-1" || r.URL.Path != "/api/conversations.list" {
			t.Errorf("asked %s with %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Write([]byte(`{"ok":false,"error":"missing_scope","needed":"im:read","provided":"channels:read"}`))
	}))
	defer srv.Close()
	pointAt(t, srv)
	var out OK
	err := Client{Token: "xoxp-1"}.Call(context.Background(), "conversations.list", url.Values{"types": {"im"}}, &out)
	var se *Error
	if !errors.As(err, &se) || se.Code != "missing_scope" || se.Needed != "im:read" || se.Method != "conversations.list" {
		t.Fatalf("got %#v", err)
	}
}

// A file's link opens with the token only at Slack's file host: the token is
// never sent to another service a file links to. A file gone, one over the
// limit, and a rate limit waited out each answer as they should.
func TestADownloadSendsTheTokenOnlyToSlack(t *testing.T) {
	asked := 0
	limited := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		if r.Header.Get("Authorization") != "Bearer xoxp-1" {
			t.Errorf("%s without the token: %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/files/F1/notes.txt":
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("hello"))
		case "/files/F2/slow.txt":
			if limited {
				limited = false
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			w.Write([]byte("at last"))
		case "/files/F3/big.bin":
			w.Write(bytes.Repeat([]byte("x"), 100))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	pointAt(t, srv)
	c := Client{Token: "xoxp-1", Retries: 1}
	ctx := context.Background()

	var buf bytes.Buffer
	if n, kind, err := c.Download(ctx, srv.URL+"/files/F1/notes.txt", &buf, 10); err != nil || n != 5 || buf.String() != "hello" || kind != "text/plain" {
		t.Errorf("a file: %d %q %q %v", n, kind, buf.String(), err)
	}
	buf.Reset()
	if _, _, err := c.Download(ctx, srv.URL+"/files/F2/slow.txt", &buf, 10); err != nil || buf.String() != "at last" {
		t.Errorf("a rate limit waited out: %q %v", buf.String(), err)
	}
	if _, _, err := c.Download(ctx, srv.URL+"/files/F3/big.bin", &bytes.Buffer{}, 10); !errors.Is(err, ErrFileTooLarge) {
		t.Errorf("over the limit: %v", err)
	}
	if _, _, err := c.Download(ctx, srv.URL+"/files/F4/gone.txt", &bytes.Buffer{}, 10); !errors.Is(err, ErrFileGone) {
		t.Errorf("gone: %v", err)
	}
	before := asked
	for _, link := range []string{
		"https://docs.google.com/document/d/1",
		"http://files.slack.com/F1", // not over https
		"https://files.slack.com.evil.test/F1",
		"https://evil.test/files.slack.com",
		"not a link",
	} {
		if _, _, err := c.Download(ctx, link, &bytes.Buffer{}, 10); !errors.Is(err, ErrNotSlackFile) {
			t.Errorf("%s: %v, want ErrNotSlackFile", link, err)
		}
	}
	if asked != before {
		t.Errorf("a link that isn't Slack's was asked")
	}
	if !fileLink("https://files.slack.com/files-pri/T1-F1/notes.txt") {
		t.Error("Slack's file host refused")
	}
	for host, want := range map[string]bool{
		"https://slack.com/x": true, "https://acme.slack.com/x": true, "https://a.slack-edge.com/x": true,
		"https://slack.com.evil.test/x": false, "http://acme.slack.com/x": false, "https://evilslack.com/x": false,
	} {
		u, _ := url.Parse(host)
		if got := slackHost(u); got != want {
			t.Errorf("slackHost(%s) = %v", host, got)
		}
	}
	if strings.Contains(Base, "slack.com") {
		t.Error("Base wasn't pointed at the test's server")
	}
}
