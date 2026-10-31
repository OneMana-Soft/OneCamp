// Package models (Approvals) keeps a project's status rules and its approval
// requests and decisions (migration 491). business/Approvals owns the rules.
// Every statement runs through models/pgexec: the caller's ctx, the pool's
// timeout, and one transaction where a decision locks its request.
package models

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// StatusRule is what moving a task into one of a project's statuses takes.
type StatusRule struct {
	ProjectID       uuid.UUID `json:"project_id"`
	Status          string    `json:"status"`
	Who             string    `json:"who"`
	People          []string  `json:"people"`
	Required        []string  `json:"required"`
	Approvers       []string  `json:"approvers"`
	ApprovalsNeeded int       `json:"approvals_needed"`
	// Client: the project's clients must approve too, through its links.
	Client    bool      `json:"client"`
	UpdatedBy uuid.UUID `json:"updated_by"`
	UpdatedAt time.Time `json:"updated_at"`
}

// WantsApproval is whether a move into the status waits for a yes: its
// approvers', the client's, or both.
func (r *StatusRule) WantsApproval() bool { return len(r.Approvers) > 0 || r.Client }

const ruleColumns = `project_id, status, who, people::text[], required, approvers::text[], approvals_needed, client, updated_by, updated_at`

func scanRule(s pgexec.Row) (*StatusRule, error) {
	var r StatusRule
	var people, required, approvers pq.StringArray
	if err := s.Scan(&r.ProjectID, &r.Status, &r.Who, &people, &required, &approvers, &r.ApprovalsNeeded, &r.Client, &r.UpdatedBy, &r.UpdatedAt); err != nil {
		return nil, err
	}
	r.People, r.Required, r.Approvers = helpers.NonNil(people), helpers.NonNil(required), helpers.NonNil(approvers)
	return &r, nil
}

// MaxRulesPerProject bounds a project's status rules.
const MaxRulesPerProject = 40

// ListRules is a project's status rules.
func ListRules(ctx context.Context, projectID uuid.UUID) ([]*StatusRule, error) {
	out, err := pgexec.Query(ctx, `SELECT `+ruleColumns+` FROM status_rules WHERE project_id = $1 ORDER BY status LIMIT 100`, scanRule, projectID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Approvals ListRules: %+v", err)
		return nil, err
	}
	if out == nil {
		out = []*StatusRule{}
	}
	return out, nil
}

// RulesFor is the rules of a project for any of the given statuses (a custom
// status's id and the built-in key it counts as).
func RulesFor(ctx context.Context, projectID uuid.UUID, statuses []string) ([]*StatusRule, error) {
	return pgexec.Query(ctx, `SELECT `+ruleColumns+` FROM status_rules WHERE project_id = $1 AND status = ANY($2)`, scanRule, projectID, pq.Array(statuses))
}

// CountRules is how many rules a project has.
func CountRules(ctx context.Context, projectID uuid.UUID) (int, error) {
	return pgexec.Scalar[int](ctx, `SELECT count(*) FROM status_rules WHERE project_id = $1`, projectID)
}

// PutRule saves a status's rule, replacing what it had.
func PutRule(ctx context.Context, r *StatusRule) error {
	_, err := pgexec.Exec(ctx,
		`INSERT INTO status_rules (project_id, status, who, people, required, approvers, approvals_needed, client, updated_by, updated_at)
		 VALUES ($1, $2, $3, $4::uuid[], $5, $6::uuid[], $7, $8, $9, NOW())
		 ON CONFLICT (project_id, status) DO UPDATE SET who = EXCLUDED.who, people = EXCLUDED.people, required = EXCLUDED.required,
		   approvers = EXCLUDED.approvers, approvals_needed = EXCLUDED.approvals_needed, client = EXCLUDED.client,
		   updated_by = EXCLUDED.updated_by, updated_at = NOW()`,
		r.ProjectID, r.Status, r.Who, pq.Array(r.People), pq.Array(r.Required), pq.Array(r.Approvers), r.ApprovalsNeeded, r.Client, r.UpdatedBy)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Approvals PutRule: %+v", err)
	}
	return err
}

