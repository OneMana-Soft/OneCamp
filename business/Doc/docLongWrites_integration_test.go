//go:build integration

package businness

// Duplicate and delete for good when the person's request ends part way,
// against Postgres 12 and Dgraph (REGRESSION-AUDIT, 24 Oct 2026). The web
// stops waiting after 30 s and the proxy then ends the request, which cancels
// its context. Both writes finish their work one page at a time after the
// part that can't be undone: a duplicate seeds each copy's body after the
// copies exist, and a purge forgets each page's search entry after the pages
// are gone from the graph. Cut there, the copies past the cut stayed blank,
// and the entries stayed in search with nothing left to purge again.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"slices"
	"sync"
	"testing"

	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/services/collab"
)

func TestADuplicateWhoseRequestEndsStillFillsEveryCopy(t *testing.T) {
	w := newQuietWorld(t)
	ctx := context.Background()
	root := w.pageUnder(t, "Launch plan", "<p>The plan.</p>", nil)
	sources := []string{root.Uuid}
	bodies := map[string]string{root.Uuid: "<p>The plan.</p>"}
	for _, title := range []string{"Budget", "Timeline", "Risks"} {
		body := "<p>" + title + " for the launch.</p>"
		page := w.pageUnder(t, title, body, root)
		sources = append(sources, page.Uuid)
		bodies[page.Uuid] = body
	}
	editable := map[string]bool{}
	for _, id := range sources {
		editable[id] = true
	}

	// The request ends once the first copy's body has gone in: the web
	// stopped waiting.
	request, leave := context.WithCancel(ctx)
	defer leave()
	collab.Live = cutAfterFirstSeed{collabSaves: &collabSaves{}, leave: leave}

	copied, err := DuplicateDocs(request, w.sam, sources, editable, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.heard(t)
	if len(copied.Copies) != len(sources) {
		t.Fatalf("made %d copies of %d pages", len(copied.Copies), len(sources))
	}
	for _, c := range copied.Copies {
		if got, err := readStoredBody(ctx, c.UUID); err != nil || got != bodies[c.SourceUUID] {
			t.Errorf("the copy of %s holds %q (%v); want its source's %q", c.SourceUUID, got, err, bodies[c.SourceUUID])
		}
	}
}

func TestAPurgeWhoseRequestEndsStillForgetsEveryPage(t *testing.T) {
	w := newQuietWorld(t)
	ctx := context.Background()
	root := w.pageUnder(t, "Old launch", "", nil)
	pages := []string{root.Uuid}
	for _, title := range []string{"Old budget", "Old timeline", "Old risks"} {
		pages = append(pages, w.pageUnder(t, title, "", root).Uuid)
	}
	if _, err := TrashSubtree(ctx, root.Uuid, w.sam.Uid, w.sam.Uuid, ""); err != nil {
		t.Fatal(err)
	}
	w.heard(t)

	// Search, which hears each page's entry forgotten; the request ends at
	// the first: the web stopped waiting.
	request, leave := context.WithCancel(ctx)
	defer leave()
	var mu sync.Mutex
	var forgot []string
	search := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			mu.Lock()
			forgot = append(forgot, path.Base(r.URL.Path))
			mu.Unlock()
			leave()
		}
		fmt.Fprint(rw, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[],"result":"deleted","_id":"x"}`)
	}))
	t.Cleanup(search.Close)
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client

	purged, err := PurgeDoc(request, root.Uuid, w.sam.Uid, w.sam.Uuid, false)
	if err != nil || purged.Count != len(pages) {
		t.Fatalf("the purge answered %+v, %v; want all %d pages gone", purged, err, len(pages))
	}
	w.heard(t)
	mu.Lock()
	defer mu.Unlock()
	if got, want := slices.Sorted(slices.Values(forgot)), slices.Sorted(slices.Values(pages)); !slices.Equal(got, want) {
		t.Errorf("search forgot %v; want every page gone for good %v", got, want)
	}
}

// cutAfterFirstSeed is the collaboration service, whose first seed ends the
// request that sent it.
type cutAfterFirstSeed struct {
	*collabSaves
	leave context.CancelFunc
}

func (s cutAfterFirstSeed) Seed(ctx context.Context, docUUID, html string, o collab.Options) (collab.Result, error) {
	r, err := s.collabSaves.Seed(ctx, docUUID, html, o)
	s.leave()
	return r, err
}
