// Package domain (Cycle) is the query text that says whether a project runs
// its cycles on a schedule: models/postgres/Cycle reads it before it lets a
// project's cycles run side by side (SetParallel), and a task's status write
// returns it (domain/Task TouchForStatus) so a task that starts in a project
// with no schedule reads no schedule to join a cycle (business/Cycle
// startedJoinsCycle). One text, so the two never disagree.
package domain

// Scheduled is whether the project the parameter project names (a
// placeholder such as "$1") has a cycle schedule, as a boolean SQL
// expression.
func Scheduled(project string) string {
	return `EXISTS (SELECT 1 FROM cycle_schedules WHERE project_uuid = ` + project + `)`
}
