package opensearchInit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// OpenSearch answers a delete of a document it hasn't got with a 404 whose
// body isn't an error, which the client reports as one. It's already gone.
func TestDeletingWhatIsGoneIsNoError(t *testing.T) {
	status, body := http.StatusNotFound, `{"_index":"ai_embeddings","_id":"post:x","_version":1,"result":"not_found","_shards":{"total":1,"successful":1,"failed":0}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/ai_embeddings/_doc/post:x" {
			t.Errorf("asked %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c, err := newClient(&OpenSearchConfig{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := deleteDocument(ctx, c, "ai_embeddings", "post:x"); err != nil {
		t.Fatalf("a document that isn't there: %v", err)
	}
	status, body = http.StatusNotFound, `{"error":{"type":"index_not_found_exception","reason":"no such index [ai_embeddings]"},"status":404}`
	if err := deleteDocument(ctx, c, "ai_embeddings", "post:x"); err != nil {
		t.Fatalf("an index that isn't there: %v", err)
	}
	status, body = http.StatusForbidden, `{"error":{"type":"security_exception","reason":"no permissions"},"status":403}`
	if err := deleteDocument(ctx, c, "ai_embeddings", "post:x"); err == nil {
		t.Fatal("a refusal is still an error")
	}
	status, body = http.StatusOK, `{"_index":"ai_embeddings","_id":"post:x","result":"deleted"}`
	if err := deleteDocument(ctx, c, "ai_embeddings", "post:x"); err != nil {
		t.Fatalf("a delete that worked: %v", err)
	}
	if err := deleteDocument(ctx, nil, "ai_embeddings", "post:x"); err == nil {
		t.Fatal("no client is an error")
	}
}
