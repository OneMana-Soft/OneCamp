//go:build integration

package integration_test

// linear-39 round 2: what a change replaced, read from the Work Change Log
// (models/postgres/TaskChange LatestValues and LatestFieldValue) against real
// Postgres 12 with every migration, 547's index included: each field's value
// in the newest earlier change of the same task that names it, a JSON null
// kept, a field no earlier change names left out, another task's changes and
// later ones never read, and a custom field by its id.
//
// Run: go test -tags=integration ./tests/integration/ -run TestWhatAChangeReplacedIsTheNewestEarlierValueOfTheSameTask -v

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	taskChangeModels "github.com/akashc777/OneCamp/models/postgres/TaskChange"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestWhatAChangeReplacedIsTheNewestEarlierValueOfTheSameTask(t *testing.T) {
	env := integration.ConnectServerPostgres(t)
	ctx := context.Background()
	task, other, project := uuid.NewString(), uuid.NewString(), uuid.NewString()
	add := func(resource, diff string) int64 {
		t.Helper()
		var seq int64
		if err := env.PG.QueryRow(`INSERT INTO task_changes (project_uuid, resource_uuid, kind, diff) VALUES ($1, $2, 'task.updated', $3::jsonb) RETURNING sequence`,
			project, resource, diff).Scan(&seq); err != nil {
			t.Fatal(err)
		}
		return seq
	}
	add(task, `{"task_name":"Draft","task_priority":"low","task_assignee":{"user_uuid":"u1"}}`)
	add(task, `{"field_id":"f-1","value":{"amount":3}}`)
	add(other, `{"task_priority":"urgent","task_label":"Ops"}`)
	add(task, `{"task_priority":"medium","task_assignee":null}`)
	add(task, `{"field_id":"f-1","value":{"amount":5}}`)
	add(task, `{"field_id":"f-2","value":"x"}`)
	now := add(task, `{"task_priority":"high","task_assignee":{"user_uuid":"u2"},"task_status":"done","task_label":"Launch"}`)
	add(task, `{"task_priority":"urgent","task_status":"todo"}`)

	values := func(raw map[string]json.RawMessage) map[string]any {
		out := map[string]any{}
		for k, v := range raw {
			var x any
			if err := json.Unmarshal(v, &x); err != nil {
				t.Fatalf("%s: %s: %v", k, v, err)
			}
			out[k] = x
		}
		return out
	}
	got, err := taskChangeModels.LatestValues(ctx, task, now, []string{"task_assignee", "task_label", "task_priority", "task_status"})
	if err != nil {
		t.Fatal(err)
	}
	// task_priority: the newest earlier change (medium), not the first (low), another task's (urgent) or a later one;
	// task_assignee: cleared then, so null; task_status and task_label: no earlier change of this task names them.
	if want := map[string]any{"task_priority": "medium", "task_assignee": nil}; !reflect.DeepEqual(values(got), want) {
		t.Errorf("replaced %v, want %v", values(got), want)
	}
	if got, err := taskChangeModels.LatestValues(ctx, task, now, nil); err != nil || len(got) != 0 {
		t.Errorf("no fields answered %v, %v; want nothing, unread", got, err)
	}

	v, found, err := taskChangeModels.LatestFieldValue(ctx, task, now, "f-1")
	if err != nil || !found || !reflect.DeepEqual(values(map[string]json.RawMessage{"v": v})["v"], map[string]any{"amount": float64(5)}) {
		t.Errorf("custom field f-1 was %s (found %v, err %v), want its newest earlier value {amount: 5}", v, found, err)
	}
	if _, found, err := taskChangeModels.LatestFieldValue(ctx, task, now, "f-9"); err != nil || found {
		t.Errorf("a custom field never set was found (%v, err %v)", found, err)
	}

	// The read walks the task's own changes newest first, on migration 547's index.
	var def string
	if err := env.PG.QueryRow(`SELECT indexdef FROM pg_indexes WHERE tablename = 'task_changes' AND indexname = 'idx_task_changes_task'`).Scan(&def); err != nil {
		t.Fatalf("idx_task_changes_task: %v", err)
	}
	if !strings.Contains(def, "(resource_uuid, sequence)") {
		t.Errorf("idx_task_changes_task is %q, want it on (resource_uuid, sequence)", def)
	}
}
