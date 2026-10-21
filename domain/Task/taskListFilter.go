package Domain

// A task list's filters as graph conditions (PR1, the BE audit's controller
// DQL fragments): the project's list and board and a person's list and board
// each built these in their controller, four copies. Every value is checked
// before it reaches the query: column names against the allow-list, values
// against the safe set, the search sanitised, dates parsed and re-formatted.

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// ErrSearchTerm is a search that can't be made safe (too long, or no usable
// characters but those refused): the person's to fix.
var ErrSearchTerm = errors.New("search string is too long or invalid")

// TaskListParam is one filter a list sends: a column and its value (one
// string, or a list of them).
type TaskListParam struct {
	ID    string `json:"id"`
	Value any    `json:"value"`
}

// TaskListFilter is a task list's filters as the person set them.
type TaskListFilter struct {
	// Search is the list's search box.
	Search string
	Params []TaskListParam
	// From and To, when both are set, keep tasks due, starting or running
	// inside the window (a calendar's range).
	From, To *time.Time
	// DueBy keeps open tasks with a due date up to it: overdue and due
	// today, for Home (PR1, the BE audit's due_by).
	DueBy *time.Time
	Now   time.Time
	// Status turns statuses (built-in and the project's own) into a
	// condition: business/TaskStatus.FilterClause.
	Status func(values []string) string
	// Side answers the filters whose values live in Postgres beside the
	// tasks (cycles, custom fields), or false.
	Side func(id string, values []string) (string, bool)
}

func nameMatches(term string) string {
	return fmt.Sprintf(`regexp(task_name, /.*%s.*/i)`, term)
}

// Clause is the filters as one graph condition, "" for none.
func (f TaskListFilter) Clause() (string, error) {
	var parts []string
	if strings.TrimSpace(f.Search) != "" {
		safe, err := dgraphquery.SanitizeSearchTerm(f.Search)
		switch {
		case err == nil:
			parts = append(parts, nameMatches(safe))
		case !errors.Is(err, dgraphquery.ErrEmpty):
			return "", ErrSearchTerm
		}
	}
	if f.From != nil && f.To != nil {
		s, t := f.From.Format(time.RFC3339Nano), f.To.Format(time.RFC3339Nano)
		parts = append(parts, fmt.Sprintf(`((ge(task_due_date, "%s") AND le(task_due_date, "%s")) OR (ge(task_start_date, "%s") AND le(task_start_date, "%s")) OR (le(task_start_date, "%s") AND ge(task_due_date, "%s")))`, s, t, s, t, t, s))
	}
	if f.DueBy != nil {
		parts = append(parts, dgraphStruct.TASK_OPEN_FILTER+fmt.Sprintf(` AND le(task_due_date, "%s") AND %s`, f.DueBy.Format(time.RFC3339Nano), dgraphquery.Deleted("task_due_date")))
	}
	now := f.Now.Format(time.RFC3339Nano)
	for _, p := range f.Params {
		if !dgraphquery.IsAllowedColumnName(p.ID) {
			continue
		}
		var clause string
		switch v := p.Value.(type) {
		case string:
			switch p.ID {
			case "task_name":
				if safe, err := dgraphquery.SanitizeSearchTerm(v); err == nil {
					clause = nameMatches(safe)
				}
			case "overdue":
				clause = dgraphStruct.TASK_OPEN_FILTER + fmt.Sprintf(` AND lt(task_due_date, "%s") AND %s`, now, dgraphquery.Deleted("task_due_date"))
			case "upcoming":
				clause = dgraphStruct.TASK_OPEN_FILTER + fmt.Sprintf(` AND gt(task_start_date, "%s")`, now)
			}
		case []any:
			values := make([]string, 0, len(v))
			for _, item := range v {
				if s, ok := item.(string); ok && dgraphquery.IsAllowedFilterValue(s) {
					values = append(values, s)
				}
			}
			switch p.ID {
			case "task_priority":
				clause = fmt.Sprintf(`anyofterms(task_priority,"%s")`, strings.Join(values, " "))
			case "task_status":
				if f.Status != nil {
					clause = f.Status(values)
				}
			case "task_assignee_name":
				clause = fmt.Sprintf(`uid_in(task_assignee, [%s])`, strings.Join(values, ", "))
			case "task_project_name":
				clause = fmt.Sprintf(`uid_in(task_project, [%s])`, strings.Join(values, ", "))
			}
			if f.Side != nil {
				if side, ok := f.Side(p.ID, values); ok {
					clause = side
				}
			}
		}
		if clause != "" {
			parts = append(parts, clause)
		}
	}
	return strings.Join(parts, " AND "), nil
}
