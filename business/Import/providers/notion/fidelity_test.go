package notion

// N1, Notion move-in: a Notion workspace read through the API, each block of
// its pages compared one to one with the doc it becomes. The fixture
// (testdata/n1/handbook.json) is built from Notion's documented block and
// rich-text shapes and is labelled "unverified against a real export" until
// the owner's test workspace replaces it. What still stays in Notion is its
// "gaps" list, which the import's own plan says.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
	"github.com/akashc777/OneCamp/helpers/safefetch"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

type expectNode struct {
	Type    string         `json:"type"`
	Attrs   map[string]any `json:"attrs"`
	Text    string         `json:"text"`
	Marks   []string       `json:"marks"`
	Inline  []string       `json:"inline"`
	Links   []string       `json:"links"`
	Columns []float64      `json:"columns"`
}

type expectComment struct {
	ID      string `json:"id"`
	Author  string `json:"author"`
	Created string `json:"created"`
	Body    string `json:"body"`
}

type fidelityFixture struct {
	Label          string                     `json:"label"`
	Answers        map[string]json.RawMessage `json:"answers"`
	Files          map[string]string          `json:"files"`
	Expect         map[string][]expectNode    `json:"expect"`
	ExpectComments map[string][]expectComment `json:"expect_comments"`
	// Gaps are what stays in Notion; plan is the words the import's plan
	// says it in, "" for a gap that is said nowhere but here.
	Gaps []struct {
		Gap  string `json:"gap"`
		Plan string `json:"plan"`
	} `json:"gaps"`
}

func TestANotionWorkspaceArrivesBlockForBlock(t *testing.T) {
	raw, err := os.ReadFile("testdata/n1/handbook.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx fidelityFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if fx.Label != "unverified against a real export" && fx.Label != "real export, personal content replaced" {
		t.Fatalf("the fixture says what it is: %q", fx.Label)
	}
	answers := map[string]string{}
	for k, v := range fx.Answers {
		answers[k] = string(v)
	}
	p, _, _ := withFakeNotion(t, answers)
	kept := func(_ context.Context, address string) (*safefetch.Response, error) {
		u, _ := url.Parse(address)
		body, ok := fx.Files[path.Base(u.Path)]
		if !ok {
			return &safefetch.Response{URL: u, Status: http.StatusNotFound}, nil
		}
		return &safefetch.Response{URL: u, Status: http.StatusOK, ContentType: "application/octet-stream", Body: []byte(body)}, nil
	}
	fetchFile = kept
	fetchPicture = func(ctx context.Context, address string) (*safefetch.Response, error) {
		r, err := kept(ctx, address)
		if r != nil {
			r.ContentType = "image/png"
		}
		return r, err
	}
	tr := &tree{p: p, ctx: context.Background(), tok: "secret", job: &importModels.Job{Id: uuid.New(), TriggeredBy: ptrUUID(uuid.New())}, titles: map[string]string{}, tables: map[string][]string{}}
	root := ""
	for k := range fx.Answers {
		if strings.HasPrefix(k, "GET /pages/") && !strings.Contains(string(fx.Answers[k]), `"page_id"`) {
			root = strings.TrimPrefix(k, "GET /pages/")
		}
	}
	if err := tr.page(root, ""); err != nil {
		t.Fatal(err)
	}
	if len(tr.pages) != len(fx.Expect) {
		t.Fatalf("%d pages, the fixture expects %d", len(tr.pages), len(fx.Expect))
	}
	for _, w := range tr.pages {
		want, ok := fx.Expect[w.title]
		if !ok {
			t.Fatalf("a page the fixture doesn't expect: %q", w.title)
		}
		html, err := tr.html(w)
		if err != nil {
			t.Fatal(err)
		}
		// As the docs pipeline reads it (business/Import pages_worker.go).
		body, err := contentcodec.FromHTML(html, contentcodec.HTMLOptions{Foreign: true, Assets: len(w.files) > 0})
		if err != nil {
			t.Fatal(err)
		}
		got := body.Content
		for i := 0; i < max(len(got), len(want)); i++ {
			if i >= len(got) || i >= len(want) {
				t.Errorf("%s: block %d: %d blocks arrived, %d expected\n%s", w.title, i, len(got), len(want), html)
				break
			}
			if diff := compareNode(got[i], want[i]); diff != "" {
				t.Errorf("%s: block %d (%s): %s", w.title, i, want[i].Type, diff)
			}
		}
		// The page's comments, one to one, by their author, at their time.
		wantComments := fx.ExpectComments[w.title]
		if len(w.comments) != len(wantComments) {
			t.Errorf("%s: %d comments arrived, %d expected: %+v", w.title, len(w.comments), len(wantComments), w.comments)
		}
		for i := 0; i < min(len(w.comments), len(wantComments)); i++ {
			got, want := w.comments[i], wantComments[i]
			if got.SourceID != want.ID || got.AuthorSourceID != want.Author || got.Created.UTC().Format(time.RFC3339) != want.Created || got.Body != want.Body {
				t.Errorf("%s: comment %d is %+v, expected %+v", w.title, i, got, want)
			}
		}
		// Every file a block names came with its page.
		body.Walk(func(n *contentcodec.Node) bool {
			if src, _ := n.Attrs["src"].(string); strings.HasPrefix(src, "files/") && w.files[src] == nil {
				t.Errorf("%s: %s names %s, which didn't come with the page", w.title, n.Type, src)
			}
			return true
		})
	}
	// What stays in Notion is said in the import's plan.
	plan := strings.ToLower(strings.Join(pagesWarnings, " "))
	for _, gap := range fx.Gaps {
		if gap.Plan != "" && !strings.Contains(plan, strings.ToLower(gap.Plan)) {
			t.Errorf("the plan doesn't say: %s", gap.Gap)
		}
	}
}

// compareNode says how a block differs from what is expected; "" when it
// doesn't.
func compareNode(n *contentcodec.Node, e expectNode) string {
	if n.Type != e.Type {
		return fmt.Sprintf("arrived as %s", n.Type)
	}
	for k, v := range e.Attrs {
		if fmt.Sprint(n.Attrs[k]) != fmt.Sprint(v) {
			return fmt.Sprintf("%s is %v, expected %v", k, n.Attrs[k], v)
		}
	}
	var words []string
	marks, inline, links := map[string]bool{}, map[string]bool{}, []string{}
	n.Walk(func(c *contentcodec.Node) bool {
		if c.Type == "text" {
			words = append(words, strings.Fields(c.Text)...)
		} else if c != n {
			inline[c.Type] = true
		}
		for _, m := range c.Marks {
			marks[m.Type] = true
			if m.Type == "link" {
				links = append(links, fmt.Sprint(m.Attrs["href"]))
			}
		}
		return true
	})
	if e.Text != "" && strings.Join(words, " ") != e.Text {
		return fmt.Sprintf("reads %q", strings.Join(words, " "))
	}
	for _, m := range e.Marks {
		if !marks[m] {
			return "no " + m + " mark"
		}
	}
	for _, k := range e.Inline {
		if !inline[k] {
			return "no " + k
		}
	}
	if e.Links != nil && !slices.Equal(links, e.Links) {
		return fmt.Sprintf("links %v", links)
	}
	if e.Columns != nil {
		var ws []float64
		for _, c := range n.Content {
			w, _ := c.Attrs["width"].(float64)
			ws = append(ws, w)
		}
		if !slices.Equal(ws, e.Columns) {
			return fmt.Sprintf("columns %v", ws)
		}
	}
	return ""
}
