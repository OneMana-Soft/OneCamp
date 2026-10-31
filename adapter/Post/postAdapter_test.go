package adapter

import (
	"encoding/json"
	"testing"
)

// A post's kind and facts are the server's to set (a call's post, calls
// CALLS-3b): no request body sets them, whatever it names them.
func TestARequestSetsNoPostKind(t *testing.T) {
	var in InputCreateOrUpdatePostInfo
	if err := json.Unmarshal([]byte(`{"post_text_html":"hi","kind":"huddle","post_kind":"huddle","Kind":"huddle","system":"{}","post_system":"{}","System":"{}"}`), &in); err != nil {
		t.Fatal(err)
	}
	if in.Kind != "" || in.System != "" || in.HTMLText != "hi" {
		t.Errorf("decoded %+v", in)
	}
}
