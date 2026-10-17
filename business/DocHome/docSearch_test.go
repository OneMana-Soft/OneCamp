package business

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	docSearchModel "github.com/akashc777/OneCamp/models/openSearch/DocSearch"
)

func TestSafePassageKeepsOnlyMarks(t *testing.T) {
	got := SafePassage(`the <mark>launch</mark> <script>x</script> &amp; <b>bold</b>`)
	want := `the <mark>launch</mark> &lt;script&gt;x&lt;/script&gt; &amp; &lt;b&gt;bold&lt;/b&gt;`
	if got != want {
		t.Errorf("got %s", got)
	}
}

func TestASearchNeedsTwoLetters(t *testing.T) {
	if _, err := cleanSearchText(" a "); err != ErrSearchTooShort {
		t.Errorf("one letter: %v", err)
	}
	got, _ := cleanSearchText(strings.Repeat("é", 300))
	if len([]rune(got)) != maxSearchRunes {
		t.Errorf("a search is bounded: %d", len([]rune(got)))
	}
}

// fakeRules lets the person open the docs listed, and nothing else.
type fakeRules struct {
	accessBusiness.Resolver
	open map[string]bool
}

func (f fakeRules) Filter(_ context.Context, _ accessBusiness.Subject, res []accessBusiness.Resource, _ accessBusiness.Level) ([]accessBusiness.Resource, error) {
	var out []accessBusiness.Resource
	for _, r := range res {
		if f.open[r.UUID] {
			out = append(out, r)
		}
	}
	return out, nil
}

func TestSearchNamesOnlyWhatThePersonMayOpen(t *testing.T) {
	saved, savedRun, savedRows := accessBusiness.Rules, runDocSearch, readDocRows
	defer func() { accessBusiness.Rules, runDocSearch, readDocRows = saved, savedRun, savedRows }()
	accessBusiness.Rules = fakeRules{open: map[string]bool{"a": true, "c": true, "top": true}}
	runDocSearch = func(context.Context, string) ([]docSearchModel.DocHit, int, error) {
		return []docSearchModel.DocHit{
			{UUID: "a", BodyPassages: []string{"the <mark>plan</mark>"}},
			{UUID: "b", BodyPassages: []string{"secret <mark>plan</mark>"}},
			{UUID: "c", TitlePassage: "<mark>Plan</mark> B"},
			{UUID: "a"},
		}, 4, nil
	}
	hidden := &dgraphStruct.DgraphDoc{Uuid: "hidden", Title: "Private parent"}
	readDocRows = func(_ context.Context, ids []string, _ string) ([]*dgraphStruct.DgraphDoc, error) {
		var out []*dgraphStruct.DgraphDoc
		for _, id := range ids {
			switch id {
			case "a":
				out = append(out, &dgraphStruct.DgraphDoc{Uuid: "a", Title: "A", Parent: &dgraphStruct.DgraphDoc{Uuid: "top", Title: "Top"}})
			case "c":
				out = append(out, &dgraphStruct.DgraphDoc{Uuid: "c", Title: "C", Parent: hidden})
			case "b":
				t.Error("a doc the person can't open is never read for the page")
			}
		}
		return out, nil
	}
	page, err := SearchDocs(context.Background(), Searcher{}, "plan", "", 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if page.Count != 2 || len(page.Docs) != 2 || page.Docs[0].UUID != "a" || page.Docs[1].UUID != "c" {
		t.Fatalf("got %+v", page)
	}
	if page.Docs[0].Highlights[0] != "the <mark>plan</mark>" || len(page.Docs[0].Path) != 1 || page.Docs[0].Path[0].Title != "Top" {
		t.Errorf("a's passage and path: %+v", page.Docs[0])
	}
	if c := page.Docs[1]; !c.Path[0].Redacted || c.Path[0].Title != "" || c.TitleHighlight != "<mark>Plan</mark> B" {
		t.Errorf("a page above that can't be opened is never named: %+v", c)
	}
	second, _ := SearchDocs(context.Background(), Searcher{}, "plan", "", 0, 1)
	if len(second.Docs) != 1 || second.NextCursor == "" || helpers.CursorOffset(second.NextCursor) != 1 {
		t.Errorf("paged: %+v", second)
	}
}

// chunkRules lets the person open every doc whose id doesn't start with "x",
// and fails a chunk holding "boom"; it counts the docs it was asked about.
type chunkRules struct {
	accessBusiness.Resolver
	mu    *sync.Mutex
	asked *int
	most  *int
}

func (c chunkRules) Filter(_ context.Context, _ accessBusiness.Subject, res []accessBusiness.Resource, _ accessBusiness.Level) ([]accessBusiness.Resource, error) {
	c.mu.Lock()
	*c.asked += len(res)
	*c.most = max(*c.most, len(res))
	c.mu.Unlock()
	var out []accessBusiness.Resource
	for _, r := range res {
		if r.UUID == "boom" {
			return nil, errors.New("the graph is away")
		}
		if !strings.HasPrefix(r.UUID, "x") {
			out = append(out, r)
		}
	}
	return out, nil
}

func TestASearchsDocsAreCheckedInChunksAndAllOfThemAre(t *testing.T) {
	saved := accessBusiness.Rules
	defer func() { accessBusiness.Rules = saved }()
	var mu sync.Mutex
	asked, most := 0, 0
	accessBusiness.Rules = chunkRules{mu: &mu, asked: &asked, most: &most}

	var ids []string
	for i := 0; i < 130; i++ {
		id := fmt.Sprintf("d%03d", i)
		if i%10 == 0 {
			id = "x" + id
		}
		ids = append(ids, id)
	}
	open, err := readable(context.Background(), accessBusiness.Subject{}, ids)
	if err != nil {
		t.Fatal(err)
	}
	if asked != 130 || most > filterChunk {
		t.Fatalf("asked about %d docs, %d at most at once", asked, most)
	}
	if len(open) != 117 || open["xd000"] || !open["d129"] {
		t.Fatalf("%d open: %v", len(open), open)
	}

	if _, err := readable(context.Background(), accessBusiness.Subject{}, append(ids, "boom")); err == nil {
		t.Fatal("a chunk the rule couldn't check fails the whole check")
	}
}
