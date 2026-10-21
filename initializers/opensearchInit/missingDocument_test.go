package opensearchInit

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// searchServer stands in for OpenSearch: it answers each update with status
// and reply, records the body it was sent, and is the client the package's
// own functions use for the test.
func searchServer(t *testing.T, status int, reply string) *string {
	t.Helper()
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	c, err := newClient(&OpenSearchConfig{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	prev := OpenSearchClient
	OpenSearchClient = c
	t.Cleanup(func() { OpenSearchClient = prev })
	return &body
}

const documentMissingReply = `{"error":{"root_cause":[{"type":"document_missing_exception","reason":"[eaab1b6a]: document missing","index":"tasks"}],"type":"document_missing_exception","reason":"[eaab1b6a]: document missing","index":"tasks"},"status":404}`

// The demo's nightly reset empties search, and every rename after it failed
// at ERROR: document_missing_exception, 21 in a week. A partial update to a
// document that isn't there is skipped, not failed and not written.
func TestAPartialUpdateToAMissingDocumentIsSkipped(t *testing.T) {
	searchServer(t, http.StatusNotFound, documentMissingReply)
	if err := UpdateDocument(context.Background(), "tasks", "eaab1b6a", strings.NewReader(`{"doc":{"task_name":"Ship"}}`)); err != nil {
		t.Errorf("a missing document failed the update: %v", err)
	}
}

// A partial update never creates a document: one holding only the fields a
// rename sends (a board's privacy without its access) is worse than none.
func TestAPartialUpdateNeverCreatesTheDocument(t *testing.T) {
	sent := searchServer(t, http.StatusOK, `{"_index":"boards","_id":"b1","result":"noop"}`)
	if err := UpdateDocument(context.Background(), "boards", "b1", strings.NewReader(`{"doc":{"title":"Plan"},"doc_as_upsert":true}`)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(*sent, "doc_as_upsert") || !strings.Contains(*sent, `"title":"Plan"`) {
		t.Errorf("sent %s, want the fields without doc_as_upsert", *sent)
	}
}

// Any other refusal is still an error.
func TestAnUpdateSearchRefusedIsStillAnError(t *testing.T) {
	searchServer(t, http.StatusInternalServerError, `{"error":{"root_cause":[],"type":"exception","reason":"boom"},"status":500}`)
	if err := UpdateDocument(context.Background(), "tasks", "t1", strings.NewReader(`{"doc":{"task_name":"Ship"}}`)); err == nil {
		t.Error("a refused update reported success")
	}
}
