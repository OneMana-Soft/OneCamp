// Package models (Task) is a task's Postgres row: its project, its Google
// Calendar event and its GitHub link. Statements run through models/pgexec,
// with the caller's ctx (the BE audit's style-B executors, PR3); the ones
// domain/Task writes come in as text.
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

type Task struct {
	Id                   uuid.UUID `gorm:"type:uuid;default:uuid_generate_v4()"`
	ProjectId            uuid.UUID
	TaskAssignee         uuid.UUID
	CreatedBy            uuid.UUID `json:"created_by"`
	CreatedAt            time.Time
	UpdatedAt            time.Time
	DeletedAt            time.Time
	TaskGoogleCalendarId *string
	GitHubIssueNumber    *int
	GitHubIssueURL       *string
	GitHubPRNumber       *int
	GitHubPRURL          *string
	GitHubBranch         *string
	GitHubSyncStatus     *string
	GitHubSyncError      *string
	GitHubSyncAttempts   *int
}

// exec runs a write and says its failure in the log as where.
func exec(ctx context.Context, where, query string, args ...any) error {
	if _, err := pgexec.Exec(ctx, query, args...); err != nil {
		helpers.LogErrorWithContext(ctx, "models/%s Failed err: %+v", where, err)
		return err
	}
	return nil
}

// scalarOrZero is query's one value, the zero value when it returns no row.
func scalarOrZero[T any](ctx context.Context, query string, args ...any) (T, error) {
	v, err := pgexec.Scalar[T](ctx, query, args...)
	if errors.Is(err, sql.ErrNoRows) {
		var zero T
		return zero, nil
	}
	return v, err
}

// scanTask reads query's task row: id, created_by, project_id, task_assignee,
// created_at, updated_at, deleted_at, task_google_calendar_id.
func scanTask(r pgexec.Row) (*Task, error) {
	var t Task
	var createdAt, updatedAt, deletedAt sql.NullTime
	var calendarID sql.NullString
	if err := r.Scan(&t.Id, &t.CreatedBy, &t.ProjectId, &t.TaskAssignee, &createdAt, &updatedAt, &deletedAt, &calendarID); err != nil {
		return nil, err
	}
	t.CreatedAt, t.UpdatedAt, t.DeletedAt = createdAt.Time, updatedAt.Time, deletedAt.Time
	if calendarID.Valid {
		t.TaskGoogleCalendarId = &calendarID.String
	}
	return &t, nil
}

// GetTaskByUUID is the task query reads by id; an empty Task when there is
// none (callers check its Id).
func GetTaskByUUID(ctx context.Context, query string, id uuid.UUID) (*Task, error) {
	t, found, err := pgexec.QueryOne(ctx, query, scanTask, id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetTaskByUUID Failed to get task err: %+v", err)
		return nil, err
	}
	if !found {
		return &Task{}, nil
	}
	return t, nil
}

func UpdateTaskNameByTaskUUID(ctx context.Context, query string, currentTime time.Time, taskUUID uuid.UUID) error {
	return exec(ctx, "UpdateTaskNameByTaskUUID", query, currentTime, taskUUID)
}

func UpdateTaskDeletedTimeByUUID(ctx context.Context, query string, taskUUID uuid.UUID, deleteTime *time.Time, updateTime *time.Time) error {
	return exec(ctx, "UpdateTaskDeletedTimeByUUID", query, deleteTime, updateTime, taskUUID)
}

func UpdateTaskDeletedTimeToNullByUUID(ctx context.Context, query string, updateTime *time.Time, taskUUID uuid.UUID) error {
	return exec(ctx, "UpdateTaskDeletedTimeToNullByUUID", query, updateTime, taskUUID)
}

func UpdateTaskGoogleCalendarId(ctx context.Context, query string, googleEventId *string, updatedAt time.Time, taskUUID uuid.UUID) error {
	return exec(ctx, "UpdateTaskGoogleCalendarId", query, googleEventId, updatedAt, taskUUID)
}

// GetSyncedTaskGCalIds is the Google Calendar events of the live tasks
// assigned to a person.
func GetSyncedTaskGCalIds(ctx context.Context, userUID uuid.UUID) ([]string, error) {
	ids, err := pgexec.Query(ctx, `
		SELECT task_google_calendar_id
		FROM tasks
		WHERE task_assignee = $1 AND task_google_calendar_id IS NOT NULL AND task_google_calendar_id != '' AND deleted_at IS NULL
	`, func(r pgexec.Row) (string, error) {
		var s string
		err := r.Scan(&s)
		return s, err
	}, userUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetSyncedTaskGCalIds Failed err: %+v", err)
		return nil, err
	}
	return ids, nil
}

