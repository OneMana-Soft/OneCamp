package models

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

//
//CREATE TABLE IF NOT EXISTS tasks(
//"id" uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
//"task_name" varchar NOT NULL,
//"project_id" uuid REFERENCES teams(id),
//"created_by" uuid REFERENCES users(id),
//"task_assignee" uuid REFERENCES users(id),
//"created_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
//"updated_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
//"deleted_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW()
//);
//

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

func CreateTask(query string, taskUUID uuid.UUID, projectUUID uuid.UUID, createdByUUID uuid.UUID, createdAt time.Time) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		taskUUID,
		projectUUID,
		createdByUUID,
		createdAt,
	)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateTask Failed to create new task err: %+v",
			err)
		return
	}

	return
}

// func CreateTaskWithAssignee(query string, taskUUID uuid.UUID, projectUUID uuid.UUID, taskAssignee uuid.UUID, createdByUUID uuid.UUID, createdAt time.Time) (err error) {
// 	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
// 	defer cancel()
// 	_, err = postgresInit.DBConn.SqlDB.ExecContext(
// 		ctx,
// 		query,
// 		taskUUID,
// 		projectUUID,
// 		taskAssignee,
// 		createdByUUID,
// 		createdAt,
// 	)
// 	if err != nil {
// 		helpers.LogErrorWithContext(ctx,
// 			"models/CreateTaskWithAssignee Failed to create new task err: %+v",
// 			err)
// 		return
// 	}

// 	return
// }

//func CheckIfTaskExistByTaskNameAndProjectUUID(query string, projectName string, teamId uuid.UUID) (exist bool, err error) {
//	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
//	defer cancel()
//	err = postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, projectName, teamId).Scan(&exist)
//	if err != nil {
//		helpers.LogErrorWithContext(ctx,
//			"models/CheckIfTaskExistByTaskNameAndTeamUUID Failed to check if project name exist in given team ID err: %+v",
//			err)
//		return
//	}
//
//	return
//}

func GetTaskByUUID(query string, uuid uuid.UUID) (task *Task, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	var taskInfo Task
	var createdAt sql.NullTime
	var updatedAt sql.NullTime
	var deletedAt sql.NullTime
	var taskGoogleCalendarId sql.NullString

	row := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, uuid)
	err = row.Scan(
		&taskInfo.Id,
		&taskInfo.CreatedBy,
		&taskInfo.ProjectId,
		&taskInfo.TaskAssignee,
		&createdAt,
		&updatedAt,
		&deletedAt,
		&taskGoogleCalendarId,
	)

	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		helpers.LogErrorWithContext(ctx,
			"models/GetTaskByUUID Failed to get task err: %+v",
			err)
		return
	}

	if createdAt.Valid {
		taskInfo.CreatedAt = createdAt.Time
	}

	if updatedAt.Valid {
		taskInfo.UpdatedAt = updatedAt.Time
	}

	if deletedAt.Valid {
		taskInfo.DeletedAt = deletedAt.Time
	}

	if taskGoogleCalendarId.Valid {
		taskInfo.TaskGoogleCalendarId = &taskGoogleCalendarId.String
	}

	return &taskInfo, nil

}

func UpdateTaskNameByTaskUUID(query string, currentTime time.Time, projectUUID uuid.UUID) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		currentTime,
		projectUUID,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateTaskNameByTaskUUID Failed to update task name err: %+v",
			err)
		return
	}

	return
}

func UpdateTaskDeletedTimeByUUID(query string, projectUUID uuid.UUID, deleteTime *time.Time, updateTime *time.Time) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		deleteTime,
		updateTime,
		projectUUID,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateTaskDeletedTimeByUUID Failed to update task's delete time err: %+v",
			err)
		return
	}

	return
}

func UpdateTaskDeletedTimeToNullByUUID(query string, updateTime *time.Time, projectUUID uuid.UUID) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		updateTime,
		projectUUID,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateTaskDeletedTimeToNullByUUID Failed to update task's delete time to null err: %+v",
			err)
		return
	}

	return
}

