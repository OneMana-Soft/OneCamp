package collab

import "context"

// TaskChecks is what a setTaskChecks write did: how many to-dos in the doc
// stand for the tasks (Held; 0 when the doc no longer holds them), and how
// many of their boxes moved (Changed).
type TaskChecks struct {
	Changed int `json:"changed"`
	Held    int `json:"held"`
}

// SetTaskChecks ticks or clears every to-do in a doc that stands for one of
// the tasks in checks (task uuid -> done), in one transaction on the live doc
// (the collaboration service's setTaskChecks op, docs programme C1). A doc
// whose boxes already agree is left untouched.
// A variable, so a test can stand in for the collaboration service.
var SetTaskChecks = func(ctx context.Context, docUUID string, checks map[string]bool, o Options) (TaskChecks, error) {
	var a TaskChecks
	body := wire{TaskChecks: checks, By: &o.By}
	if err := call(ctx, docUUID, "setTaskChecks", body, &a); err != nil {
		return TaskChecks{}, err
	}
	return a, nil
}
