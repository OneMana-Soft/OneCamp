package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A search keeps a task it reached by the assignee grant only for the task's
// assignee now, read in one read, and only when some hit needs it.
func TestKeepAssignedChecksTheTaskAsItIsInOneRead(t *testing.T) {
	const (
		xan, yuri, olu = "aaaaaaaa-0000-0000-0000-00000000000a", "aaaaaaaa-0000-0000-0000-00000000000b", "aaaaaaaa-0000-0000-0000-00000000000c"
		launch, other  = "bbbbbbbb-0000-0000-0000-000000000001", "bbbbbbbb-0000-0000-0000-000000000002"
		kit, sketch    = "cccccccc-0000-0000-0000-000000000001", "cccccccc-0000-0000-0000-000000000002"
		inLaunch       = "cccccccc-0000-0000-0000-000000000003"
	)
	person := func(id string) *dgraphStruct.DgraphUser { return &dgraphStruct.DgraphUser{Uuid: id} }
	live := []*dgraphStruct.DgraphTask{
		// The kit went from Xan to Yuri; its entry may still name Xan.
		{Uuid: kit, Assignee: person(yuri), CreatedBy: person(olu), Project: &dgraphStruct.DgraphProject{Uuid: launch}},
		// Xan's own sketch, in no project.
		{Uuid: sketch, CreatedBy: person(xan)},
	}
	var reads [][]string
	var fail error
	saved := tasksAsTheyAre
	t.Cleanup(func() { tasksAsTheyAre = saved })
	tasksAsTheyAre = func(_ context.Context, ids []string, _ string) ([]*dgraphStruct.DgraphTask, error) {
		reads = append(reads, ids)
		if fail != nil {
			return nil, fail
		}
		return live, nil
	}
	hits := []SimilarResult{
		{ContentType: "task", ContentUUID: kit, TaskUUID: kit, ProjectUUID: launch},
		{ContentType: "comment", ContentUUID: "note", TaskUUID: kit, ProjectUUID: launch},
		{ContentType: "task", ContentUUID: sketch, TaskUUID: sketch},
		{ContentType: "task", ContentUUID: inLaunch, TaskUUID: inLaunch, ProjectUUID: other},
		{ContentType: "post", ContentUUID: "p1", ChannelUUID: "c1"},
	}
	ids := func(rs []SimilarResult) []string {
		var out []string
		for _, r := range rs {
			out = append(out, r.ContentUUID)
		}
		return out
	}
	keep := func(readers ...taskReader) []string {
		in := append([]SimilarResult(nil), hits...)
		return ids(keepAssigned(context.Background(), readers, in, similarTask))
	}

	for _, c := range []struct {
		name    string
		readers []taskReader
		want    []string
		fail    error
	}{
		{"one of the project's people: the hits in their projects stay, someone else's sketch in no project goes",
			[]taskReader{readerIn(olu, []string{launch, other})}, []string{kit, "note", inLaunch, "p1"}, nil},
		{"the old assignee, outside the project: the task and its comment go; his own sketch stays",
			[]taskReader{readerIn(xan, nil)}, []string{sketch, "p1"}, nil},
		{"the new assignee, outside the project: the task and its comment stay",
			[]taskReader{readerIn(yuri, nil)}, []string{kit, "note", "p1"}, nil},
		{"an agent's run for someone the task has gone from: both grants must hold",
			[]taskReader{readerIn(yuri, nil), readerIn(xan, nil)}, []string{"p1"}, nil},
		{"a read that fails keeps none of what it was for",
			[]taskReader{readerIn(yuri, []string{other})}, []string{inLaunch, "p1"}, errors.New("graph down")},
	} {
		t.Run(c.name, func(t *testing.T) {
			reads, fail = nil, c.fail
			got := keep(c.readers...)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("kept %v, want %v", got, c.want)
			}
			if len(reads) != 1 {
				t.Errorf("%d reads of the graph (%v), want one for every hit", len(reads), reads)
			}
		})
	}

	t.Run("only covered hits: no read at all", func(t *testing.T) {
		reads, fail = nil, nil
		in := []SimilarResult{hits[0], hits[1], hits[4]}
		got := ids(keepAssigned(context.Background(), []taskReader{readerIn(olu, []string{launch})}, in, similarTask))
		if !reflect.DeepEqual(got, []string{kit, "note", "p1"}) || len(reads) != 0 {
			t.Errorf("kept %v with %d reads; want everything and no read", got, len(reads))
		}
	})
}

// Hand-overs are one update-by-query for up to taskAssigneeBatch tasks: a
// bulk run of n tasks is ceil(n/taskAssigneeBatch) writes, not n.
func TestHandOversAreOneUpdateByQueryPerBatch(t *testing.T) {
	var bodies []map[string]any
	was := updateTaskEntries
	updateTaskEntries = func(_ context.Context, body []byte, _ string) (int, error) {
		var b map[string]any
		if err := json.Unmarshal(body, &b); err != nil {
			t.Fatalf("body: %v", err)
		}
		bodies = append(bodies, b)
		return 0, nil
	}
	t.Cleanup(func() { updateTaskEntries = was })

	to := map[string]string{"t1": "u1", "t2": "", "t3": "u2", " ": "u9"}
	if _, err := SetTaskAssignees(context.Background(), to); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 1 {
		t.Fatalf("3 hand-overs made %d update-by-queries, want 1", len(bodies))
	}
	params := bodies[0]["script"].(map[string]any)["params"].(map[string]any)["to"]
	if want := map[string]any{"t1": "u1", "t2": "", "t3": "u2"}; !reflect.DeepEqual(params, want) {
		t.Fatalf("params.to = %v, want %v", params, want)
	}
	filter := bodies[0]["query"].(map[string]any)["bool"].(map[string]any)["filter"].([]any)
	terms := filter[1].(map[string]any)["terms"].(map[string]any)["content_uuid"]
	if want := []any{"t1", "t2", "t3"}; !reflect.DeepEqual(terms, want) {
		t.Fatalf("the query asks for %v, want %v", terms, want)
	}

	bodies = nil
	many := make(map[string]string, 2*taskAssigneeBatch+1)
	for i := 0; i < 2*taskAssigneeBatch+1; i++ {
		many[fmt.Sprintf("t%04d", i)] = "u1"
	}
	if _, err := SetTaskAssignees(context.Background(), many); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 3 {
		t.Fatalf("%d hand-overs made %d update-by-queries, want 3", len(many), len(bodies))
	}

	bodies = nil
	if _, err := SetTaskAssignees(context.Background(), map[string]string{"": "u1"}); err != nil || len(bodies) != 0 {
		t.Fatalf("no task made %d writes (err %v), want none", len(bodies), err)
	}
}