// SetGitHubPRFieldsOnTask sets PR fields on a specific task by UUID.
func SetGitHubPRFieldsOnTask(ctx context.Context, query string, taskUUID uuid.UUID, prNumber int, prURL string, branch string) error {
	return exec(ctx, "SetGitHubPRFieldsOnTask", query, prNumber, prURL, branch, taskUUID)
}

// FindTaskUUIDByPRURL finds a task by its GitHub PR URL ("" for none).
func FindTaskUUIDByPRURL(ctx context.Context, query string, prURL string) (string, error) {
	return scalarOrZero[string](ctx, query, prURL)
}

// SetGitHubIssueFieldsOnTask sets GitHub issue metadata on a specific task.
func SetGitHubIssueFieldsOnTask(ctx context.Context, query string, taskUUID uuid.UUID, issueNumber int, issueURL string) error {
	return exec(ctx, "SetGitHubIssueFieldsOnTask", query, issueNumber, issueURL, taskUUID)
}

// FindTaskUUIDByGitHubIssueURL finds a task UUID by its GitHub issue URL ("" for none).
func FindTaskUUIDByGitHubIssueURL(ctx context.Context, query string, issueURL string) (string, error) {
	return scalarOrZero[string](ctx, query, issueURL)
}

// FindTasksUUIDByGitHubURLs is which of urls are already linked to a task,
// url to task id, in one round trip: the import paths' check of hundreds or
// thousands of addresses, so that what is skipped can name the task it is
// already linked to.
func FindTasksUUIDByGitHubURLs(ctx context.Context, query string, urls []string) (map[string]string, error) {
	if len(urls) == 0 {
		return nil, nil
	}
	return pgexec.Pairs[string, string](ctx, query, pq.Array(urls))
}

// GetTaskLastSyncedAtByIssueURL returns the github_last_synced_at for a task by issue URL.
func GetTaskLastSyncedAtByIssueURL(ctx context.Context, query string, issueURL string) (*time.Time, error) {
	return scalarOrZero[*time.Time](ctx, query, issueURL)
}

// GetTaskLastSyncedAtByPRURL returns the github_last_synced_at for a task by PR URL.
func GetTaskLastSyncedAtByPRURL(ctx context.Context, query string, prURL string) (*time.Time, error) {
	return scalarOrZero[*time.Time](ctx, query, prURL)
}

// ClearGitHubIssueFieldsFromTask clears GitHub issue fields from a task.
func ClearGitHubIssueFieldsFromTask(ctx context.Context, query string, taskUUID uuid.UUID) error {
	return exec(ctx, "ClearGitHubIssueFieldsFromTask", query, taskUUID)
}

// GetTaskGitHubURLs returns the GitHub issue and PR URLs for a task.
func GetTaskGitHubURLs(ctx context.Context, query string, taskUUID uuid.UUID) (issueURL, prURL *string, err error) {
	type urls struct{ issue, pr *string }
	u, _, err := pgexec.QueryOne(ctx, query, func(r pgexec.Row) (urls, error) {
		var u urls
		err := r.Scan(&u.issue, &u.pr)
		return u, err
	}, taskUUID)
	if err != nil {
		return nil, nil, err
	}
	return u.issue, u.pr, nil
}

// TaskGitHubLink is a task's stored GitHub issue and pull request addresses
// ("" for none), with the project it is in (uuid.Nil for none).
type TaskGitHubLink struct {
	TaskID    uuid.UUID
	ProjectID uuid.UUID
	IssueURL  string
	PRURL     string
}

// SetGitHubLastSyncedAt updates the github_last_synced_at timestamp for a task.
func SetGitHubLastSyncedAt(ctx context.Context, query string, taskUUID uuid.UUID, t time.Time) error {
	return exec(ctx, "SetGitHubLastSyncedAt", query, t, taskUUID)
}

// SetGitHubBranchOnTaskByTaskID sets the github_branch on a specific task by its UUID.
func SetGitHubBranchOnTaskByTaskID(ctx context.Context, query string, branchName string, taskUUID uuid.UUID) error {
	return exec(ctx, "SetGitHubBranchOnTaskByTaskID", query, branchName, taskUUID)
}

// FindTaskUUIDByGitHubBranch finds a task UUID by its linked GitHub branch in
// a project linking the repository owner/name: query takes owner, name and
// branch, in that order ("" for none).
func FindTaskUUIDByGitHubBranch(ctx context.Context, query string, owner, name, branch string) (string, error) {
	return scalarOrZero[string](ctx, query, owner, name, branch)
}

