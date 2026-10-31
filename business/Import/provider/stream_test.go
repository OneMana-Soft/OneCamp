package provider

import (
	"context"
	"testing"
)

// drained reads a stream to its end: what came, and the error, if any.
func drained[T any](items <-chan T, errs <-chan error) (n int, err error) {
	for range items {
		n++
	}
	for e := range errs {
		if err == nil {
			err = e
		}
	}
	return n, err
}

func TestASourceWithNoTasksBringsNoneAndMapsNoStatus(t *testing.T) {
	var p NoTasks
	ctx := context.Background()
	counts := map[string]func() (int, error){
		"people":   func() (int, error) { return drained(p.IterUsers(ctx, nil, JobOptions{})) },
		"teams":    func() (int, error) { return drained(p.IterTeams(ctx, nil, JobOptions{})) },
		"projects": func() (int, error) { return drained(p.IterProjects(ctx, nil, JobOptions{})) },
		"tasks":    func() (int, error) { return drained(p.IterTasksOfProject(ctx, nil, JobOptions{}, "p1")) },
		"subtasks": func() (int, error) { return drained(p.IterSubtasksOfTask(ctx, nil, JobOptions{}, "t1")) },
		"comments": func() (int, error) { return drained(p.IterCommentsOfTask(ctx, nil, JobOptions{}, "t1")) },
	}
	for what, read := range counts {
		if n, err := read(); n != 0 || err != nil {
			t.Errorf("%s: %d came, err %v; want none and no error", what, n, err)
		}
	}
	if len(p.DefaultStatusMap()) != 0 || len(p.DefaultPriorityMap()) != 0 {
		t.Errorf("maps %v and %v; want both empty", p.DefaultStatusMap(), p.DefaultPriorityMap())
	}
}
