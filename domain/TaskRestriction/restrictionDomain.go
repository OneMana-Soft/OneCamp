// Package domain (TaskRestriction) is the query text that says whom a
// restricted task's rows keep out (WA2): models/postgres/TaskRestriction
// reads by it which restricted tasks let a reader in, and a count that must
// leave out what a reader can't see (a project's triage, domain/Triage)
// leaves out by it, so the two never disagree.
package domain

import (
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
)

// Over is what KeptOut reads, joined onto a restriction r: its task t, and
// v, the task it is restricted with (none for one restricted on its own).
const Over = `JOIN tasks t ON t.id = r.task_id
		LEFT JOIN tasks v ON v.id = r.via_task_id`

// KeptOut is whether a restricted task's rows leave who out, over r (its
// row), t (the task) and v (the task it is restricted with): neither made by
// them, and no grant to them on it or on v.
func KeptOut(who string) string {
	return `t.created_by IS DISTINCT FROM ` + who + `
		AND (r.via_task_id IS NULL OR v.created_by IS DISTINCT FROM ` + who + `)
		AND NOT EXISTS (SELECT 1 FROM task_restriction_grants g
		                WHERE g.task_id = COALESCE(r.via_task_id, r.task_id) AND g.user_id = ` + who + `
		                AND ` + softDelete.Live("g") + `)`
}
