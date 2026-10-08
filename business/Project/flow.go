package business

// The flow of work: how many tasks stood where at the end of each week of a
// report (a cumulative flow diagram). To do (the backlog with it), in
// progress and in review are the open tasks then; done is what got done since
// the report began. A band that keeps widening is work piling up at that step;
// done rising steadily is work getting through. Jira keeps the chart for its
// paid plans; Asana, monday.com and Notion don't draw it.

import (
	"sort"
	"strings"
	"time"

	domain "github.com/akashc777/OneCamp/domain/Project"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// FlowWeek is where the report's tasks stood at the end of one week (or now,
// for this one).
type FlowWeek struct {
	ToDo       int `json:"to_do"`
	InProgress int `json:"in_progress"`
	InReview   int `json:"in_review"`
	Done       int `json:"done"`
}

// builtIn is every status a task can be in, as its history names them.
var builtIn = map[string]bool{
	dgraphStruct.TASK_STATUS_BACKLOG: true, dgraphStruct.TASK_STATUS_TODO: true, dgraphStruct.TASK_STATUS_INPROGRESS: true,
	dgraphStruct.TASK_STATUS_INREVIEW: true, dgraphStruct.TASK_STATUS_DONE: true, dgraphStruct.TASK_STATUS_CANCELED: true,
}

// statusSpan is a task being in a status (a built-in one, its category) from at on.
type statusSpan struct {
	at       time.Time
	category string
}

// statusSpans is a task's history as statuses from moments on, oldest first:
// what it was in when it was made, then each change. A status's history names
// a built-in status by its key and a project's own by its name as it was
// then (own: lower-cased name → category). A name no status has now reads as
// the status before it; the last change reads as the task's status now. A
// task with no history was to do until its status took hold (status since).
func statusSpans(t *dgraphStruct.DgraphTask, own map[string]string) []statusSpan {
	var changes []*dgraphStruct.DgraphTaskActivity
	for _, a := range t.Activity {
		if a != nil && a.Type == dgraphStruct.ACTIVITY_TYPE_STATUS && a.LogTime != nil {
			changes = append(changes, a)
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].LogTime.Before(*changes[j].LogTime) })
	categoryOf := func(state, otherwise string) string {
		if builtIn[state] {
			return state
		}
		if c, ok := own[strings.ToLower(strings.TrimSpace(state))]; ok && builtIn[c] {
			return c
		}
		return otherwise
	}
	var start time.Time
	if t.CreatedAt != nil {
		start = *t.CreatedAt
	}
	if len(changes) == 0 {
		// No history (an import sets statuses without one): to do until the
		// status took hold, when that's known.
		if t.StatusSince != nil && t.StatusSince.After(start) && t.Status != dgraphStruct.TASK_STATUS_TODO && t.Status != dgraphStruct.TASK_STATUS_BACKLOG {
			return []statusSpan{{at: start, category: dgraphStruct.TASK_STATUS_TODO}, {at: *t.StatusSince, category: t.Status}}
		}
		return []statusSpan{{at: start, category: t.Status}}
	}
	spans := []statusSpan{{at: start, category: categoryOf(changes[0].PrevState, dgraphStruct.TASK_STATUS_TODO)}}
	for i, a := range changes {
		otherwise := spans[len(spans)-1].category
		if i == len(changes)-1 {
			otherwise = t.Status
		}
		spans = append(spans, statusSpan{at: *a.LogTime, category: categoryOf(a.NextState, otherwise)})
	}
	return spans
}

// spanAt is the span the task was in at t (its status, and since when), and
// whether it existed then.
func spanAt(spans []statusSpan, t time.Time) (statusSpan, bool) {
	if len(spans) == 0 || t.Before(spans[0].at) {
		return statusSpan{}, false
	}
	at := spans[0]
	for _, s := range spans[1:] {
		if s.at.After(t) {
			break
		}
		at = s
	}
	return at, true
}

// buildFlow is where the tasks stood at the end of each of the weeks from
// first, the last ending now. projects are the report's (open tasks and those
// closed since first), own their statuses by project uuid, only the projects
// kept when not empty. Done counts a task only once it got done after first
// (one done before, and still done, isn't news; reopened and done again, it
// is); cancelled ones never count. Pure.
func buildFlow(projects []domain.ReportProject, own map[string]map[string]string, first, now time.Time, weeks int, only map[string]bool) []FlowWeek {
	flow := make([]FlowWeek, weeks)
	ends := make([]time.Time, weeks)
	for i := range ends {
		ends[i] = first.AddDate(0, 0, 7*(i+1))
		if ends[i].After(now) {
			ends[i] = now
		}
	}
	for _, p := range projects {
		if len(only) > 0 && !only[p.UUID] {
			continue
		}
		for _, list := range [][]*dgraphStruct.DgraphTask{p.Open, p.Closed} {
			for _, t := range list {
				if t == nil {
					continue
				}
				spans := statusSpans(t, own[p.UUID])
				for i, end := range ends {
					span, ok := spanAt(spans, end)
					if !ok {
						continue
					}
					switch span.category {
					case dgraphStruct.TASK_STATUS_DONE:
						if span.at.After(first) {
							flow[i].Done++
						}
					case dgraphStruct.TASK_STATUS_CANCELED:
					case dgraphStruct.TASK_STATUS_INPROGRESS:
						flow[i].InProgress++
					case dgraphStruct.TASK_STATUS_INREVIEW:
						flow[i].InReview++
					default:
						flow[i].ToDo++
					}
				}
			}
		}
	}
	return flow
}
