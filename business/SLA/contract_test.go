package business

import (
	"context"
	"testing"
	"time"

	model "github.com/akashc777/OneCamp/models/postgres/SLA"
	"github.com/google/uuid"
)

// StateOf is where a task's kept timer stands now: none while its project's
// SLAs are off, none for a task with no timer, else its state and due.
func TestStateOfReadsTheKeptTimer(t *testing.T) {
	task := uuid.New()
	due := time.Now().Add(48 * time.Hour)
	risk := due.Add(-24 * time.Hour)
	kept := map[uuid.UUID]model.Timer{task: {TaskID: task, ProjectID: project, State: model.Running, DueAt: &due, RiskAt: &risk}}
	on := false
	swap(t, &timerOf, func(_ context.Context, id uuid.UUID) (model.Timer, bool, error) {
		x, ok := kept[id]
		return x, ok, nil
	})
	swap(t, &slaOn, func(_ context.Context, p uuid.UUID) bool { return on && p == project })
	if s, d, err := SLAs.StateOf(context.Background(), task); s != None || d != nil || err != nil {
		t.Errorf("SLAs off: %q %v %v, want none", s, d, err)
	}
	on = true
	if s, d, err := SLAs.StateOf(context.Background(), uuid.New()); s != None || d != nil || err != nil {
		t.Errorf("no timer: %q %v %v, want none", s, d, err)
	}
	if s, d, err := SLAs.StateOf(context.Background(), task); s != OnTrack || d == nil || !d.Equal(due) || err != nil {
		t.Errorf("a running timer: %q %v %v, want on track, due %s", s, d, err, due)
	}
}
