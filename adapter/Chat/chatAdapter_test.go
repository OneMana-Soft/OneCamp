package adapter

import (
	"encoding/json"
	"testing"
)

// A message's kind and facts are the server's to set (a call's message,
// calls CALLS-3b): no request body sets them, whatever it names them.
func TestARequestSetsNoChatKind(t *testing.T) {
	var in ChatInfo
	if err := json.Unmarshal([]byte(`{"text_html":"hi","kind":"huddle","chat_kind":"huddle","Kind":"huddle","system":"{}","chat_system":"{}","System":"{}"}`), &in); err != nil {
		t.Fatal(err)
	}
	if in.Kind != "" || in.System != "" || in.TextHtml != "hi" {
		t.Errorf("decoded %+v", in)
	}
}
