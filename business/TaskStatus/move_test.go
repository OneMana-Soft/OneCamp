package business

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestMoveFilterMatches(t *testing.T) {
	proj, other, qa := uuid.NewString(), uuid.NewString(), uuid.NewString()
	cases := []struct {
		name   string
		filter MoveFilter
		move   Move
		want   bool
	}{
		{"any move", MoveFilter{}, Move{ProjectID: proj, OldStatus: "todo", NewStatus: "inProgress"}, true},
		{"another project", MoveFilter{ProjectID: proj}, Move{ProjectID: other, OldStatus: "todo", NewStatus: "done"}, false},
		{"into done", MoveFilter{ProjectID: proj, ToStatus: "done"}, Move{ProjectID: proj, OldStatus: "inReview", NewStatus: "done"}, true},
		{"into a custom status counting as done", MoveFilter{ToStatus: "done"}, Move{OldStatus: "inReview", NewStatus: "done", NewCustom: uuid.NewString()}, true},
		{"within done is not into done", MoveFilter{ToStatus: "done"}, Move{OldStatus: "done", NewStatus: "done", OldCustom: uuid.NewString()}, false},
		{"into QA", MoveFilter{ToStatus: qa}, Move{OldStatus: "inProgress", NewStatus: "inReview", NewCustom: qa}, true},
		{"into In Review but not QA", MoveFilter{ToStatus: qa}, Move{OldStatus: "inProgress", NewStatus: "inReview"}, false},
		{"out of QA", MoveFilter{ToStatus: qa}, Move{OldStatus: "inReview", NewStatus: "done", OldCustom: qa}, false},
	}
	for _, c := range cases {
		if got := c.filter.Matches(c.move); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestMoveFromEvent(t *testing.T) {
	m := MoveFromEvent(map[string]interface{}{"project_id": "p", "old_status": "todo", "new_status": "inReview", "new_custom_id": "qa", "old_custom_id": "", "other": 3})
	if m != (Move{ProjectID: "p", OldStatus: "todo", NewStatus: "inReview", NewCustom: "qa"}) {
		t.Fatalf("%+v", m)
	}
}

// Built-ins need no database; a project's own statuses are in the integration test.
func TestNormalizeMoveFilter(t *testing.T) {
	ctx := context.Background()
	f, err := NormalizeMoveFilter(ctx, "", "In review")
	if err != nil || f != (MoveFilter{ToStatus: "inReview"}) {
		t.Fatalf("%+v %v", f, err)
	}
	if f, err := NormalizeMoveFilter(ctx, "", ""); err != nil || f != (MoveFilter{}) {
		t.Fatalf("empty: %+v %v", f, err)
	}
	if _, err := NormalizeMoveFilter(ctx, "", "QA"); err == nil || !strings.Contains(err.Error(), "project") {
		t.Fatalf("a project's own status with no project: %v", err)
	}
	if _, err := NormalizeMoveFilter(ctx, "not-a-uuid", ""); err == nil {
		t.Fatal("a bad project id was accepted")
	}
}
