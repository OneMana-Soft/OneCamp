//go:build integration
// +build integration

package models_test

import (
	"testing"
	"time"

	restriction "github.com/akashc777/OneCamp/models/postgres/TaskRestriction"
	"github.com/google/uuid"
)

// Sub-tasks made together are restricted with the tasks above them in one
// statement, as Inherit restricts one (CR-167 F1): with the task above, or
// with the task it is restricted with; not below an open one. Tasks and the
// tasks above them must pair up.
func TestSubTasksMadeTogetherAreRestrictedWithTheTasksAbove(t *testing.T) {
	ctx, w := setup(t)
	at := time.Now().UTC().Truncate(time.Second)
	if err := restriction.Restrict(ctx, w.secret, w.project, w.ana, []uuid.UUID{w.ivy}, []uuid.UUID{w.below}, at); err != nil {
		t.Fatal(err)
	}
	subs := []uuid.UUID{uuid.New(), uuid.New(), uuid.New(), uuid.New()}
	for _, id := range subs {
		w.exec(t, `INSERT INTO tasks (id, project_id, created_by) VALUES ($1, $2, $3)`, id, w.project, w.maya)
	}
	if err := restriction.InheritAll(ctx, subs[:1], nil, w.maya, at); err == nil {
		t.Error("tasks with no tasks above them to pair with were taken")
	}
	if err := restriction.InheritAll(ctx, subs, []uuid.UUID{w.secret, w.secret, w.below, w.beside}, w.maya, at); err != nil {
		t.Fatal(err)
	}
	for i, id := range subs[:3] {
		r, err := restriction.Of(ctx, id)
		if err != nil || !r.Restricted || r.Via != w.secret || r.RestrictedBy != w.maya {
			t.Errorf("sub-task %d: %+v (%v), want restricted with the secret task by Maya", i, r, err)
		}
	}
	if !letIn(t, ctx, w.ivy, subs[0]) || letIn(t, ctx, w.hal, subs[0]) {
		t.Error("a sub-task lets in other people than the task above")
	}
	if r, err := restriction.Of(ctx, subs[3]); err != nil || r.Restricted {
		t.Errorf("below an open task: %+v (%v), want open", r, err)
	}
}