// DeleteRule removes a status's rule.
func DeleteRule(ctx context.Context, projectID uuid.UUID, status string) error {
	_, err := pgexec.Exec(ctx, `DELETE FROM status_rules WHERE project_id = $1 AND status = $2`, projectID, status)
	return err
}

// Approval states.
const (
	Pending   = "pending"
	Approved  = "approved"
	Changes   = "changes"
	Rejected  = "rejected"
	Withdrawn = "withdrawn"
	// MovedOn: the task went on without it (moved by another path, or deleted).
	MovedOn = "moved_on"
)

// Verdicts.
const (
	VerdictApprove = "approve"
	VerdictChanges = "changes"
	VerdictReject  = "reject"
)

// What a request is about.
const (
	KindTask = "task"
	KindDoc  = "doc"
	KindFile = "file"
)

// Decision is one answer: a member's (UserID), or a client's through a link
// the request names (GrantID, with the name they gave). On a doc it keeps the
// body it decided on.
type Decision struct {
	UserID    uuid.UUID `json:"user_id"`
	GrantID   uuid.UUID `json:"-"`
	Client    bool      `json:"client,omitempty"`
	Name      string    `json:"name,omitempty"`
	Verdict   string    `json:"verdict"`
	Comment   string    `json:"comment"`
	BodyHash  string    `json:"-"`
	Version   *int      `json:"version,omitempty"`
	DecidedAt time.Time `json:"decided_at"`
}

// Approval is a request about a task, a doc or a file, and its answers: a
// task's move into a status (ToStatus), or the item itself approved. A task's
// request has its TaskID and ProjectID; a doc's has no project.
type Approval struct {
	ID           uuid.UUID  `json:"id"`
	ResourceKind string     `json:"resource_kind"`
	ResourceID   string     `json:"resource_id"`
	ProjectID    uuid.UUID  `json:"project_id"`
	TaskID       uuid.UUID  `json:"task_id"`
	ToStatus     string     `json:"to_status"`
	Ask          string     `json:"ask,omitempty"`
	DueAt        *time.Time `json:"due_at,omitempty"`
	RequestedBy  uuid.UUID  `json:"requested_by"`
	Approvers    []string   `json:"approvers"`
	Needed       int        `json:"needed"`
	// ClientGrants are the links whose client must approve too; members see
	// only whether there are any (Client).
	ClientGrants []string   `json:"-"`
	Client       bool       `json:"client"`
	State        string     `json:"state"`
	CreatedAt    time.Time  `json:"created_at"`
	DecidedAt    *time.Time `json:"decided_at,omitempty"`
	Decisions    []Decision `json:"decisions"`
}

// A request's columns; a task's written by an older API may lack its
// resource_id, which is its task_id.
const approvalColumns = `id, resource_kind, COALESCE(resource_id, task_id::text), COALESCE(project_id, '00000000-0000-0000-0000-000000000000'),
	COALESCE(task_id, '00000000-0000-0000-0000-000000000000'), COALESCE(to_status, ''), ask, due_at, requested_by, approvers::text[], needed,
	client_grants::text[], state, created_at, decided_at`

func scanApproval(s pgexec.Row) (*Approval, error) {
	var a Approval
	var approvers, grants pq.StringArray
	var decided, due sql.NullTime
	if err := s.Scan(&a.ID, &a.ResourceKind, &a.ResourceID, &a.ProjectID, &a.TaskID, &a.ToStatus, &a.Ask, &due, &a.RequestedBy, &approvers, &a.Needed,
		&grants, &a.State, &a.CreatedAt, &decided); err != nil {
		return nil, err
	}
	a.Approvers = helpers.NonNil(approvers)
	a.ClientGrants = helpers.NonNil(grants)
	a.Client = len(a.ClientGrants) > 0
	if decided.Valid {
		a.DecidedAt = &decided.Time
	}
	if due.Valid {
		a.DueAt = &due.Time
	}
	a.Decisions = []Decision{}
	return &a, nil
}

// ErrNotFound is an approval that isn't there.
var ErrNotFound = errors.New("approvals: not found")

// Request opens a request to move a task, or answers the one already open
// for it (created false).
func Request(ctx context.Context, a *Approval) (*Approval, bool, error) {
	a.ResourceKind, a.ResourceID = KindTask, a.TaskID.String()
	return Open(ctx, a)
}

