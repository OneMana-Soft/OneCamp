package businness

import (
	"testing"
	"time"

	userDomain "github.com/akashc777/OneCamp/domain/User"
	auditModels "github.com/akashc777/OneCamp/models/postgres/AdminAudit"
	"github.com/google/uuid"
)

// A doc's changes as its insights show them (docs C6-3): the record's
// summary, what kind of actor, the person's name (for an agent, who asked
// it), and when; never their email.
func TestADocsChangesNameWhoMadeThem(t *testing.T) {
	maya := uuid.New()
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	rows := []*auditModels.AuditEntry{
		{Action: "doc.published", Summary: "Published ‘Launch notes’", ActorKind: "human", ActorID: &maya, ActorEmail: "maya@example.test", CreatedAt: at},
		nil,
		{Action: "agent.doc.shared", Summary: "Shared ‘Launch notes’ with 1 person", ActorKind: "agent", ActorID: &maya, CreatedAt: at.Add(-time.Hour)},
		{Action: "doc.purged", Summary: "Deleted a page for good", ActorKind: "system", CreatedAt: at.Add(-2 * time.Hour)},
	}
	got := eventsOf(rows, map[string]userDomain.UserDisplay{maya.String(): {Display: "Maya Patel"}})
	want := []DocEvent{
		{Action: "doc.published", Summary: "Published ‘Launch notes’", ActorKind: "human", Actor: "Maya Patel", At: at},
		{Action: "agent.doc.shared", Summary: "Shared ‘Launch notes’ with 1 person", ActorKind: "agent", Actor: "Maya Patel", At: at.Add(-time.Hour)},
		{Action: "doc.purged", Summary: "Deleted a page for good", ActorKind: "system", At: at.Add(-2 * time.Hour)},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d: %+v, want %+v", i, got[i], want[i])
		}
	}
}