func UpdateTaskGoogleCalendarId(query string, googleEventId *string, updatedAt time.Time, taskUUID uuid.UUID) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		googleEventId,
		updatedAt,
		taskUUID,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/UpdateTaskGoogleCalendarId Failed to update task gcal id err: %+v", err)
		return
	}

	return
}

func GetSyncedTaskGCalIds(ctx context.Context, userUID uuid.UUID) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	query := `
		SELECT task_google_calendar_id 
		FROM tasks 
		WHERE task_assignee = $1 AND task_google_calendar_id IS NOT NULL AND task_google_calendar_id != '' AND deleted_at IS NULL
	`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query, userUID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	// Iteration can stop on a mid-query failure (dropped connection, server-side
	// error) rather than on end-of-rows. Without it this returns a PARTIAL result
	// with a nil error, and the caller cannot tell truncated data from a short list.
	if err := rows.Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "models/Task rows iteration failed err: %+v", err)
		return nil, err
	}
	return ids, nil
}

// SetGitHubPRFieldsOnTask sets PR fields on a specific task by UUID.
func SetGitHubPRFieldsOnTask(query string, taskUUID uuid.UUID, prNumber int, prURL string, branch string) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, query, prNumber, prURL, branch, taskUUID)
	return err
}

// FindTaskUUIDByPRURL finds a task by its GitHub PR URL.
func FindTaskUUIDByPRURL(query string, prURL string) (taskUUID string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	err = postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, prURL).Scan(&taskUUID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	return taskUUID, nil
}

// SetGitHubIssueFieldsOnTask sets GitHub issue metadata on a specific task.
func SetGitHubIssueFieldsOnTask(query string, taskUUID uuid.UUID, issueNumber int, issueURL string) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, query, issueNumber, issueURL, taskUUID)
	return err
}

// FindTaskUUIDByGitHubIssueURL finds a task UUID by its GitHub issue URL.
func FindTaskUUIDByGitHubIssueURL(query string, issueURL string) (taskUUID string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	err = postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, issueURL).Scan(&taskUUID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	return taskUUID, nil
}

// FindTasksUUIDByGitHubURLs returns the subset of urls that are
// already linked to a task. Used by the import paths to do
// idempotency checks for hundreds or thousands of URLs in a single
// round trip instead of one query per URL.
//
// Returns a set of URLs that exist (deleted_at IS NULL) plus an
// owner-attribution map (url -> task UUID) so callers can log skipped
// items with the existing task they're already linked to.
func FindTasksUUIDByGitHubURLs(query string, urls []string) (map[string]string, error) {
	if len(urls) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query, pq.Array(urls))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]string, len(urls))
	for rows.Next() {
		var url, taskUUID string
		if err := rows.Scan(&url, &taskUUID); err != nil {
			return nil, err
		}
		out[url] = taskUUID
	}
	return out, rows.Err()
}

// GetTaskLastSyncedAtByIssueURL returns the github_last_synced_at for a task by issue URL.
func GetTaskLastSyncedAtByIssueURL(query string, issueURL string) (lastSyncedAt *time.Time, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	err = postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, issueURL).Scan(&lastSyncedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return lastSyncedAt, nil
}

// GetTaskLastSyncedAtByPRURL returns the github_last_synced_at for a task by PR URL.
func GetTaskLastSyncedAtByPRURL(query string, prURL string) (lastSyncedAt *time.Time, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	err = postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, prURL).Scan(&lastSyncedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return lastSyncedAt, nil
}

// ClearGitHubIssueFieldsFromTask clears GitHub issue fields from a task.
func ClearGitHubIssueFieldsFromTask(query string, taskUUID uuid.UUID) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, query, taskUUID)
	return err
}

// GetTaskGitHubURLs returns the GitHub issue and PR URLs for a task.
func GetTaskGitHubURLs(query string, taskUUID uuid.UUID) (issueURL, prURL *string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	err = postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, taskUUID).Scan(&issueURL, &prURL)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, err
	}
	return issueURL, prURL, nil
}