// Open opens a request about any item, or answers the one already open for
// it (created false): one open request per item.
func Open(ctx context.Context, a *Approval) (*Approval, bool, error) {
	if a.ID == uuid.Nil {
		a.ID = uuid.New()
	}
	if a.ResourceKind == "" {
		a.ResourceKind, a.ResourceID = KindTask, a.TaskID.String()
	}
	n, err := pgexec.Exec(ctx,
		`INSERT INTO approvals (id, resource_kind, resource_id, project_id, task_id, to_status, ask, due_at, requested_by, approvers, needed, client_grants)
		 VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7, $8, $9, $10::uuid[], $11, $12::uuid[]) ON CONFLICT DO NOTHING`,
		a.ID, a.ResourceKind, a.ResourceID, nilUUID(a.ProjectID), nilUUID(a.TaskID), a.ToStatus, a.Ask, a.DueAt, a.RequestedBy,
		pq.Array(helpers.NonNil(a.Approvers)), a.Needed, pq.Array(helpers.NonNil(a.ClientGrants)))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Approvals Open: %+v", err)
		return nil, false, err
	}
	if n == 1 {
		got, err := Get(ctx, a.ID)
		return got, true, err
	}
	open, err := PendingForItem(ctx, a.ResourceKind, a.ResourceID)
	return open, false, err
}

// nilUUID is NULL for the zero id: a doc's request has no project or task.
func nilUUID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

func withDecisions(ctx context.Context, a *Approval) (*Approval, error) {
	ds, err := pgexec.Query(ctx, `SELECT COALESCE(user_id, '00000000-0000-0000-0000-000000000000'), COALESCE(grant_id, '00000000-0000-0000-0000-000000000000'),
		name, verdict, comment, body_hash, version, decided_at FROM approval_decisions WHERE approval_id = $1 ORDER BY decided_at`,
		func(r pgexec.Row) (Decision, error) {
			var d Decision
			var version sql.NullInt64
			err := r.Scan(&d.UserID, &d.GrantID, &d.Name, &d.Verdict, &d.Comment, &d.BodyHash, &version, &d.DecidedAt)
			d.Client = d.GrantID != uuid.Nil
			if version.Valid {
				v := int(version.Int64)
				d.Version = &v
			}
			return d, err
		}, a.ID)
	if err != nil {
		return nil, err
	}
	a.Decisions = append(a.Decisions, ds...)
	return a, nil
}

func one(ctx context.Context, q string, args ...any) (*Approval, error) {
	a, found, err := pgexec.QueryOne(ctx, q, scanApproval, args...)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrNotFound
	}
	return withDecisions(ctx, a)
}

