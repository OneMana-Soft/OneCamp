package business

import (
	"context"
	"testing"
	"time"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/google/uuid"
)

func boardEvent(kind resourceEventBusiness.Kind, private string, data map[string]string, people ...string) resourceEventBusiness.Event {
	d := map[string]string{"title": "Launch plan", "private": private}
	for k, v := range data {
		d[k] = v
	}
	return resourceEventBusiness.Event{Kind: kind, ResourceKind: KindBoard, ResourceUUID: "b1", Subjects: people,
		Actor: resourceEventBusiness.Actor{UserUUID: uuid.NewString()}, At: time.Unix(0, 0), Data: d}
}

// A share says what the people can now do, and an export what it made and
// where; a private board is never named, and its title stays out of the row.
func TestBoardAuditRowSaysWhatHappenedAndNeverNamesAPrivateBoard(t *testing.T) {
	cases := []struct {
		e        resourceEventBusiness.Event
		summary  string
		hasTitle bool
	}{
		{boardEvent(resourceEventBusiness.BoardShared, "false", map[string]string{"level": "edit"}, "u1", "u2"), "Shared ‘Launch plan’ with 2 people who can edit", true},
		{boardEvent(resourceEventBusiness.BoardShared, "true", map[string]string{"level": "view"}, "u1"), "Shared a private board with 1 person who can view", false},
		{boardEvent(resourceEventBusiness.BoardExported, "false", map[string]string{"format": "pdf", "via": "browser"}), "Exported ‘Launch plan’ as a PDF, in the browser", true},
		{boardEvent(resourceEventBusiness.BoardExported, "false", map[string]string{"format": "csv", "via": "browser"}), "Exported ‘Launch plan’ as a CSV of its notes, in the browser", true},
		{boardEvent(resourceEventBusiness.BoardExported, "", map[string]string{"format": "png", "via": "browser"}), "Exported a private board as png, in the browser", false},
	}
	for _, c := range cases {
		summary, meta := boardAuditRow(c.e)
		if summary != c.summary {
			t.Errorf("%s: summary %q, want %q", c.e.Kind, summary, c.summary)
		}
		if _, ok := meta["title"]; ok != c.hasTitle {
			t.Errorf("%s %q: title in the row %v, want %v", c.e.Kind, summary, ok, c.hasTitle)
		}
		if meta["resource_kind"] != KindBoard || meta["resource_uuid"] != "b1" {
			t.Errorf("%s: meta %v, want the board's kind and id", c.e.Kind, meta)
		}
	}
	if _, meta := boardAuditRow(cases[0].e); meta["people"] != "u1,u2" || meta["level"] != "edit" {
		t.Errorf("a share's row %v, want its people and level", meta)
	}
}

// The board's events go into the log under "boards"; nothing else of a
// board's, and nothing of another kind's, is taken.
func TestBoardEventsAreRecordedUnderBoards(t *testing.T) {
	var categories []string
	oldR, oldE := auditBusiness.RecordEventRow, auditBusiness.EventActorEmail
	t.Cleanup(func() { auditBusiness.RecordEventRow, auditBusiness.EventActorEmail = oldR, oldE })
	auditBusiness.RecordEventRow = func(_ context.Context, _ *uuid.UUID, _, _, _, category, _ string, _ map[string]interface{}) error {
		categories = append(categories, category)
		return nil
	}
	auditBusiness.EventActorEmail = func(context.Context, uuid.UUID) string { return "" }
	for _, kind := range []resourceEventBusiness.Kind{resourceEventBusiness.BoardShared, resourceEventBusiness.BoardExported} {
		e := boardEvent(kind, "false", nil)
		if !boardsAudit.Wants(e) {
			t.Fatalf("%s isn't recorded", kind)
		}
		boardsAudit.Run(context.Background(), e)
	}
	if len(categories) != 2 || categories[0] != auditBusiness.CategoryBoards || categories[1] != auditBusiness.CategoryBoards {
		t.Fatalf("rows under %v, want two under %q", categories, auditBusiness.CategoryBoards)
	}
	other := boardEvent(resourceEventBusiness.DocShared, "false", nil)
	notBoard := boardEvent(resourceEventBusiness.BoardShared, "false", nil)
	notBoard.ResourceKind = "doc"
	if boardsAudit.Wants(other) || boardsAudit.Wants(notBoard) {
		t.Fatalf("the boards' sink takes another area's events")
	}
}