// UpdateTaskPRState updates the GitHub PR state fields on a task.
func UpdateTaskPRState(ctx context.Context, query string, taskUUID uuid.UUID, prState string, checkStatus string, reviewState string, isDraft *bool) error {
	return exec(ctx, "UpdateTaskPRState", query, prState, checkStatus, reviewState, isDraft, taskUUID)
}

// UpdateTaskPRInfo updates only PR number and URL for a task.
func UpdateTaskPRInfo(ctx context.Context, query string, taskUUID uuid.UUID, prNumber int, prURL string) error {
	return exec(ctx, "UpdateTaskPRInfo", query, prNumber, prURL, taskUUID)
}

// GetTaskProjectID returns the project_id for a given task UUID (uuid.Nil for none).
func GetTaskProjectID(ctx context.Context, taskUUID uuid.UUID, deletedToo bool) (uuid.UUID, error) {
	id, err := scalarOrZero[uuid.UUID](ctx, `
		SELECT project_id FROM tasks WHERE id = $1 AND ($2 OR deleted_at IS NULL)
	`, taskUUID, deletedToo)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetTaskProjectID Failed err: %+v", err)
		return uuid.Nil, err
	}
	return id, nil
}

// SetGitHubSyncStatus updates the sync status, error, and attempt count for a task.
func SetGitHubSyncStatus(ctx context.Context, taskUUID uuid.UUID, status string, errMsg *string, attempts int) error {
	return exec(ctx, "SetGitHubSyncStatus", `
		UPDATE tasks
		SET github_sync_status = $1,
			github_sync_error = $2,
			github_sync_attempts = $3,
			updated_at = NOW()
		WHERE id = $4
	`, status, errMsg, attempts, taskUUID)
}

// GetGitHubSyncStatus returns the current sync status fields for a task.
func GetGitHubSyncStatus(ctx context.Context, taskUUID uuid.UUID) (status string, errMsg *string, attempts int, err error) {
	type sync struct {
		s, e sql.NullString
		a    sql.NullInt32
	}
	got, _, err := pgexec.QueryOne(ctx, `
		SELECT github_sync_status, github_sync_error, github_sync_attempts
		FROM tasks WHERE id = $1
	`, func(r pgexec.Row) (sync, error) {
		var v sync
		err := r.Scan(&v.s, &v.e, &v.a)
		return v, err
	}, taskUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetGitHubSyncStatus Failed err: %+v", err)
		return "", nil, 0, err
	}
	if got.e.Valid {
		str := got.e.String
		errMsg = &str
	}
	return got.s.String, errMsg, int(got.a.Int32), nil
}

// GitHubMeta is all GitHub metadata fields for a task.
type GitHubMeta struct {
	IssueNumber   *int
	IssueURL      *string
	PRNumber      *int
	PRURL         *string
	Branch        *string
	PRState       *string
	PRCheckStatus *string
	PRReviewState *string
	PRIsDraft     *bool
	SyncStatus    string
	SyncError     *string
	SyncAttempts  int
	LastSyncedAt  *time.Time
}

// linkDest is where a task's GitHub link columns land (github_issue_number,
// github_issue_url, github_pr_number, github_pr_url, github_branch,
// github_pr_state, github_pr_check_status, github_pr_review_state,
// github_pr_is_draft, in that order); fill copies them into a GitHubMeta.
type linkDest struct {
	in, pn                 sql.NullInt32
	iu, pu, b, ps, pcs, rs sql.NullString
	draft                  sql.NullBool
}

func (d *linkDest) fill(m *GitHubMeta) {
	str := func(s sql.NullString) *string {
		if !s.Valid {
			return nil
		}
		v := s.String
		return &v
	}
	num := func(n sql.NullInt32) *int {
		if !n.Valid {
			return nil
		}
		v := int(n.Int32)
		return &v
	}
	m.IssueNumber, m.PRNumber = num(d.in), num(d.pn)
	m.IssueURL, m.PRURL, m.Branch = str(d.iu), str(d.pu), str(d.b)
	m.PRState, m.PRCheckStatus, m.PRReviewState = str(d.ps), str(d.pcs), str(d.rs)
	if d.draft.Valid {
		v := d.draft.Bool
		m.PRIsDraft = &v
	}
}

