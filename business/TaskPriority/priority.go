package taskpriority

// Package taskpriority is one priority scale for every way a task is made or
// changed, and pure, as business/TaskRank is: urgent, high,
// medium, low, or none (""), as Linear, Jira and Asana have it. Each place
// that took a priority kept its own list of three, so "No priority" was
// refused and an importer's or the AI's "urgent" became "high".

import (
	"slices"
	"strings"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// Is is a priority a task can be set to.
func Is(p string) bool { return slices.Contains(dgraphStruct.VALID_TASK_PRIORITIES, p) }

// IsOrNone is a priority, or none, which takes a task's priority
// off.
func IsOrNone(p string) bool { return p == "" || Is(p) }

// Rank sorts priorities from urgent (0) to none, after every
// priority.
func Rank(p string) int {
	if i := slices.Index(dgraphStruct.VALID_TASK_PRIORITIES, p); i >= 0 {
		return i
	}
	return len(dgraphStruct.VALID_TASK_PRIORITIES)
}

// FromWords is the priority someone or something else means by
// words (an importer's "Highest", a model's "P1"), and whether the words
// name one at all.
func FromWords(words string) (string, bool) {
	switch strings.ToLower(strings.Join(strings.Fields(words), " ")) {
	case "urgent", "critical", "highest", "blocker", "p0", "asap":
		return dgraphStruct.TASK_PRIORITY_URGENT, true
	case "high", "very high", "major", "p1", "important":
		return dgraphStruct.TASK_PRIORITY_HIGH, true
	case "medium", "normal", "moderate", "p2", "p3":
		return dgraphStruct.TASK_PRIORITY_MEDIUM, true
	case "low", "lowest", "very low", "minor", "trivial", "p4", "p5", "best effort":
		return dgraphStruct.TASK_PRIORITY_LOW, true
	case "", "none", "no priority", "no", "unset":
		return "", true
	}
	return "", false
}
