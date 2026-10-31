package business

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A Giphy stand-in on loopback: it answers status and body, and hands each
// request's path and query to the test.
func serveGiphy(t *testing.T, status int, body string) <-chan *url.URL {
	t.Helper()
	asked := make(chan *url.URL, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked <- r.URL
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	oldAPI, oldCheck, oldClient := giphyAPI, forwardCheck, forwardClient
	giphyAPI = srv.URL + "/v1/gifs"
	forwardCheck = func(string) error { return nil }
	forwardClient = func() *http.Client { return srv.Client() }
	t.Cleanup(func() { giphyAPI, forwardCheck, forwardClient = oldAPI, oldCheck, oldClient })
	return asked
}

// Two GIFs as Giphy answers them (sizes are strings), and a third with no
// rendition the picker can draw.
const giphyTwoOfMany = `{
  "data": [
    {"id": "g1", "title": "Party Parrot GIF", "images": {
      "fixed_width": {"url": "https://media1.giphy.com/g1/200w.gif", "webp": "https://media1.giphy.com/g1/200w.webp", "width": "200", "height": "150"},
      "fixed_height": {"url": "https://media1.giphy.com/g1/200.gif", "width": "267", "height": "200"},
      "original": {"url": "https://media1.giphy.com/g1/giphy.gif", "width": "480", "height": "360"}}},
    {"id": "g2", "title": "", "images": {
      "fixed_width": {"url": "https://media2.giphy.com/g2/200w.gif", "width": "200", "height": "113"},
      "downsized_large": {"url": "https://media2.giphy.com/g2/giphy-downsized-large.gif", "width": "640", "height": "360"}}},
    {"id": "g3", "title": "Broken", "images": {}}
  ],
  "pagination": {"total_count": 120, "count": 3, "offset": 24}
}`

func TestGiphyQueryAsksTrendingWithNoTerm(t *testing.T) {
	asked := serveGiphy(t, http.StatusOK, giphyTwoOfMany)
	gifs, next, err := giphyQuery(context.Background(), "key-1", "", 0, giphyPickerPage)
	if err != nil {
		t.Fatalf("giphyQuery: %v", err)
	}
	u := <-asked
	if u.Path != "/v1/gifs/trending" {
		t.Fatalf("asked %s, want trending", u.Path)
	}
	q := u.Query()
	if q.Get("api_key") != "key-1" || q.Get("limit") != "24" || q.Get("offset") != "0" || q.Get("rating") != giphyRating || q.Has("q") {
		t.Fatalf("query %v", q)
	}
	if len(gifs) != 2 {
		t.Fatalf("%d gifs: want 2 (the one with no rendition skipped)", len(gifs))
	}
	if next == nil || *next != 3 {
		t.Fatalf("next page at %s: want 3, after all three GIFs Giphy sent, the skipped one too", offsetText(next))
	}
	g := gifs[0]
	if g.ID != "g1" || g.Title != "Party Parrot GIF" {
		t.Fatalf("first gif %+v", g)
	}
	if g.Preview.URL != "https://media1.giphy.com/g1/200w.webp" || g.Preview.Width != 200 || g.Preview.Height != 150 {
		t.Fatalf("preview %+v: want the 200 px wide webp with its size", g.Preview)
	}
	if g.Send.URL != "https://media1.giphy.com/g1/200.gif" || g.Send.Width != 267 || g.Send.Height != 200 {
		t.Fatalf("send %+v: want fixed_height with its size", g.Send)
	}
	if p, s := gifs[1].Preview, gifs[1].Send; p.URL != "https://media2.giphy.com/g2/200w.gif" || s.URL != "https://media2.giphy.com/g2/giphy-downsized-large.gif" || s.Width != 640 {
		t.Fatalf("second gif preview %+v send %+v: want the gif when there's no webp, and downsized_large when there's no fixed_height", p, s)
	}
}

func TestGiphyQuerySearchesATermFromAnOffset(t *testing.T) {
	asked := serveGiphy(t, http.StatusOK, giphyTwoOfMany)
	if _, _, err := giphyQuery(context.Background(), "key-1", "party parrot", 24, giphyPickerPage); err != nil {
		t.Fatalf("giphyQuery: %v", err)
	}
	u := <-asked
	if u.Path != "/v1/gifs/search" || u.Query().Get("q") != "party parrot" || u.Query().Get("offset") != "24" {
		t.Fatalf("asked %s?%s", u.Path, u.RawQuery)
	}
}

// testGiphy tells a refused key from an unreachable Giphy by the status in
// the error.
func TestGiphyQuerySaysWhenGiphyRefuses(t *testing.T) {
	serveGiphy(t, http.StatusUnauthorized, `{"message": "Invalid authentication credentials"}`)
	_, _, err := giphyQuery(context.Background(), "bad", "hello", 0, giphyCommandResults)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err %v, want one naming 401", err)
	}
}

// A failed request's error is logged and shown to admins, and the request's
// address holds the workspace's key (CR-21's MF1).
func TestGiphyQueryErrorsNeverCarryTheKey(t *testing.T) {
	hold := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-hold }))
	defer slow.Close()
	defer close(hold)
	refused := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	refused.Close()
	oldAPI, oldCheck, oldClient := giphyAPI, forwardCheck, forwardClient
	t.Cleanup(func() { giphyAPI, forwardCheck, forwardClient = oldAPI, oldCheck, oldClient })
	forwardCheck = func(string) error { return nil }
	forwardClient = func() *http.Client { return &http.Client{} }
	for name, srv := range map[string]*httptest.Server{"refused": refused, "timed out": slow} {
		giphyAPI = srv.URL + "/v1/gifs"
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		_, _, err := giphyQuery(ctx, "SECRETKEY123", "cats", 0, giphyPickerPage)
		cancel()
		if err == nil || strings.Contains(err.Error(), "SECRETKEY123") || strings.Contains(err.Error(), "/v1/gifs") {
			t.Errorf("%s: err %v; want one without the key or the address", name, err)
		}
	}
}

// offsetText is a next page's offset as a failure says it.
func offsetText(o *int) string {
	if o == nil {
		return "none"
	}
	return strconv.Itoa(*o)
}

func TestNextGiphyOffset(t *testing.T) {
	cases := []struct {
		name                 string
		offset, count, total int
		want                 int // -1: no next page
	}{
		{"a full first page", 0, 24, 120, 24},
		{"the last page", 96, 24, 120, -1},
		{"an empty page", 24, 0, 120, -1},
		{"past the deepest offset Giphy serves", 4980, 24, 9000, -1},
		{"up to it", 4950, 24, 9000, 4974},
	}
	for _, c := range cases {
		got := nextGiphyOffset(c.offset, c.count, c.total)
		if (c.want < 0) != (got == nil) || (got != nil && *got != c.want) {
			t.Errorf("%s: next %s, want %d", c.name, offsetText(got), c.want)
		}
	}
}

// The picker refuses what Giphy can't serve before it asks for the key.
func TestGiphyPickerRefusesBadInput(t *testing.T) {
	for name, in := range map[string]struct {
		term   string
		offset int
	}{
		"a term past 50 characters": {strings.Repeat("é", giphyMaxTerm+1), 0},
		"a negative offset":         {"cats", -1},
		"an offset past Giphy's":    {"cats", giphyMaxOffset + 1},
	} {
		if _, err := GiphyPicker(context.Background(), in.term, in.offset); !errors.Is(err, ErrGiphyBadRequest) {
			t.Errorf("%s: err %v, want ErrGiphyBadRequest", name, err)
		}
	}
}
