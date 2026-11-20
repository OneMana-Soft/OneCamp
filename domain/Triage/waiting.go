package domain

// MaxWaiting bounds AllWaiting's read: more tasks waiting than this is an
// error, and the lists' filter fails closed on it rather than read without
// bound.
const MaxWaiting = 50_000

// AllWaiting is every task waiting in any project's triage, with its project
// (triage_items holds one row per task), at most $1 rows: the lists' filter
// (taskread Waiting). models/postgres/Triage reads MaxWaiting+1 to tell a
// queue at the bound from one past it.
const AllWaiting = `SELECT task_uuid, project_uuid FROM triage_items WHERE decided_at IS NULL LIMIT $1`
