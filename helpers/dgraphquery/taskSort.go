package dgraphquery

import "encoding/json"

// taskSorts are the sorts a task list offers, by the key the app sends, each
// to the predicate the graph sorts by; nothing else a request names reaches
// the graph. The app offers Start, Due and Created, from the list's column
// headers (task_start_date, task_due_date, task_created_at) and its Sort menu,
// whose Created sent task_created_date, which saved views and shared links
// keep. No task has that predicate, and the graph refused the whole list
// ("Cannot sort by unknown attribute"). A key belongs here only if every task
// carries its predicate: TotalOrder puts task_uuid after it, and the graph
// refuses a second key when a page's edge falls among tasks that share the
// first while some lack it.
var taskSorts = map[string]string{
	"task_start_date":   "task_start_date",
	"task_due_date":     "task_due_date",
	"task_created_at":   "task_created_at",
	"task_created_date": "task_created_at",
}

// taskListDefaultOrder is a task list's order when it asks for none the list
// offers: newest first.
const taskListDefaultOrder = "orderdesc: task_created_at"

// TaskListOrder is the graph order for a task list's sorting parameter, the
// app's JSON list of {"id", "desc"}: the last sort in it the list offers (a
// list sorts by one), or newest first. A sort the list doesn't offer, or whose
// desc isn't true or false, is passed over; a parameter that isn't such a list
// is an error.
func TaskListOrder(sorting string) (string, error) {
	if sorting == "" {
		return taskListDefaultOrder, nil
	}
	var sorts []struct {
		ID   string `json:"id"`
		Desc any    `json:"desc"`
	}
	if err := json.Unmarshal([]byte(sorting), &sorts); err != nil {
		return "", err
	}
	order := taskListDefaultOrder
	for _, s := range sorts {
		predicate, ok := taskSorts[s.ID]
		if !ok {
			continue
		}
		switch s.Desc {
		case true:
			order = "orderdesc: " + predicate
		case false:
			order = "orderasc: " + predicate
		}
	}
	return order, nil
}
