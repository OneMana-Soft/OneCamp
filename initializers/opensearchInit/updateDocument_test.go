package opensearchInit

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A partial update asks OpenSearch to try again when another write to the
// document lands in between, rather than fail and lose its change.
func TestUpdatesRetryOnConflict(t *testing.T) {
	var query, path, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		query, path, body = r.URL.RawQuery, r.URL.Path, string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"_index":"tasks","_id":"t1","result":"updated"}`))
	}))
	defer srv.Close()
	c, err := newClient(&OpenSearchConfig{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := updateDocument(context.Background(), c, "tasks", "t1", strings.NewReader(`{"doc":{"task_name":"Ship"}}`)); err != nil {
		t.Fatal(err)
	}
	if path != "/tasks/_update/t1" || !strings.Contains(query, "retry_on_conflict=5") || body != `{"doc":{"task_name":"Ship"}}` {
		t.Fatalf("sent %s?%s %s", path, query, body)
	}
	if err := updateDocument(context.Background(), nil, "tasks", "t1", strings.NewReader(`{}`)); err == nil {
		t.Fatal("no client is an error, not a panic")
	}
}
