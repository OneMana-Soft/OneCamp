package business

import "testing"

func TestVerifyRunClaims(t *testing.T) {
	cases := []struct {
		name      string
		draft     string
		failedWr  []string
		wantNeeds bool
	}{
		{"no failures → no correction", "Done — created the task.", nil, false},
		{"failed write + claims done → correct", "Done — I've created the task.", []string{"create_task"}, true},
		{"failed write but honest → no correction", "I couldn't create the task; it failed. Please retry.", []string{"create_task"}, false},
		{"failed write but no success claim → no correction", "Here's what I found in the channel.", []string{"create_task"}, false},
		{"failed write + 'successfully' → correct", "Successfully assigned the task to Sam.", []string{"assign_task"}, true},
		{"empty draft → no correction", "", []string{"create_task"}, false},
		{"failed write + acknowledges partial → no correction", "Updated the doc, but assigning failed — please retry.", []string{"assign_task"}, false},
	}
	for _, c := range cases {
		got := verifyRunClaims(c.draft, c.failedWr)
		if got.NeedsCorrection != c.wantNeeds {
			t.Errorf("%s: verifyRunClaims(%q, %v) NeedsCorrection = %v, want %v", c.name, c.draft, c.failedWr, got.NeedsCorrection, c.wantNeeds)
		}
	}
}

func TestClaimCorrectionMentionsTools(t *testing.T) {
	msg := claimCorrection([]string{"create_task", "assign_task", "create_task"})
	if !contains(msg, "create task") || !contains(msg, "assign task") {
		t.Errorf("claimCorrection should humanize + list failed tools, got: %q", msg)
	}
	// De-duplicated: "create task" should appear once.
	if countOccurrences(msg, "create task") != 1 {
		t.Errorf("claimCorrection should de-duplicate tool names, got: %q", msg)
	}
}

func contains(s, sub string) bool { return countOccurrences(s, sub) > 0 }

func countOccurrences(s, sub string) int {
	n := 0
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			n++
		}
	}
	return n
}
