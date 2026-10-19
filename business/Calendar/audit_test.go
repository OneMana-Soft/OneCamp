package Calendar

import (
	"testing"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/google/uuid"
)

// The audit names who did what to which event, and never its words.
func TestTheAuditRowNeverQuotesTheEvent(t *testing.T) {
	summary, meta := auditRow(resourceEventBusiness.Event{Kind: resourceEventBusiness.CalendarEventUpdated, ResourceKind: KindEvent,
		ResourceUUID: "e1", Actor: resourceEventBusiness.Actor{UserUUID: uuid.NewString()},
		Data: map[string]string{"changed": "time,location", "kind": "event", "title": "Acquisition talks"}})
	if summary != "Changed an event (time, location)" || meta["event_uuid"] != "e1" || meta["changed"] != "time,location" {
		t.Errorf("row %q %+v", summary, meta)
	}
	if _, ok := meta["title"]; ok {
		t.Error("the row quotes a title")
	}
	if s, _ := auditRow(resourceEventBusiness.Event{Kind: resourceEventBusiness.CalendarEventRSVP, Data: map[string]string{"by": "guest"}}); s != "A guest invited by email answered an invitation" {
		t.Errorf("a guest's answer: %q", s)
	}
}