// SetGitHubLastSyncedAt updates the github_last_synced_at timestamp for a task.
func SetGitHubLastSyncedAt(query string, taskUUID uuid.UUID, t time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, query, t, taskUUID)
	return err
}

// SetGitHubBranchOnTaskByTaskID sets the github_branch on a specific task by its UUID.
func SetGitHubBranchOnTaskByTaskID(query string, branchName string, taskUUID uuid.UUID) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, query, branchName, taskUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/SetGitHubBranchOnTaskByTaskID Failed err: %+v", err)
	}
	return err
}

// FindTaskUUIDByGitHubBranch finds a task UUID by its linked GitHub branch.
func FindTaskUUIDByGitHubBranch(query string, branch string) (taskUUID string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	err = postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, branch).Scan(&taskUUID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	return taskUUID, nil
}

// UpdateTaskPRState updates the GitHub PR state fields on a task.
func UpdateTaskPRState(query string, taskUUID uuid.UUID, prState string, checkStatus string, reviewState string, isDraft *bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, query, prState, checkStatus, reviewState, isDraft, taskUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/UpdateTaskPRState Failed err: %+v", err)
	}
	return err
}

// UpdateTaskPRInfo updates only PR number and URL for a task.
func UpdateTaskPRInfo(query string, taskUUID uuid.UUID, prNumber int, prURL string) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, query, prNumber, prURL, taskUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/UpdateTaskPRInfo Failed err: %+v", err)
	}
	return err
}

// GetTaskProjectID returns the project_id for a given task UUID.
func GetTaskProjectID(taskUUID uuid.UUID) (uuid.UUID, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	var projectID uuid.UUID
	err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `
		SELECT project_id FROM tasks WHERE id = $1 AND deleted_at IS NULL
	`, taskUUID).Scan(&projectID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return uuid.Nil, nil
		}
		helpers.LogErrorWithContext(ctx, "models/GetTaskProjectID Failed err: %+v", err)
		return uuid.Nil, err
	}
	return projectID, nil
}

// SetGitHubSyncStatus updates the sync status, error, and attempt count for a task.
func SetGitHubSyncStatus(taskUUID uuid.UUID, status string, errMsg *string, attempts int) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `
		UPDATE tasks
		SET github_sync_status = $1,
			github_sync_error = $2,
			github_sync_attempts = $3,
			updated_at = NOW()
		WHERE id = $4
	`, status, errMsg, attempts, taskUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/SetGitHubSyncStatus Failed err: %+v", err)
	}
	return err
}

// GetGitHubSyncStatus returns the current sync status fields for a task.
func GetGitHubSyncStatus(taskUUID uuid.UUID) (status string, errMsg *string, attempts int, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	var s sql.NullString
	var e sql.NullString
	var a sql.NullInt32

	err = postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `
		SELECT github_sync_status, github_sync_error, github_sync_attempts
		FROM tasks WHERE id = $1
	`, taskUUID).Scan(&s, &e, &a)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil, 0, nil
		}
		helpers.LogErrorWithContext(ctx, "models/GetGitHubSyncStatus Failed err: %+v", err)
		return "", nil, 0, err
	}

	if s.Valid {
		status = s.String
	}
	if e.Valid {
		str := e.String
		errMsg = &str
	}
	if a.Valid {
		attempts = int(a.Int32)
	}
	return
}

// GetGitHubMetaForTask returns all GitHub metadata fields for a task.
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

