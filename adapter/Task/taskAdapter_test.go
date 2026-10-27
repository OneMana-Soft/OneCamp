package adapter

import (
	"encoding/json"
	"testing"
)

// A comment's time, and its files' with it, is the server's: only a backfill
// sets it, in Go (business/GitHub's sync). A request that names one is read
// for everything else and never for its time.
func TestACommentRequestNeverSetsItsTime(t *testing.T) {
	var in CreateOrUpdateTaskCommentInput
	body := `{"task_comment_body":"<p>hi</p>","task_uuid":"t-1","created_at":"2001-01-01T00:00:00Z"}`
	if err := json.Unmarshal([]byte(body), &in); err != nil {
		t.Fatalf("reading a comment request: %v", err)
	}
	if in.CreatedAt != nil {
		t.Errorf("a request set the comment's time to %v", in.CreatedAt)
	}
	if in.CommentBody != "<p>hi</p>" || in.TaskUuid != "t-1" {
		t.Errorf("the rest of the request wasn't read: %+v", in)
	}
}
