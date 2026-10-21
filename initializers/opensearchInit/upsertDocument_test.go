package opensearchInit

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// The whole document, as when a person is created, is written even when the
// index has none: an index the reset emptied fills again as things are made.
func TestAWholeDocumentIsCreatedWhenItIsNotThere(t *testing.T) {
	sent := searchServer(t, http.StatusCreated, `{"_index":"users","_id":"u1","result":"created"}`)
	if err := UpsertDocument(context.Background(), "users", "u1", strings.NewReader(`{"doc":{"user_name":"kim","user_full_name":"Kim Lee"}}`)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(*sent, `"doc_as_upsert":true`) || !strings.Contains(*sent, `"user_full_name":"Kim Lee"`) {
		t.Errorf("sent %s, want the whole document with doc_as_upsert", *sent)
	}
}
