package business

import (
	"strings"
	"testing"
	"time"

	auditModel "github.com/akashc777/OneCamp/models/postgres/AdminAudit"
)

// A refusal must reach the activity feed, and arrive marked as one.
//
// WHAT WAS WRONG. The homepage sells "a denied call leaves a row with the
// reason, the credential, the named agent, and the human behind it". The only
// refusal this system records is mcp.tool_call.refused. The feed drew on actions
// prefixed "ai." and "api." only, so that row was written, hash-chained, and
// shown to nobody: the one thing a competitor cannot copy next month was
// invisible inside the product that does it.
//
// The status matters as much as the presence. Every audit item arrived with an
// empty status, so even once a refusal reached the feed it would render exactly
// like a config change.
func TestRefusalsReachTheFeedAndAreMarked(t *testing.T) {
	entry := func(action string) *auditModel.AuditEntry {
		return &auditModel.AuditEntry{
			Action: action, Category: "agent", Summary: "…", CreatedAt: time.Now(),
		}
	}
	items := auditsToActivity([]*auditModel.AuditEntry{
		entry("mcp.tool_call.refused"),
		entry("mcp.tool_call.allowed"),
		entry("ai.config.update"),
		entry("agent.drill.refused"),
	})
	if len(items) != 4 {
		t.Fatalf("mapped %d items, want 4", len(items))
	}

	want := map[string]string{
		"mcp.tool_call.refused": ActivityStatusRefused,
		"mcp.tool_call.allowed": ActivityStatusAllowed,
		"ai.config.update":      "",
		"agent.drill.refused":   ActivityStatusRefused,
	}
	for _, it := range items {
		if got := it.Status; got != want[it.Title] {
			t.Errorf("%s has status %q, want %q", it.Title, got, want[it.Title])
		}
	}
}

// The prefixes are the filter that hid refusals, so they are pinned.
func TestActivityDrawsOnTheActionsThatCarryDecisions(t *testing.T) {
	for _, want := range []string{"mcp.", "agent."} {
		found := false
		for _, p := range aiActivityActionPrefixes {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Errorf("the activity feed does not read %q actions, so refusals recorded "+
				"under that prefix are written and never shown", want)
		}
	}
}

// Refused is not failed. A feed that files a working guarantee beside real
// errors teaches the reader to treat it as a fault.
func TestRefusedIsNotReportedAsAFailure(t *testing.T) {
	if strings.Contains(strings.ToLower(ActivityStatusRefused), "fail") {
		t.Error("the refused status reads as a failure")
	}
	if ActivityStatusRefused == ActivityStatusAllowed {
		t.Fatal("allowed and refused are the same value")
	}
}
