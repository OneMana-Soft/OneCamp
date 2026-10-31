// Mapping helpers shared by every provider.
package provider

import (
	"strings"

	taskPriority "github.com/akashc777/OneCamp/business/TaskPriority"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// Valid OneCamp task statuses (mirrors models/dgraph/struct.go).
var validStatuses = map[string]struct{}{
	"todo":       {},
	"inProgress": {},
	"backlog":    {},
	"inReview":   {},
	"canceled":   {},
	"done":       {},
}

// ApplyStatusMap clamps a source status to the OneCamp vocabulary using
// (in order):
//  1. The operator-confirmed override (mapping)
//  2. The provider's default proposal (defaults)
//  3. Built-in heuristics (StatusFromHeuristic)
//  4. "todo" as the safe fallback
//
// All keys are case-insensitive.
func ApplyStatusMap(source string, mapping, defaults map[string]string) string {
	src := strings.TrimSpace(strings.ToLower(source))
	if src == "" {
		return "todo"
	}
	if v, ok := mapping[src]; ok {
		if _, valid := validStatuses[v]; valid {
			return v
		}
	}
	if v, ok := defaults[src]; ok {
		if _, valid := validStatuses[v]; valid {
			return v
		}
	}
	if v := StatusFromHeuristic(src); v != "" {
		return v
	}
	return "todo"
}

// StatusFromHeuristic maps common synonyms to OneCamp statuses. Used
// only as a last resort when no mapping is configured.
func StatusFromHeuristic(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "open", "todo", "to do", "to-do", "new", "ready":
		return "todo"
	case "in progress", "inprogress", "doing", "in-progress", "in development", "developing", "started", "wip":
		return "inProgress"
	case "backlog", "icebox", "later", "deferred":
		return "backlog"
	case "in review", "inreview", "in-review", "code review", "qa", "review", "testing":
		return "inReview"
	case "done", "closed", "resolved", "completed", "complete", "finished", "fixed", "won't fix", "wontfix":
		if s == "won't fix" || s == "wontfix" {
			return "canceled"
		}
		return "done"
	case "canceled", "cancelled", "won't do", "abandoned", "rejected", "duplicate":
		return "canceled"
	}
	return ""
}

// ApplyPriorityMap is a source priority as a task holds it (one scale:
// urgent, high, medium, low or none): the import's map, then the provider's,
// then what the words mean. A source with no priority gives none; words that
// name none of them give medium.
func ApplyPriorityMap(source string, mapping, defaults map[string]string) string {
	src := strings.TrimSpace(strings.ToLower(source))
	for _, m := range []map[string]string{mapping, defaults} {
		if v, ok := m[src]; ok && taskPriority.IsOrNone(v) {
			return v
		}
	}
	if v, ok := taskPriority.FromWords(src); ok {
		return v
	}
	return dgraphStruct.TASK_PRIORITY_MEDIUM
}

// StatusDefaults is defaults with each status that has no default of its own
// proposed by its class (kinds: name → class, both lower-cased), as Linear's
// "Ready to ship" is a started state. A name's own default wins. Pure.
func StatusDefaults(defaults, kinds map[string]string) map[string]string {
	out := make(map[string]string, len(defaults)+len(kinds))
	for k, v := range defaults {
		out[k] = v
	}
	for name, kind := range kinds {
		name, kind = strings.TrimSpace(strings.ToLower(name)), strings.TrimSpace(strings.ToLower(kind))
		if _, own := out[name]; own || name == "" {
			continue
		}
		if v, ok := defaults[kind]; ok {
			out[name] = v
		}
	}
	return out
}

// ProposeStatuses is the status the plan proposes for each of values, by the
// defaults StatusDefaults gives. Pure.
func ProposeStatuses(values []string, defaults map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for _, v := range values {
		key := strings.TrimSpace(strings.ToLower(v))
		out[key] = ApplyStatusMap(key, nil, defaults)
	}
	return out
}
