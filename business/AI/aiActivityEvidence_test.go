package business

import (
	"testing"
	"time"

	auditModel "github.com/akashc777/OneCamp/models/postgres/AdminAudit"
)

// A refusal in the feed carries the row that proves it.
//
// The feed and the audit log were two accounts of one event: the feed said an
// agent was stopped, the log held the hash-chained proof, and nothing connected
// them. An admin had to search the log by hand. A member, who cannot open the
// admin log at all, had no way to check a claim being made to them about their
// own agent.
//
// So an audit-derived row carries the sequence number and BOTH hashes. Both,
// because one hash on its own demonstrates nothing — the link is the claim, and
// the link is what an edit breaks.
func TestAnAuditRowInTheFeedCarriesItsEvidence(t *testing.T) {
	entries := []*auditModel.AuditEntry{{
		Seq:        1412,
		Action:     "mcp.tool_call.refused",
		Category:   "agent",
		ActorEmail: "priya@example.test",
		Summary:    "post in #finance refused",
		PrevHash:   "9f2c41ab",
		EntryHash:  "7d10c8e5",
		CreatedAt:  time.Now().UTC(),
	}}

	items := auditsToActivity(entries)
	if len(items) != 1 {
		t.Fatalf("wanted one item, got %d", len(items))
	}
	it := items[0]
	if it.Seq != 1412 {
		t.Errorf("the row does not say where it is in the log: seq %d", it.Seq)
	}
	if it.PrevHash == "" || it.EntryHash == "" {
		t.Errorf("a hash pair is the claim; got prev=%q this=%q", it.PrevHash, it.EntryHash)
	}
	if it.Status != "refused" {
		t.Errorf("a refusal must arrive marked as one, got %q", it.Status)
	}
}

// An agent run is not an audit row and has no place in the chain. Inventing a
// sequence number for one would be worse than leaving it out: it would point a
// reader at a row that does not exist.
func TestAnAgentRunCarriesNoChainPosition(t *testing.T) {
	item := AIActivityItem{Kind: "agent_run", Title: "Release Captain", Status: "succeeded"}
	if item.Seq != 0 || item.PrevHash != "" || item.EntryHash != "" {
		t.Error("an agent run should carry no chain position at all")
	}
}

// TestActivityCarriesWhoStartedIt pins the feed's reading of the audit row's
// initiator: the same key the audit layer writes, so a member's own feed and
// an auditor's log give one answer to "was anybody there".
func TestActivityCarriesWhoStartedIt(t *testing.T) {
	meta := `{"initiator":"schedule","agent_id":"a1"}`
	items := auditsToActivity([]*auditModel.AuditEntry{{Action: "agent.run", Metadata: &meta}})
	if len(items) != 1 || items[0].Initiator != "schedule" {
		t.Fatalf("expected initiator schedule, got %+v", items)
	}

	// A row that never said stays silent rather than defaulting to person.
	none := auditsToActivity([]*auditModel.AuditEntry{{Action: "agent.run"}})
	if none[0].Initiator != "" {
		t.Errorf("a row with no initiator was given %q", none[0].Initiator)
	}
	bad := `not json`
	broken := auditsToActivity([]*auditModel.AuditEntry{{Action: "agent.run", Metadata: &bad}})
	if broken[0].Initiator != "" {
		t.Errorf("malformed metadata produced %q", broken[0].Initiator)
	}
}
