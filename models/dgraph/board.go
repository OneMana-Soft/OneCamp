package models

import (
	"encoding/json"
	"fmt"
	"strings"
)

// BoardOpenLimit is how many cards of each open column a board sends, in
// board order, until it asks for more with openLimit. It is what a column
// sent before columns were counted, when Dgraph cut it at 1,000 without
// saying so, so a web that never asks sees no change.
const BoardOpenLimit = 1000

// BoardOpenMax is the most a board may ask for in an open column.
const BoardOpenMax = 5000

// BoardColumn is one of a board's six columns, by built-in status.
type BoardColumn struct {
	Status string // the task_status of its tasks, and its key in a board's column counts
	Key    string // its name in a board answer: project_tasks_<Key>, user_tasks_<Key>
	Closed bool   // done and cancelled: the newest closedLimit, not the first openLimit
}

// BoardColumns are a board's columns, in the order a board answer has them.
var BoardColumns = []BoardColumn{
	{Status: TASK_STATUS_TODO, Key: "todo"},
	{Status: TASK_STATUS_INPROGRESS, Key: "in_progress"},
	{Status: TASK_STATUS_BACKLOG, Key: "backlog"},
	{Status: TASK_STATUS_INREVIEW, Key: "in_review"},
	{Status: TASK_STATUS_CANCELED, Key: "canceled", Closed: true},
	{Status: TASK_STATUS_DONE, Key: "done", Closed: true},
}

// BoardPlaces is a board column without its cards: where each card sits, and
// how many tasks the column has.
type BoardPlaces struct {
	// Places are the column's tasks with only what orders them and says which
	// of the board's columns they show in (uid, uuid, rank, creation time,
	// custom status): all of an open column, the newest closedLimit of a
	// closed one.
	Places []*DgraphTask
	// Count is every task in the column, and Custom how many of them are in
	// each custom status, by id; the rest are in the column's own status.
	Count  int
	Custom map[string]int
}

const boardPlaceFields = "uid task_uuid task_rank task_created_at task_custom_status"

// BoardPlacesQuery is the DQL, inside a project's or a person's block, for a
// board's columns without their cards; ParseBoardPlaces reads its answer.
// edge leads to the board's tasks (project_tasks, user_tasks) and filter is
// what keeps a task on the board: alive, and the board's own filters.
//
// An open column is read whole and unsorted: Dgraph cuts a sorted block at
// 1,000 without saying so (dgraphquery.SortedCap), and a board's order is
// business/TaskRank's, worked out from the places. A closed column is its
// newest closedLimit, or whole when closedLimit is 0.
func BoardPlacesQuery(edge, filter string, closedLimit int) string {
	var b strings.Builder
	for _, c := range BoardColumns {
		where := fmt.Sprintf("%s AND eq(task_status, %q)", filter, c.Status)
		window := ""
		if c.Closed && closedLimit > 0 {
			window = fmt.Sprintf(" (orderdesc: task_created_at, first: %d)", min(closedLimit, BoardClosedMax))
		}
		fmt.Fprintf(&b, "\n%[1]s: %[2]s @filter(%[3]s)%[4]s { %[5]s }"+
			"\n%[1]s_count: count(%[2]s @filter(%[3]s))"+
			"\n%[1]s_custom: %[2]s @filter(%[3]s) @groupby(task_custom_status) { count(uid) }",
			"places_"+c.Key, edge, where, window, boardPlaceFields)
	}
	return b.String()
}

// ParseBoardPlaces reads the columns BoardPlacesQuery asked for out of the
// block that asked, by column key (BoardColumn.Key).
func ParseBoardPlaces(block json.RawMessage) (map[string]*BoardPlaces, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(block, &fields); err != nil {
		return nil, err
	}
	out := make(map[string]*BoardPlaces, len(BoardColumns))
	for _, c := range BoardColumns {
		name := "places_" + c.Key
		col := &BoardPlaces{Custom: map[string]int{}}
		var groups []struct {
			GroupBy []struct {
				ID    string `json:"task_custom_status"`
				Count int    `json:"count"`
			} `json:"@groupby"`
		}
		for key, into := range map[string]any{name: &col.Places, name + "_count": &col.Count, name + "_custom": &groups} {
			if raw, ok := fields[key]; ok {
				if err := json.Unmarshal(raw, into); err != nil {
					return nil, fmt.Errorf("%s: %w", key, err)
				}
			}
		}
		for _, g := range groups {
			for _, s := range g.GroupBy {
				if s.ID != "" {
					col.Custom[s.ID] += s.Count
				}
			}
		}
		out[c.Key] = col
	}
	return out, nil
}

// ColumnCounts is how many tasks each column a board draws holds, by the
// column's key: a built-in status, or the id of a custom status the board
// draws a column for (known). A built-in column holds its tasks in no custom
// status, and those in a custom status the board doesn't know, as the web's
// columnsByStatus puts them. Every built-in status and every known custom
// status has a count, 0 included.
func ColumnCounts(columns map[string]*BoardPlaces, known []string) map[string]int {
	counts := make(map[string]int, len(BoardColumns)+len(known))
	for _, id := range known {
		counts[id] = 0
	}
	for _, c := range BoardColumns {
		counts[c.Status] = 0
		col := columns[c.Key]
		if col == nil {
			continue
		}
		own := col.Count
		for _, id := range known {
			own -= col.Custom[id]
			counts[id] += col.Custom[id]
		}
		counts[c.Status] = own
	}
	return counts
}

// Column is the cards of one of the board's columns in a project's board
// answer, by BoardColumn.Key; nil for any other key.
func (p *DgraphProject) Column(key string) *[]*DgraphTask {
	switch key {
	case "todo":
		return &p.TasksTodo
	case "in_progress":
		return &p.TasksInProgresss
	case "backlog":
		return &p.TasksBacklog
	case "in_review":
		return &p.TasksInReview
	case "canceled":
		return &p.TasksCanceled
	case "done":
		return &p.TasksDone
	}
	return nil
}

// Column is the cards of one of the board's columns in a person's board
// answer (My Tasks), by BoardColumn.Key; nil for any other key.
func (u *DgraphUser) Column(key string) *[]*DgraphTask {
	switch key {
	case "todo":
		return &u.TasksTodo
	case "in_progress":
		return &u.TasksInProgress
	case "backlog":
		return &u.TasksBacklog
	case "in_review":
		return &u.TasksInReview
	case "canceled":
		return &u.TasksCanceled
	case "done":
		return &u.TasksDone
	}
	return nil
}
