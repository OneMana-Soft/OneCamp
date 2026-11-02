package ai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// An AI entry that something else is writing when an access write's
// update-by-query reaches it (a bulk embed, another access write) is a
// version conflict, and conflicts "proceed" skips it: not written, not
// counted. One run answered done, so a board made public just after its
// words were embedded kept "private" for everyone else's AI until its words
// changed (CR-129, row 2131). Each run is refreshed first, so an entry
// embedded a moment ago is matched, and the write runs again while a
// conflict remains, as every search update-by-query does.
func TestAnAccessWriteThatMeetsAConflictRunsAgain(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(context.Context) (int, error)
	}{
		{"a board's access", func(ctx context.Context) (int, error) {
			return SetBoardAccess(ctx, "board-1", BoardAccess{Public: true, CreatedBy: "maya"})
		}},
		{"docs' access", func(ctx context.Context) (int, error) {
			return SetDocAccess(ctx, []DocAccess{{DocUUID: "doc-1", Private: true, CreatedBy: "maya"}})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var runs, refreshes atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				switch {
				case strings.HasSuffix(r.URL.Path, "/_refresh"):
					refreshes.Add(1)
					_, _ = io.WriteString(w, `{"_shards":{"total":1,"successful":1,"failed":0}}`)
				case strings.HasSuffix(r.URL.Path, "/_update_by_query"):
					if runs.Add(1) == 1 {
						// One entry written, one held by another write.
						_, _ = io.WriteString(w, `{"total":2,"updated":1,"version_conflicts":1,"failures":[]}`)
						return
					}
					_, _ = io.WriteString(w, `{"total":1,"updated":1,"version_conflicts":0,"failures":[]}`)
				default:
					http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
				}
			}))
			defer srv.Close()
			client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{srv.URL}}})
			if err != nil {
				t.Fatal(err)
			}
			prev := opensearchInit.OpenSearchClient
			opensearchInit.OpenSearchClient = client
			defer func() { opensearchInit.OpenSearchClient = prev }()

			n, err := tc.write(context.Background())
			if err != nil || n != 2 || runs.Load() != 2 {
				t.Fatalf("wrote %d entries over %d runs (err %v): the entry in conflict must be written by a second run", n, runs.Load(), err)
			}
			if refreshes.Load() != 2 {
				t.Fatalf("%d refreshes for 2 runs: each run is refreshed first, so an entry embedded a moment ago is matched", refreshes.Load())
			}
		})
	}
}
