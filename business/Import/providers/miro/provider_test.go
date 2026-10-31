package miro

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

type noWait struct{}

func (noWait) Wait(context.Context) error { return nil }

// fakeMiro is Miro's API as its spec answers, over TLS (a token is only sent
// to an https address), for one board of 120 items in three pages; its own
// handler first, when it answers.
type fakeMiro struct {
	t     *testing.T
	srv   *httptest.Server
	mu    sync.Mutex
	asked []string
	// styleless lists items the list gives without a style; their typed
	// read gives it.
	styleless map[string]bool
	override  func(w http.ResponseWriter, r *http.Request) bool
}

func newFakeMiro(t *testing.T) (*fakeMiro, *Provider) {
	f := &fakeMiro{t: t, styleless: map[string]bool{"9": true}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	p := &Provider{
		rl: noWait{}, base: f.srv.URL, client: f.srv.Client(), now: func() time.Time { return time.UnixMilli(fixedNow) },
		token: func(context.Context, *importModels.Job) (string, error) { return "good-token", nil },
		download: func(_ context.Context, att importProvider.SourceAttachment, dest io.Writer) (string, int64, error) {
			if !strings.HasPrefix(att.URL, "https://files.example/") || att.Headers["Authorization"] != "" {
				return "", 0, fmt.Errorf("downloaded %q with headers %v", att.URL, att.Headers)
			}
			n, err := dest.Write([]byte("PNGBYTES"))
			return "image/png", int64(n), err
		},
	}
	return f, p
}

func (f *fakeMiro) json(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeMiro) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.asked = append(f.asked, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	f.mu.Unlock()
	if f.override != nil && f.override(w, r) {
		return
	}
	if r.Header.Get("Authorization") != "Bearer good-token" {
		w.WriteHeader(http.StatusUnauthorized)
		f.json(w, map[string]any{"status": 401, "code": "tokenNotProvided", "message": "No token"})
		return
	}
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	switch path := r.URL.Path; {
	case path == "/v2/boards":
		all := []map[string]any{}
		for i := 0; i < 60; i++ {
			all = append(all, map[string]any{"id": fmt.Sprintf("b%02d", i), "name": fmt.Sprintf("Board %d", i), "description": "",
				"type": "board", "viewLink": "https://miro.com/app/board/b" + strconv.Itoa(i), "owner": map[string]any{"name": "Ann"}})
		}
		limit, _ := strconv.Atoi(q.Get("limit"))
		end := min(offset+limit, len(all))
		f.json(w, map[string]any{"data": all[offset:end], "total": len(all), "size": end - offset, "offset": offset, "limit": limit})
	case path == "/v2/boards/b01":
		f.json(w, map[string]any{"id": "b01", "name": "Launch retro", "description": "Q3"})
	case path == "/v2/boards/gone":
		w.WriteHeader(http.StatusNotFound)
	case path == "/v2/boards/b01/items" && q.Get("tag_id") != "":
		if q.Get("tag_id") == "t1" {
			f.json(w, map[string]any{"data": []any{map[string]any{"id": "1", "type": "sticky_note"}}, "total": 1, "offset": offset})
			return
		}
		f.json(w, map[string]any{"data": []any{}, "total": 0, "offset": offset})
	case path == "/v2/boards/b01/items":
		// 120 sticky notes, 50 a page, by cursor.
		page := map[string]int{"": 0, "c50": 50, "c100": 100}[q.Get("cursor")]
		limit, _ := strconv.Atoi(q.Get("limit"))
		var data []any
		for i := page; i < min(page+limit, 120); i++ {
			it := map[string]any{"id": strconv.Itoa(i), "type": "sticky_note", "data": map[string]any{"content": "<p>Note " + strconv.Itoa(i) + "</p>"},
				"position": map[string]any{"x": i * 10, "y": 0, "origin": "center", "relativeTo": "canvas_center"}, "geometry": map[string]any{"width": 200, "height": 200}}
			if !f.styleless[strconv.Itoa(i)] {
				it["style"] = map[string]any{"fillColor": "light_blue"}
			}
			data = append(data, it)
		}
		cursor := ""
		if page+limit < 120 && limit == pageSize {
			cursor = "c" + strconv.Itoa(page+limit)
		}
		f.json(w, map[string]any{"data": data, "total": 120, "size": len(data), "cursor": cursor, "limit": limit})
	case path == "/v2/boards/b01/sticky_notes/9":
		f.json(w, map[string]any{"id": "9", "type": "sticky_note", "style": map[string]any{"fillColor": "violet"}})
	case path == "/v2/boards/b01/connectors":
		f.json(w, map[string]any{"data": []any{map[string]any{"id": "c1", "shape": "straight", "startItem": map[string]any{"id": "1"}, "endItem": map[string]any{"id": "2"}}}, "cursor": ""})
	case path == "/v2/boards/b01/tags":
		f.json(w, map[string]any{"data": []any{map[string]any{"id": "t1", "title": "launch", "fillColor": "red"}}, "total": 1, "offset": offset})
	case path == "/v2/boards/b01/groups":
		f.json(w, map[string]any{"data": []any{map[string]any{"id": "g1", "type": "group", "data": map[string]any{"items": []any{"3", "4"}}}}, "cursor": ""})
	case strings.HasPrefix(path, "/v2/boards/b01/resources/images/"):
		if q.Get("format") != "original" || q.Get("redirect") != "false" {
			f.t.Errorf("a picture was asked for as %s", r.URL.RawQuery)
		}
		f.json(w, map[string]any{"type": "image", "url": "https://files.example/abc.png?sig=1"})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestATokenMiroRefusesIsSaidAsSuch(t *testing.T) {
	_, p := newFakeMiro(t)
	_, err := p.Discover(context.Background(), "u", &importModels.Token{AccessToken: "wrong"})
	if !errors.Is(err, importProvider.ErrTokenRejected) {
		t.Fatalf("a refused token: %v", err)
	}
	if err := p.Validate(context.Background(), &importModels.Job{}, nil); err != nil {
		t.Fatalf("a good token: %v", err)
	}
}

func TestBoardsAreListedPageByPage(t *testing.T) {
	_, p := newFakeMiro(t)
	items, err := p.Discover(context.Background(), "u", &importModels.Token{AccessToken: "good-token"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 60 || items[59].ID != "b59" || items[1].Kind != "board" || items[1].URL == "" || items[1].Meta["owner"] != "Ann" {
		t.Fatalf("%d boards; last %v", len(items), items[len(items)-1])
	}
}

func TestThePlanCountsTheBoardPickedAndQueuesIt(t *testing.T) {
	_, p := newFakeMiro(t)
	job := &importModels.Job{Id: uuid.New()}
	plan, chunks, err := p.Plan(context.Background(), job, importProvider.JobOptions{optBoardID: "b01"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.BoardCount != 1 || plan.ItemCount != 120 || len(chunks) != 1 || *chunks[0].ParentSourceId != "b01" ||
		chunks[0].ChunkType != importModels.ChunkBoards || chunks[0].ImportId != job.Id {
		t.Fatalf("plan %+v, chunks %+v", plan, chunks)
	}
	if !strings.Contains(strings.Join(plan.Warnings, " "), "comments, votes") {
		t.Fatalf("the plan says what Miro's API doesn't give: %v", plan.Warnings)
	}
	if _, _, err := p.Plan(context.Background(), job, importProvider.JobOptions{optBoardID: "gone"}); err == nil || !strings.Contains(err.Error(), "isn't visible") {
		t.Fatalf("a board that's gone: %v", err)
	}
}

func TestABoardIsReadWholeAndDrawn(t *testing.T) {
	f, p := newFakeMiro(t)
	var reads []int
	b, err := p.BoardOf(context.Background(), &importModels.Job{}, nil, "b01", func(n int) { reads = append(reads, n) })
	if err != nil {
		t.Fatal(err)
	}
	if b.Title != "Launch retro" || b.Items != 120 || len(reads) != 3 || reads[2] != 120 {
		t.Fatalf("title %q, %d items, reads %v", b.Title, b.Items, reads)
	}
	els := map[string]map[string]any{}
	for _, el := range b.Elements {
		els[el["id"].(string)] = el
	}
	// 120 notes and their words, and the connector between notes 1 and 2.
	if len(b.Elements) != 241 || els["miro-c1"]["type"] != "arrow" {
		t.Fatalf("%d elements", len(b.Elements))
	}
	data := func(id string) map[string]any { return els[id]["customData"].(map[string]any) }
	if data("miro-9")["colour"] != "purple" || data("miro-8")["colour"] != "ice" {
		t.Fatal("a note whose style the list left out is read on its own, and keeps its colour")
	}
	if tags, _ := data("miro-1")["tags"].([]any); len(tags) != 1 || tags[0] != "launch" {
		t.Fatalf("the tagged note's tags: %v", data("miro-1")["tags"])
	}
	if g, _ := els["miro-3"]["groupIds"].([]any); len(g) != 1 || g[0] != "miro-group-g1" {
		t.Fatalf("a grouped note: %v", els["miro-3"]["groupIds"])
	}
	asked := strings.Join(f.asked, "\n")
	if strings.Count(asked, "/sticky_notes/") != 1 {
		t.Fatalf("only the note without a style is read on its own:\n%s", asked)
	}
}

func TestATooManyRequestsWaitsUntilMiroResets(t *testing.T) {
	f, p := newFakeMiro(t)
	reset := time.Now().Add(20 * time.Second).Unix()
	f.override = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
		w.WriteHeader(http.StatusTooManyRequests)
		return true
	}
	_, err := p.BoardOf(context.Background(), &importModels.Job{}, nil, "b01", nil)
	d, limited := importProvider.IsRateLimited(err)
	if !limited || d < 15*time.Second || d > 21*time.Second {
		t.Fatalf("429: %v, waiting %v", err, d)
	}
}

func TestAPictureIsReadFromTheAddressMiroHandsOut(t *testing.T) {
	f, p := newFakeMiro(t)
	att := importProvider.SourceAttachment{SourceID: "5", Name: "Mood board",
		URL: f.srv.URL + "/v2/boards/b01/resources/images/5?format=preview&redirect=false"}
	var buf bytes.Buffer
	mime, n, err := p.FetchAttachment(context.Background(), &importModels.Job{}, nil, att, &buf)
	if err != nil || mime != "image/png" || n != 8 || buf.String() != "PNGBYTES" {
		t.Fatalf("%q %d %v %q", mime, n, err, buf.String())
	}
	// The token goes to Miro's API only: never to an address on another host.
	att.URL = "https://evil.example/v2/boards/b01/resources/images/5"
	if _, _, err := p.FetchAttachment(context.Background(), &importModels.Job{}, nil, att, &buf); !errors.Is(err, importProvider.ErrAttachmentGone) {
		t.Fatalf("an image address off Miro's API: %v", err)
	}
	u, _ := url.Parse(f.srv.URL)
	att.URL = "http://" + u.Host + "/v2/boards/b01/resources/images/5"
	if _, _, err := p.FetchAttachment(context.Background(), &importModels.Job{}, nil, att, &buf); !errors.Is(err, importProvider.ErrAttachmentGone) {
		t.Fatalf("an image address without TLS: %v", err)
	}
}
