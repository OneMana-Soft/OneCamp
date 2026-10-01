// Mapping helpers shared by every provider.
package provider

import "strings"

// Valid OneCamp task statuses (mirrors models/dgraph/struct.go).
var validStatuses = map[string]struct{}{
	"todo":       {},
	"inProgress": {},
	"backlog":    {},
	"inReview":   {},
	"canceled":   {},
	"done":       {},
}

// Valid OneCamp task priorities.
var validPriorities = map[string]struct{}{
	"low":    {},
	"medium": {},
	"high":   {},
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

// ApplyPriorityMap clamps a source priority to {low|medium|high}.
func ApplyPriorityMap(source string, mapping, defaults map[string]string) string {
	src := strings.TrimSpace(strings.ToLower(source))
	if src == "" {
		return "medium"
	}
	if v, ok := mapping[src]; ok {
		if _, valid := validPriorities[v]; valid {
			return v
		}
	}
	if v, ok := defaults[src]; ok {
		if _, valid := validPriorities[v]; valid {
			return v
		}
	}
	if v := PriorityFromHeuristic(src); v != "" {
		return v
	}
	return "medium"
}

// PriorityFromHeuristic covers Jira's Highest/High/…, Asana's High/Medium/Low,
// Trello's stars-as-priority, etc.
func PriorityFromHeuristic(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "highest", "very high", "p0", "p1", "urgent", "critical", "high":
		return "high"
	case "lowest", "very low", "p4", "p5", "low":
		return "low"
	case "medium", "normal", "moderate", "p2", "p3":
		return "medium"
	}
	return ""
}
