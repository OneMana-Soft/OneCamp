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
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// Backend audit #20: the AskAI context searched the recent messages of each
// channel a question named, one search (and permission filter) apiece,
// until one had something. The channels it names are one search.
func TestTheChannelsAQuestionNamesAreOneSearch(t *testing.T) {
	var searches atomic.Int32
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		searches.Add(1)
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = io.WriteString(w, `{"hits":{"hits":[]}}`)
	}))
	defer srv.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{srv.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	prev := opensearchInit.OpenSearchClient
	opensearchInit.OpenSearchClient = client
	defer func() { opensearchInit.OpenSearchClient = prev }()
	// Nothing is restricted. Deciding that reads Postgres (models/taskread),
	// which a unit test doesn't connect; run with the integration tag, the
	// tests before this one leave the server's pool reaching nothing.
	decide := taskread.Decide
	taskread.Decide = func(context.Context, string) (map[string]bool, error) { return nil, nil }
	defer func() { taskread.Decide = decide }()

	if _, err := SearchRecentIn(context.Background(), "me", []string{"c1", "c2"}, nil, nil, "channel_uuid", []string{"c1", "c2"}, 15); err != nil {
		t.Fatal(err)
	}
	if searches.Load() != 1 {
		t.Errorf("%d searches for two channels", searches.Load())
	}
	if !strings.Contains(body, `"terms": {"channel_uuid": ["c1","c2"]}`) {
		t.Errorf("the search: %s", body)
	}
	if res, err := SearchRecentIn(context.Background(), "me", nil, nil, nil, "channel_uuid", nil, 15); err != nil || res != nil || searches.Load() != 1 {
		t.Errorf("no channels searched anyway: %v %v %d", res, err, searches.Load())
	}
}