// GetGitHubMetaForTask is a task's GitHub link and sync state (nil for no task).
func GetGitHubMetaForTask(ctx context.Context, taskUUID uuid.UUID) (*GitHubMeta, error) {
	m, found, err := pgexec.QueryOne(ctx, `
		SELECT github_issue_number, github_issue_url, github_pr_number, github_pr_url, github_branch,
			   github_pr_state, github_pr_check_status, github_pr_review_state, github_pr_is_draft,
			   github_sync_status, github_sync_error, github_sync_attempts, github_last_synced_at
		FROM tasks WHERE id = $1
	`, func(r pgexec.Row) (*GitHubMeta, error) {
		var d linkDest
		var ss, se sql.NullString
		var sa sql.NullInt32
		var lsa sql.NullTime
		if err := r.Scan(&d.in, &d.iu, &d.pn, &d.pu, &d.b, &d.ps, &d.pcs, &d.rs, &d.draft, &ss, &se, &sa, &lsa); err != nil {
			return nil, err
		}
		m := &GitHubMeta{SyncStatus: ss.String, SyncAttempts: int(sa.Int32)}
		d.fill(m)
		if se.Valid {
			m.SyncError = &se.String
		}
		if lsa.Valid {
			m.LastSyncedAt = &lsa.Time
		}
		return m, nil
	}, taskUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetGitHubMetaForTask Failed err: %+v", err)
		return nil, err
	}
	if !found {
		return nil, nil
	}
	return m, nil
}

// GetGitHubMetaForTasks returns GitHub metadata for multiple tasks in a single query.
func GetGitHubMetaForTasks(ctx context.Context, taskUUIDs []uuid.UUID) (map[uuid.UUID]*GitHubMeta, error) {
	if len(taskUUIDs) == 0 {
		return map[uuid.UUID]*GitHubMeta{}, nil
	}
	type row struct {
		id uuid.UUID
		m  *GitHubMeta
	}
	rows, err := pgexec.Query(ctx, `
		SELECT id, github_issue_number, github_issue_url, github_pr_number, github_pr_url, github_branch,
			   github_pr_state, github_pr_check_status, github_pr_review_state, github_pr_is_draft
		FROM tasks WHERE id = ANY($1)`, func(r pgexec.Row) (row, error) {
		var d linkDest
		out := row{m: &GitHubMeta{}}
		if err := r.Scan(&out.id, &d.in, &d.iu, &d.pn, &d.pu, &d.b, &d.ps, &d.pcs, &d.rs, &d.draft); err != nil {
			return out, err
		}
		d.fill(out.m)
		return out, nil
	}, pq.Array(taskUUIDs))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetGitHubMetaForTasks Failed err: %+v", err)
		return nil, err
	}
	result := make(map[uuid.UUID]*GitHubMeta, len(rows))
	for _, r := range rows {
		result[r.id] = r.m
	}
	return result, nil
}

// TouchTasks sets updated_at on many tasks, in one statement (PR2).
func TouchTasks(ctx context.Context, ids []uuid.UUID, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := pgexec.Exec(ctx, `UPDATE tasks SET updated_at = $1 WHERE id = ANY($2::uuid[])`, at, pq.Array(ids))
	return err
}

// GitHubIssueLink is a task and the issue it links.
type GitHubIssueLink struct {
	Task   uuid.UUID
	Number int
	URL    string
}

// SetGitHubIssueFieldsOnTasks links each task to its issue, in one statement.
func SetGitHubIssueFieldsOnTasks(ctx context.Context, links []GitHubIssueLink) error {
	if len(links) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(links))
	numbers := make([]int64, len(links))
	urls := make([]string, len(links))
	for i, l := range links {
		ids[i], numbers[i], urls[i] = l.Task, int64(l.Number), l.URL
	}
	_, err := pgexec.Exec(ctx, `UPDATE tasks t SET github_issue_number = l.n, github_issue_url = l.u, github_last_synced_at = NOW()
		FROM unnest($1::uuid[], $2::bigint[], $3::text[]) AS l(id, n, u) WHERE t.id = l.id`, pq.Array(ids), pq.Array(numbers), pq.Array(urls))
	return err
}

// ClearGitHubLinkFromTasks unlinks tasks from GitHub, in one statement.
func ClearGitHubLinkFromTasks(ctx context.Context, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := pgexec.Exec(ctx, `UPDATE tasks SET github_issue_number = NULL, github_issue_url = NULL,
		github_pr_number = NULL, github_pr_url = NULL, github_branch = NULL, github_pr_state = NULL,
		github_pr_check_status = NULL, github_pr_review_state = NULL, github_pr_is_draft = false,
		github_last_synced_at = NULL, updated_at = NOW() WHERE id = ANY($1::uuid[])`, pq.Array(ids))
	return err
}
