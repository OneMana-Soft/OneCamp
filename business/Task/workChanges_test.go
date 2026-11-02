package business

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	workFeed "github.com/akashc777/OneCamp/business/WorkFeed"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	"github.com/akashc777/OneCamp/models/taskread"
)

// Who may see which change: a task's change in a project they may view,
// unless the task is kept from them now; a change made while its task was
// restricted only if they are let into it now; anything but a task, nobody.
// Each project is asked about once, in any case.
func TestChangeSightShowsOnlyWhatTheReaderMaySeeNow(t *testing.T) {
	const (
		open     = "1a2b3c4d-0000-4000-8000-000000000001"
		kept     = "1a2b3c4d-0000-4000-8000-000000000002"
		keptToMe = "1a2b3c4d-0000-4000-8000-000000000003"
		P        = "2b3c4d5e-0000-4000-8000-00000000000a"
		Q        = "2b3c4d5e-0000-4000-8000-00000000000b"
	)
	sv, sr := viewableProjects, readerFilter
	t.Cleanup(func() { viewableProjects, readerFilter = sv, sr })
	var asked []string
	viewableProjects = func(_ context.Context, user, uid string, ids []string) (map[string]bool, error) {
		if user != "u1" || uid != "0x1" {
			t.Errorf("asked for %q %q", user, uid)
		}
		asked = append(asked, ids...)
		return map[string]bool{P: true}, nil
	}
	readerFilter = func(context.Context, string) (taskread.Filter, error) {
		return taskread.Of(map[string]bool{kept: true, keptToMe: false}), nil
	}
	row := func(seq int64, kind, task, project string, restricted bool) workFeed.Row {
		return workFeed.Row{Sequence: seq, Change: workFeed.Change{ResourceKind: kind, ResourceUUID: task, ProjectUUID: project, Restricted: restricted}}
	}
	rows := []workFeed.Row{
		row(1, "task", open, P, false),
		row(2, "task", kept, P, false),
		row(3, "task", keptToMe, P, true),
		row(4, "task", open, P, true), // restricted then, open now: only those let in now
		row(5, "task", open, Q, false),
		row(6, "cycle", open, P, false),
		row(7, "task", open, strings.ToUpper(P), false),
	}
	who := accessBusiness.Subject{UserUUID: "u1", UserUID: "0x1"}
	got, err := changeSight(context.Background(), who, rows)
	if err != nil {
		t.Fatal(err)
	}
	var seen []int64
	for seq, ok := range got {
		if ok {
			seen = append(seen, seq)
		}
	}
	sort.Slice(seen, func(i, j int) bool { return seen[i] < seen[j] })
	if fmt.Sprint(seen) != "[1 3 7]" {
		t.Errorf("seen %v, want [1 3 7]", seen)
	}
	sort.Strings(asked)
	if fmt.Sprint(asked) != fmt.Sprint([]string{P, Q}) {
		t.Errorf("asked about %v, want P and Q once each", asked)
	}

	if got, err := changeSight(context.Background(), accessBusiness.Subject{}, rows); err != nil || len(got) != 0 {
		t.Errorf("no one: %v %v, want nothing", got, err)
	}
	gone := errors.New("the graph is gone")
	viewableProjects = func(context.Context, string, string, []string) (map[string]bool, error) { return nil, gone }
	if _, err := changeSight(context.Background(), who, rows); !errors.Is(err, gone) {
		t.Errorf("err %v, want the read's", err)
	}
	viewableProjects = func(context.Context, string, string, []string) (map[string]bool, error) {
		return map[string]bool{P: true}, nil
	}
	readerFilter = func(context.Context, string) (taskread.Filter, error) { return taskread.Filter{}, gone }
	if _, err := changeSight(context.Background(), who, rows); !errors.Is(err, gone) {
		t.Errorf("err %v, want the filter's", err)
	}
}

// A reorder logs each rank it wrote once, after the write; a write that
// fails logs nothing.
func TestReorderLogsEachRankItWritesOnce(t *testing.T) {
	old, rec := reorderSeams, recordTasks
	t.Cleanup(func() { reorderSeams, recordTasks = old, rec })
	var logged []workFeed.TaskChange
	recordTasks = func(_ context.Context, _ string, cs ...workFeed.TaskChange) []workFeed.Entry {
		logged = append(logged, cs...)
		return nil
	}
	reorderSeams.neighbours = func(context.Context, []string) ([]*dgraphStruct.DgraphTask, error) {
		return []*dgraphStruct.DgraphTask{ranked("a", 0), ranked("b", 1024)}, nil
	}
	reorderSeams.write = func(context.Context, []*dgraphStruct.DgraphTask) error { return nil }
	reorderSeams.publish = func(context.Context, *mqttStruct.MqttTaskChanged) {}
	moved := []*dgraphStruct.DgraphTask{{Uuid: "m1"}, {Uuid: "m2"}}
	if err := Reorder(context.Background(), "p", moved, "a", "b"); err != nil {
		t.Fatal(err)
	}
	if len(logged) != 2 {
		t.Fatalf("logged %d, want one per written rank", len(logged))
	}
	for i, c := range logged {
		if c.Kind != workFeed.KindUpdated || c.Project != "p" || c.Task == nil || c.Task.Uuid != moved[i].Uuid ||
			fmt.Sprint(c.Fields) != "[task_rank]" {
			t.Errorf("entry %d: %+v", i, c)
		}
	}
	logged = nil
	reorderSeams.write = func(context.Context, []*dgraphStruct.DgraphTask) error { return errors.New("no") }
	if err := Reorder(context.Background(), "p", moved, "a", "b"); err == nil || len(logged) != 0 {
		t.Errorf("a failed write: err %v, logged %d", err, len(logged))
	}
}
