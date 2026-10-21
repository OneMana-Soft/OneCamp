package business

import "testing"

// An event gets one id, kept if it already has one, without touching the
// caller's map.
func TestListenersHearEachEventWithItsOwnID(t *testing.T) {
	in := map[string]interface{}{"task_id": "t"}
	out := withEventID(in)
	id, _ := out["event_id"].(string)
	if id == "" || out["task_id"] != "t" {
		t.Fatalf("%v", out)
	}
	if _, touched := in["event_id"]; touched {
		t.Error("the caller's map changed")
	}
	if again := withEventID(out); again["event_id"] != id {
		t.Error("a redelivered event got a new id")
	}
}
