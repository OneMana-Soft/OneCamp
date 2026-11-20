package provider

import "testing"

// A source task is in its source's triage when its status is (Linear's
// state type "triage", the default map's) or when the plan read its status as
// one of that kind (a Linear triage state the team renamed).
func TestInTriageReadsTheStatusOrItsKind(t *testing.T) {
	kinds := map[string]string{"inbox": "triage", "doing": "started"}
	cases := map[string]bool{"triage": true, " Triage ": true, "Inbox": true, "Doing": false, "backlog": false, "": false}
	for status, want := range cases {
		if got := InTriage(status, kinds); got != want {
			t.Errorf("InTriage(%q) = %v, want %v", status, got, want)
		}
	}
	if !InTriage("triage", nil) || InTriage("inbox", nil) {
		t.Error("without a plan's kinds, only the triage status is triage")
	}
}