func GetGitHubMetaForTask(taskUUID uuid.UUID) (*GitHubMeta, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	var m GitHubMeta
	var in sql.NullInt32
	var iu sql.NullString
	var pn sql.NullInt32
	var pu sql.NullString
	var b sql.NullString
	var ps sql.NullString
	var pcs sql.NullString
	var prs sql.NullString
	var pid sql.NullBool
	var ss sql.NullString
	var se sql.NullString
	var sa sql.NullInt32
	var lsa sql.NullTime

	err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `
		SELECT github_issue_number, github_issue_url, github_pr_number, github_pr_url, github_branch,
			   github_pr_state, github_pr_check_status, github_pr_review_state, github_pr_is_draft,
			   github_sync_status, github_sync_error, github_sync_attempts, github_last_synced_at
		FROM tasks WHERE id = $1
	`, taskUUID).Scan(&in, &iu, &pn, &pu, &b, &ps, &pcs, &prs, &pid, &ss, &se, &sa, &lsa)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		helpers.LogErrorWithContext(ctx, "models/GetGitHubMetaForTask Failed err: %+v", err)
		return nil, err
	}

	if in.Valid {
		n := int(in.Int32)
		m.IssueNumber = &n
	}
	if iu.Valid {
		m.IssueURL = &iu.String
	}
	if pn.Valid {
		n := int(pn.Int32)
		m.PRNumber = &n
	}
	if pu.Valid {
		m.PRURL = &pu.String
	}
	if b.Valid {
		m.Branch = &b.String
	}
	if ps.Valid {
		m.PRState = &ps.String
	}
	if pcs.Valid {
		m.PRCheckStatus = &pcs.String
	}
	if prs.Valid {
		m.PRReviewState = &prs.String
	}
	if pid.Valid {
		m.PRIsDraft = &pid.Bool
	}
	if ss.Valid {
		m.SyncStatus = ss.String
	}
	if se.Valid {
		m.SyncError = &se.String
	}
	if sa.Valid {
		m.SyncAttempts = int(sa.Int32)
	}
	if lsa.Valid {
		m.LastSyncedAt = &lsa.Time
	}
	return &m, nil
}

// GetGitHubMetaForTasks returns GitHub metadata for multiple tasks in a single query.
func GetGitHubMetaForTasks(taskUUIDs []uuid.UUID) (map[uuid.UUID]*GitHubMeta, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout*5)
	defer cancel()

	if len(taskUUIDs) == 0 {
		return map[uuid.UUID]*GitHubMeta{}, nil
	}

	query := `
		SELECT id, github_issue_number, github_issue_url, github_pr_number, github_pr_url, github_branch,
			   github_pr_state, github_pr_check_status, github_pr_review_state, github_pr_is_draft
		FROM tasks WHERE id = ANY($1)
	`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query, pq.Array(taskUUIDs))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetGitHubMetaForTasks Failed err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	result := make(map[uuid.UUID]*GitHubMeta)
	for rows.Next() {
		var taskID uuid.UUID
		var m GitHubMeta
		var in sql.NullInt32
		var iu sql.NullString
		var pn sql.NullInt32
		var pu sql.NullString
		var b sql.NullString
		var ps sql.NullString
		var pcs sql.NullString
		var prs sql.NullString
		var pid sql.NullBool

		if err := rows.Scan(&taskID, &in, &iu, &pn, &pu, &b, &ps, &pcs, &prs, &pid); err != nil {
			helpers.LogErrorWithContext(ctx, "models/GetGitHubMetaForTasks scan Failed err: %+v", err)
			continue
		}
		if in.Valid {
			n := int(in.Int32)
			m.IssueNumber = &n
		}
		if iu.Valid {
			m.IssueURL = &iu.String
		}
		if pn.Valid {
			n := int(pn.Int32)
			m.PRNumber = &n
		}
		if pu.Valid {
			m.PRURL = &pu.String
		}
		if b.Valid {
			m.Branch = &b.String
		}
		if ps.Valid {
			m.PRState = &ps.String
		}
		if pcs.Valid {
			m.PRCheckStatus = &pcs.String
		}
		if prs.Valid {
			m.PRReviewState = &prs.String
		}
		if pid.Valid {
			m.PRIsDraft = &pid.Bool
		}
		result[taskID] = &m
	}
	// Iteration can stop on a mid-query failure (dropped connection, server-side
	// error) rather than on end-of-rows. Without it this returns a PARTIAL result
	// with a nil error, and the caller cannot tell truncated data from a short list.
	if err := rows.Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "models/Task rows iteration failed err: %+v", err)
		return nil, err
	}
	return result, nil
}