// Get is one approval with its answers.
func Get(ctx context.Context, id uuid.UUID) (*Approval, error) {
	return one(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE id = $1`, id)
}

// PendingFor is a task's open request.
func PendingFor(ctx context.Context, taskID uuid.UUID) (*Approval, error) {
	return PendingForItem(ctx, KindTask, taskID.String())
}

// PendingForTasks is the open requests of the tasks given, in one read (a
// task has one open request at most).
func PendingForTasks(ctx context.Context, taskIDs []uuid.UUID) ([]*Approval, error) {
	if len(taskIDs) == 0 {
		return nil, nil
	}
	return pgexec.Query(ctx, `SELECT `+approvalColumns+` FROM approvals
		WHERE resource_kind = $1 AND task_id = ANY($2) AND state = 'pending'`, scanApproval, KindTask, pq.Array(taskIDs))
}

// LatestFor is a task's newest request, open or not.
func LatestFor(ctx context.Context, taskID uuid.UUID) (*Approval, error) {
	return LatestForItem(ctx, KindTask, taskID.String())
}

// PendingForItem is an item's open request.
func PendingForItem(ctx context.Context, kind, id string) (*Approval, error) {
	return one(ctx, `SELECT `+approvalColumns+` FROM approvals
		WHERE resource_kind = $1 AND COALESCE(resource_id, task_id::text) = $2 AND state = 'pending'`, kind, id)
}

// LatestForItem is an item's newest request, open or not.
func LatestForItem(ctx context.Context, kind, id string) (*Approval, error) {
	return one(ctx, `SELECT `+approvalColumns+` FROM approvals
		WHERE resource_kind = $1 AND COALESCE(resource_id, task_id::text) = $2 ORDER BY created_at DESC LIMIT 1`, kind, id)
}

// Decider is who answers: a member (User), or a client through one of the
// request's links (Grant, with the name they gave). On a doc, the body they
// decided on.
type Decider struct {
	User     uuid.UUID
	Grant    uuid.UUID
	Name     string
	BodyHash string
	Version  *int
}

// Decide records an approver's answer to an open request, in one
// transaction with the request locked, and answers the request as it now
// stands; ErrNotFound when it isn't open.
func Decide(ctx context.Context, id, user uuid.UUID, verdict, comment string) (*Approval, error) {
	return DecideAs(ctx, id, Decider{User: user}, verdict, comment)
}

// DecideAs records a member's or a link's answer; a link answers only a
// request that names it (ErrNotFound otherwise, as for a closed one).
func DecideAs(ctx context.Context, id uuid.UUID, by Decider, verdict, comment string) (*Approval, error) {
	err := pgexec.InTx(ctx, func(ctx context.Context) error {
		state, err := pgexec.Scalar[string](ctx, `SELECT state FROM approvals WHERE id = $1
			AND ($2::uuid IS NULL OR $2::uuid = ANY(client_grants)) FOR UPDATE`, id, nilUUID(by.Grant))
		if errors.Is(err, sql.ErrNoRows) || (err == nil && state != Pending) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if by.Grant != uuid.Nil {
			_, err = pgexec.Exec(ctx,
				`INSERT INTO approval_decisions (approval_id, grant_id, name, verdict, comment, body_hash, version) VALUES ($1, $2, $3, $4, $5, $6, $7)
				 ON CONFLICT (approval_id, grant_id) WHERE grant_id IS NOT NULL DO UPDATE SET name = EXCLUDED.name, verdict = EXCLUDED.verdict,
				 comment = EXCLUDED.comment, body_hash = EXCLUDED.body_hash, version = EXCLUDED.version, decided_at = NOW()`,
				id, by.Grant, by.Name, verdict, comment, by.BodyHash, by.Version)
			return err
		}
		_, err = pgexec.Exec(ctx,
			`INSERT INTO approval_decisions (approval_id, user_id, verdict, comment, body_hash, version) VALUES ($1, $2, $3, $4, $5, $6)
			 ON CONFLICT (approval_id, user_id) DO UPDATE SET verdict = EXCLUDED.verdict, comment = EXCLUDED.comment,
			 body_hash = EXCLUDED.body_hash, version = EXCLUDED.version, decided_at = NOW()`,
			id, by.User, verdict, comment, by.BodyHash, by.Version)
		return err
	})
	if err != nil {
		return nil, err
	}
	return Get(ctx, id)
}

// Waiting is the open requests that name user as an approver and that they
// haven't answered, newest first, at most limit; the reader checks each item.
func Waiting(ctx context.Context, user uuid.UUID, limit int) ([]*Approval, error) {
	return pgexec.Query(ctx, `SELECT `+approvalColumns+` FROM approvals a
		WHERE a.state = 'pending' AND $1 = ANY(a.approvers)
		  AND NOT EXISTS (SELECT 1 FROM approval_decisions d WHERE d.approval_id = a.id AND d.user_id = $1)
		ORDER BY a.created_at DESC LIMIT $2`, scanApproval, user, limit)
}

// ForLink is a project's open task requests that name a client link and that
// no client has answered yet: what the link's client is asked. A client's yes
// is the client's part done, and their changes close the request. A task has
// one open request at most, so this is never more than the project's tasks.
func ForLink(ctx context.Context, projectID, grant uuid.UUID) ([]*Approval, error) {
	return pgexec.Query(ctx, `SELECT `+approvalColumns+` FROM approvals a
		WHERE a.state = 'pending' AND a.project_id = $1 AND a.resource_kind = 'task' AND $2 = ANY(a.client_grants)
		  AND NOT EXISTS (SELECT 1 FROM approval_decisions d WHERE d.approval_id = a.id AND d.grant_id IS NOT NULL)`,
		scanApproval, projectID, grant)
}

// Close ends an open request in a state; false when it wasn't open (someone
// closed it first).
func Close(ctx context.Context, id uuid.UUID, state string) (bool, error) {
	n, err := pgexec.Exec(ctx, `UPDATE approvals SET state = $2, decided_at = NOW() WHERE id = $1 AND state = 'pending'`, id, state)
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// CloseOpenFor ends a task's open request, if it has one, in a state: a held
// move (to_status), and an ask about the task itself only when asks is set.
func CloseOpenFor(ctx context.Context, taskID uuid.UUID, state string, asks bool) error {
	return CloseOpenForTasks(ctx, []string{taskID.String()}, state, asks)
}

// CloseOpenForTasks is CloseOpenFor for many tasks (by id) in one statement:
// the tasks an archive run took.
func CloseOpenForTasks(ctx context.Context, taskIDs []string, state string, asks bool) error {
	_, err := pgexec.Exec(ctx, `UPDATE approvals SET state = $2, decided_at = NOW()
		WHERE resource_kind = 'task' AND task_id = ANY($1::uuid[]) AND state = 'pending' AND ($3::boolean OR to_status IS NOT NULL)`, pq.Array(taskIDs), state, asks)
	return err
}

// CloseOpenForProject ends the open requests of every task a project has, in
// one statement, in a state: the project was deleted, so its tasks went on
// without them, held moves and asks alike.
func CloseOpenForProject(ctx context.Context, projectID uuid.UUID, state string) error {
	_, err := pgexec.Exec(ctx, `UPDATE approvals SET state = $2, decided_at = NOW()
		WHERE resource_kind = 'task' AND state = 'pending' AND task_id IN (SELECT id FROM tasks WHERE project_id = $1)`, projectID, state)
	return err
}

// DueRequest is an open request whose reminder is due, with the approvers
// who haven't answered it.
type DueRequest struct {
	*Approval
	Unanswered []string
}

// unanswered is a request's approvers who haven't answered it, in SQL.
const unanswered = `ARRAY(SELECT x::text FROM unnest(approvals.approvers) x
	WHERE NOT EXISTS (SELECT 1 FROM approval_decisions d WHERE d.approval_id = approvals.id AND d.user_id = x))`

func scanDue(s pgexec.Row) (*DueRequest, error) {
	var a Approval
	var approvers, grants, waiting pq.StringArray
	var decided, due sql.NullTime
	if err := s.Scan(&a.ID, &a.ResourceKind, &a.ResourceID, &a.ProjectID, &a.TaskID, &a.ToStatus, &a.Ask, &due, &a.RequestedBy, &approvers, &a.Needed,
		&grants, &a.State, &a.CreatedAt, &decided, &waiting); err != nil {
		return nil, err
	}
	a.Approvers, a.ClientGrants, a.Client = helpers.NonNil(approvers), helpers.NonNil(grants), len(grants) > 0
	if due.Valid {
		a.DueAt = &due.Time
	}
	a.Decisions = []Decision{}
	return &DueRequest{Approval: &a, Unanswered: helpers.NonNil(waiting)}, nil
}

// ClaimDue claims, each once, the open requests whose reminder is due at
// now: those due within a day and not yet reminded (soon), and those past
// due and not yet told so (late), at most limit of each. One statement per
// stage marks reminded_at, so a worker that loses the race gets nothing. A
// request asked less than a day before its due time has no "soon" stage:
// its ask was the notice.
func ClaimDue(ctx context.Context, now time.Time, limit int) (soon, late []*DueRequest, err error) {
	claim := func(cond string) ([]*DueRequest, error) {
		return pgexec.Query(ctx, `UPDATE approvals SET reminded_at = $1
			WHERE id IN (SELECT id FROM approvals WHERE state = 'pending' AND due_at IS NOT NULL AND `+cond+`
				ORDER BY due_at LIMIT $2 FOR UPDATE SKIP LOCKED)
			RETURNING `+approvalColumns+`, `+unanswered, scanDue, now, limit)
	}
	if soon, err = claim(`reminded_at IS NULL AND due_at > $1 AND due_at <= $1 + interval '24 hours'
		AND created_at <= due_at - interval '24 hours'`); err != nil {
		return nil, nil, err
	}
	late, err = claim(`due_at <= $1 AND (reminded_at IS NULL OR reminded_at < due_at)`)
	return soon, late, err
}
