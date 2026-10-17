package models

import (
	"encoding/json"
	"strings"
	"testing"
)

// Every doc payload names its doc doc_uuid; the comment event said doc_id.
// It says both for one release, so an app that reads only doc_id still works.
func TestADocCommentEventNamesItsDocBothWays(t *testing.T) {
	raw, err := json.Marshal(MqttDocComment{Type: TYPE_CREATE, CommentUuid: "c1", DocUuid: "d1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"doc_uuid":"d1"`, `"doc_id":"d1"`, `"comment_uuid":"c1"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("%s lacks %s", raw, want)
		}
	}
	var back MqttDocComment
	if err := json.Unmarshal(raw, &back); err != nil || back.DocUuid != "d1" {
		t.Fatalf("read back %+v, %v", back, err)
	}
	raw, _ = json.Marshal(MqttDocComment{Type: TYPE_DELETE, CommentUuid: "c2"})
	if strings.Contains(string(raw), "doc_uuid") {
		t.Errorf("an event with no doc names one: %s", raw)
	}
}
