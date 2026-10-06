package opensearchInit

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// A node whose memory breaker trips answers 429 and does nothing. The write
// must be sent again, whole, rather than dropped.
func TestClientRetriesWhatABusyNodeTurnedAway(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		n := len(bodies)
		mu.Unlock()
		if n < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"type":"circuit_breaking_exception"},"status":429}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"_index":"posts","_id":"p1","result":"created"}`))
	}))
	defer srv.Close()

	c, err := newClient(&OpenSearchConfig{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Document.Create(context.Background(), opensearchapi.DocumentCreateReq{Index: "posts", DocumentID: "p1", Body: strings.NewReader(`{"post_body":"hello"}`)})
	if err != nil {
		t.Fatalf("the write failed after the node recovered: %v", err)
	}
	if len(bodies) != 3 {
		t.Fatalf("sent %d times, want 3 (two refusals, then the write)", len(bodies))
	}
	for i, b := range bodies {
		if b != `{"post_body":"hello"}` {
			t.Fatalf("attempt %d sent %q, not the whole document", i+1, b)
		}
	}
}
