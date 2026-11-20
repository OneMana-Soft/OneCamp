// Package domain (Triage) is the query text of a project's triage:
// business/Triage owns its rules, and models/postgres/Triage runs the text
// through models/pgexec.
package domain

import (
	restrictionDomain "github.com/akashc777/OneCamp/domain/TaskRestriction"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
)

// waitingIn is what waits in a project's ($1) triage: its items no one has
// decided, snoozed or not. The one predicate the queue's list, the cap's
// count and a project read's count share, so no count drifts from the queue
// it stands for.
const waitingIn = ` FROM triage_items WHERE project_uuid = $1 AND decided_at IS NULL`

// WaitingList is what waits, oldest first, at most $2: the queue's read
// (GET /project/{project_uuid}/triage).
const WaitingList = `SELECT task_uuid, project_uuid, source, requester_uuid, requester_name, requester_email, arrived_at, snoozed_until` +
	waitingIn + ` ORDER BY arrived_at, task_uuid LIMIT $2`

// PutAll keeps a batch of items ($1 tasks, $2 projects, $3 sources, $4
// requesters, "" for none, $5 names, $6 emails, one per item) in one statement; a task
// already kept is left as it is.
const PutAll = `INSERT INTO triage_items (task_uuid, project_uuid, source, requester_uuid, requester_name, requester_email)
	SELECT t, p, s, NULLIF(r, '')::uuid, n, e FROM unnest($1::uuid[], $2::uuid[], $3::text[], $4::text[], $5::text[], $6::text[]) AS a(t, p, s, r, n, e)
	ON CONFLICT (task_uuid) DO NOTHING`

// CountWaiting is how many wait, snoozed or not: what the cap counts.
const CountWaiting = `SELECT count(*)` + waitingIn

// CountAwake is how many wait that no snooze holds back now: the queue's
// list less what's snoozed, which a project's read shows as waiting
// (project_triage).
const CountAwake = CountWaiting + ` AND (snoozed_until IS NULL OR snoozed_until <= NOW())`

// CountAwakeFor is CountAwake for a reader ($2, a user's id) the rule doesn't
// let into every restricted task of the project (anyone but its admins), as
// their queue lists it: a waiting task whose live restriction's rows keep
// them out (the restrictions' own KeptOut: not made by them, and no grant to
// them, on it or the task it is restricted with) isn't counted. Being its
// assignee is a graph fact no Postgres row holds, so a restricted task they
// reach only that way is left out too: under-counted by one, never revealed.
// One statement: the project's waiting items (triage_items_waiting_idx, at
// most MaxWaiting), each looked up by primary key in task_restrictions,
// tasks and task_restriction_grants.
var CountAwakeFor = CountAwake + ` AND NOT EXISTS (SELECT 1 FROM task_restrictions r ` + restrictionDomain.Over +
	` WHERE r.task_id = triage_items.task_uuid AND ` + softDelete.Live("r") + ` AND ` + restrictionDomain.KeptOut("$2::uuid") + `)`
