package models

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	taskModels "github.com/akashc777/OneCamp/models/postgres/Task"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

type GitHubLink struct {
	Id                uuid.UUID  `json:"id"`
	ProjectId         uuid.UUID  `json:"project_id"`
	RepoOwner         string     `json:"repo_owner"`
	RepoName          string     `json:"repo_name"`
	InstallationId    *int64     `json:"installation_id,omitempty"`
	WebhookSecret     *string    `json:"webhook_secret,omitempty"`
	SyncIssues        bool       `json:"sync_issues"`
	SyncPRs           bool       `json:"sync_prs"`
	AutoCreateTasks   bool       `json:"auto_create_tasks"`
	DefaultTaskStatus string     `json:"default_task_status"`
	LabelMapping      *string    `json:"label_mapping,omitempty"`
	AutomationRules   *string    `json:"automation_rules,omitempty"`
	BranchFormat      string     `json:"branch_format"`
	CreatedBy         uuid.UUID  `json:"created_by"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	DeletedAt         *time.Time `json:"deleted_at,omitempty"`
}

const GITHUB_LINK_COLS = `id, project_id, repo_owner, repo_name, installation_id, webhook_secret, sync_issues, sync_prs, auto_create_tasks, default_task_status, label_mapping, automation_rules, branch_format, created_by, created_at, updated_at, deleted_at`

func CreateGitHubLink(query string, id uuid.UUID, projectId uuid.UUID, repoOwner string, repoName string, installationId *int64, webhookSecret *string, syncIssues bool, syncPRs bool, autoCreateTasks bool, createdBy uuid.UUID, createdAt time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, query, id, projectId, repoOwner, repoName, installationId, webhookSecret, syncIssues, syncPRs, autoCreateTasks, createdBy, createdAt)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CreateGitHubLink Failed err: %+v", err)
		return err
	}
	return nil
}

func GetGitHubLinkById(query string, id uuid.UUID) (*GitHubLink, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	link, err := scanLink(postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetGitHubLinkById Failed err: %+v", err)
		return nil, err
	}
	return link, nil
}

func GetGitHubLinksByProjectId(query string, projectId uuid.UUID) ([]*GitHubLink, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	links, err := queryLinks(ctx, query, projectId)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetGitHubLinksByProjectId Failed err: %+v", err)
		return nil, err
	}
	return links, nil
}

// GetGitHubLinksByRepo is every project's live link to the repository
// owner/name, the oldest first, matched in any case as GitHub matches names.
// Several projects may link one repository (the unique index is per project).
func GetGitHubLinksByRepo(ctx context.Context, repoOwner string, repoName string) ([]*GitHubLink, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	links, err := queryLinks(dbCtx, `SELECT `+GITHUB_LINK_COLS+` FROM github_links
		WHERE lower(repo_owner) = lower($1) AND lower(repo_name) = lower($2) AND deleted_at IS NULL
		ORDER BY created_at, id`, repoOwner, repoName)
	if err != nil {
		helpers.LogErrorWithContext(dbCtx, "models/GetGitHubLinksByRepo Failed err: %+v", err)
		return nil, err
	}
	return links, nil
}

func GetAllGitHubLinks(query string) ([]*GitHubLink, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	links, err := queryLinks(ctx, query)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetAllGitHubLinks Failed err: %+v", err)
		return nil, err
	}
	return links, nil
}

// queryLinks is every row query returns, each in GITHUB_LINK_COLS order.
func queryLinks(ctx context.Context, query string, args ...any) ([]*GitHubLink, error) {
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var links []*GitHubLink
	for rows.Next() {
		link, err := scanLink(rows)
		if err != nil {
			return nil, err
		}
		links = append(links, link)
	}
	// Iteration can stop on a mid-query failure (dropped connection, server-side
	// error) rather than on end-of-rows. Without it this returns a PARTIAL result
	// with a nil error, and the caller cannot tell truncated data from a short list.
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return links, nil
}

// scanLink reads one row in GITHUB_LINK_COLS order.
func scanLink(row interface{ Scan(dest ...any) error }) (*GitHubLink, error) {
	var link GitHubLink
	var installationId sql.NullInt64
	var webhookSecret, labelMapping, automationRules sql.NullString
	var deletedAt sql.NullTime

	if err := row.Scan(&link.Id, &link.ProjectId, &link.RepoOwner, &link.RepoName, &installationId, &webhookSecret, &link.SyncIssues, &link.SyncPRs, &link.AutoCreateTasks, &link.DefaultTaskStatus, &labelMapping, &automationRules, &link.BranchFormat, &link.CreatedBy, &link.CreatedAt, &link.UpdatedAt, &deletedAt); err != nil {
		return nil, err
	}
	if installationId.Valid {
		link.InstallationId = &installationId.Int64
	}
	if webhookSecret.Valid {
		link.WebhookSecret = &webhookSecret.String
	}
	if labelMapping.Valid {
		link.LabelMapping = &labelMapping.String
	}
	if automationRules.Valid {
		link.AutomationRules = &automationRules.String
	}
	if deletedAt.Valid {
		link.DeletedAt = &deletedAt.Time
	}
	return &link, nil
}

func SoftDeleteGitHubLink(query string, deletedAt time.Time, id uuid.UUID) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, query, deletedAt, deletedAt, id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/SoftDeleteGitHubLink Failed err: %+v", err)
		return err
	}
	return nil
}

func SoftDeleteAllGitHubLinks(query string, deletedAt time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, query, deletedAt, deletedAt)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/SoftDeleteAllGitHubLinks Failed err: %+v", err)
		return err
	}
	return nil
}

func BatchClearAllGitHubFields(query string) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, query)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/BatchClearAllGitHubFields Failed err: %+v", err)
		return err
	}
	return nil
}

// TasksOnBranch is every task, deleted ones too, carrying branch in a project
// whose live link to the repository owner/name syncs its pull requests, the
// names matched in any case: its id, its project and its two addresses.
func TasksOnBranch(ctx context.Context, owner, name, branch string) ([]taskModels.TaskGitHubLink, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbCtx, `SELECT t.id, t.project_id, t.github_issue_url, t.github_pr_url FROM tasks t
		WHERE t.github_branch = $3 AND t.project_id IS NOT NULL AND t.project_id IN (SELECT l.project_id FROM github_links l
			WHERE lower(l.repo_owner) = lower($1) AND lower(l.repo_name) = lower($2) AND l.deleted_at IS NULL AND l.sync_prs)`,
		owner, name, branch)
	if err != nil {
		helpers.LogErrorWithContext(dbCtx, "models/TasksOnBranch Failed err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var out []taskModels.TaskGitHubLink
	for rows.Next() {
		var l taskModels.TaskGitHubLink
		var issue, pr sql.NullString
		if err := rows.Scan(&l.TaskID, &l.ProjectID, &issue, &pr); err != nil {
			helpers.LogErrorWithContext(dbCtx, "models/TasksOnBranch scan Failed err: %+v", err)
			return nil, err
		}
		l.IssueURL, l.PRURL = issue.String, pr.String
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		helpers.LogErrorWithContext(dbCtx, "models/TasksOnBranch rows iteration failed err: %+v", err)
		return nil, err
	}
	return out, nil
}

// UnsetBranch takes branch off the tasks, in one statement, each only while
// it still carries it, and returns the ones it changed (id and project).
func UnsetBranch(ctx context.Context, tasks []uuid.UUID, branch string) ([]taskModels.TaskGitHubLink, error) {
	if len(tasks) == 0 {
		return nil, nil
	}
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbCtx, `UPDATE tasks SET github_branch = NULL, updated_at = NOW()
		WHERE id = ANY($1::uuid[]) AND github_branch = $2 RETURNING id, project_id`, pq.Array(tasks), branch)
	if err != nil {
		helpers.LogErrorWithContext(dbCtx, "models/UnsetBranch Failed err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var out []taskModels.TaskGitHubLink
	for rows.Next() {
		var l taskModels.TaskGitHubLink
		var project uuid.NullUUID
		if err := rows.Scan(&l.TaskID, &project); err != nil {
			helpers.LogErrorWithContext(dbCtx, "models/UnsetBranch scan Failed err: %+v", err)
			return nil, err
		}
		l.ProjectID = project.UUID
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		helpers.LogErrorWithContext(dbCtx, "models/UnsetBranch rows iteration failed err: %+v", err)
		return nil, err
	}
	return out, nil
}

// TasksLinkingRepos is which asks, by their index, name a repository their
// task's own project links: tasks[i] with owners[i]/names[i], the names
// matched in any case. A deleted task, or one in no project, links none.
func TasksLinkingRepos(ctx context.Context, tasks []uuid.UUID, owners, names []string) ([]int, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbCtx, `SELECT u.i - 1 FROM unnest($1::uuid[], $2::text[], $3::text[]) WITH ORDINALITY AS u(task, owner, name, i)
		WHERE EXISTS (SELECT 1 FROM tasks t JOIN github_links l ON l.project_id = t.project_id AND l.deleted_at IS NULL
			WHERE t.id = u.task AND t.deleted_at IS NULL AND lower(l.repo_owner) = lower(u.owner) AND lower(l.repo_name) = lower(u.name))`,
		pq.Array(tasks), pq.Array(owners), pq.Array(names))
	if err != nil {
		helpers.LogErrorWithContext(dbCtx, "models/TasksLinkingRepos Failed err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var linked []int
	for rows.Next() {
		var i int
		if err := rows.Scan(&i); err != nil {
			helpers.LogErrorWithContext(dbCtx, "models/TasksLinkingRepos scan Failed err: %+v", err)
			return nil, err
		}
		linked = append(linked, i)
	}
	if err := rows.Err(); err != nil {
		helpers.LogErrorWithContext(dbCtx, "models/TasksLinkingRepos rows iteration failed err: %+v", err)
		return nil, err
	}
	return linked, nil
}

// LinkedTasks is every task of project, or of every project when project is
// uuid.Nil, with a GitHub issue or pull request stored: its id, its project
// and the two addresses ("" for none). Deleted tasks are among them, since a
// restored task brings its link back. The partial indexes on the two
// addresses serve it.
func LinkedTasks(ctx context.Context, project uuid.UUID) ([]taskModels.TaskGitHubLink, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout*5)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbCtx, `SELECT id, project_id, github_issue_url, github_pr_url FROM tasks
		WHERE (github_issue_url <> '' OR github_pr_url <> '') AND ($1::uuid IS NULL OR project_id = $1)`,
		uuid.NullUUID{UUID: project, Valid: project != uuid.Nil})
	if err != nil {
		helpers.LogErrorWithContext(dbCtx, "models/LinkedTasks Failed err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var out []taskModels.TaskGitHubLink
	for rows.Next() {
		var l taskModels.TaskGitHubLink
		var projectID uuid.NullUUID
		var issue, pr sql.NullString
		if err := rows.Scan(&l.TaskID, &projectID, &issue, &pr); err != nil {
			helpers.LogErrorWithContext(dbCtx, "models/LinkedTasks scan Failed err: %+v", err)
			return nil, err
		}
		l.ProjectID, l.IssueURL, l.PRURL = projectID.UUID, issue.String, pr.String
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		helpers.LogErrorWithContext(dbCtx, "models/LinkedTasks rows iteration failed err: %+v", err)
		return nil, err
	}
	return out, nil
}

// UnlinkTasks takes, in one statement, each row's issue and pull request off
// its task ("" keeps that one): a pull request goes with its branch and its
// state, and a task left with neither forgets when it was last synced. An
// address is taken off only while the task still holds it, so a link changed
// since the row was read is left alone. It returns the rows it changed, each
// with the addresses it took off.
func UnlinkTasks(ctx context.Context, unlink []taskModels.TaskGitHubLink) ([]taskModels.TaskGitHubLink, error) {
	if len(unlink) == 0 {
		return nil, nil
	}
	ids := make([]uuid.UUID, len(unlink))
	issues := make([]string, len(unlink))
	prs := make([]string, len(unlink))
	for i, u := range unlink {
		ids[i], issues[i], prs[i] = u.TaskID, u.IssueURL, u.PRURL
	}

	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout*5)
	defer cancel()

	// Every expression reads the task as it is when its row is written, a
	// concurrent change included, so each address is checked where it is
	// taken off.
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbCtx, `UPDATE tasks t SET
			github_issue_number = CASE WHEN t.github_issue_url = o.issue THEN NULL ELSE t.github_issue_number END,
			github_issue_url = CASE WHEN t.github_issue_url = o.issue THEN NULL ELSE t.github_issue_url END,
			github_pr_number = CASE WHEN t.github_pr_url = o.pr THEN NULL ELSE t.github_pr_number END,
			github_pr_url = CASE WHEN t.github_pr_url = o.pr THEN NULL ELSE t.github_pr_url END,
			github_branch = CASE WHEN t.github_pr_url = o.pr THEN NULL ELSE t.github_branch END,
			github_pr_state = CASE WHEN t.github_pr_url = o.pr THEN NULL ELSE t.github_pr_state END,
			github_pr_check_status = CASE WHEN t.github_pr_url = o.pr THEN NULL ELSE t.github_pr_check_status END,
			github_pr_review_state = CASE WHEN t.github_pr_url = o.pr THEN NULL ELSE t.github_pr_review_state END,
			github_pr_is_draft = CASE WHEN t.github_pr_url = o.pr THEN false ELSE t.github_pr_is_draft END,
			github_last_synced_at = CASE WHEN COALESCE(NULLIF(t.github_issue_url, o.issue), '') = ''
				AND COALESCE(NULLIF(t.github_pr_url, o.pr), '') = '' THEN NULL ELSE t.github_last_synced_at END,
			updated_at = NOW()
		FROM (SELECT id, NULLIF(issue, '') AS issue, NULLIF(pr, '') AS pr
			FROM unnest($1::uuid[], $2::text[], $3::text[]) AS u(id, issue, pr)) o
		WHERE t.id = o.id AND (t.github_issue_url = o.issue OR t.github_pr_url = o.pr)
		RETURNING t.id, t.project_id,
			CASE WHEN t.github_issue_url IS NULL THEN COALESCE(o.issue, '') ELSE '' END,
			CASE WHEN t.github_pr_url IS NULL THEN COALESCE(o.pr, '') ELSE '' END`,
		pq.Array(ids), pq.Array(issues), pq.Array(prs))
	if err != nil {
		helpers.LogErrorWithContext(dbCtx, "models/UnlinkTasks Failed err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var out []taskModels.TaskGitHubLink
	for rows.Next() {
		var l taskModels.TaskGitHubLink
		var projectID uuid.NullUUID
		if err := rows.Scan(&l.TaskID, &projectID, &l.IssueURL, &l.PRURL); err != nil {
			helpers.LogErrorWithContext(dbCtx, "models/UnlinkTasks scan Failed err: %+v", err)
			return nil, err
		}
		l.ProjectID = projectID.UUID
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		helpers.LogErrorWithContext(dbCtx, "models/UnlinkTasks rows iteration failed err: %+v", err)
		return nil, err
	}
	return out, nil
}

// UpdateAutomationRules sets the JSON automation_rules blob for a link.
// Used by the GitHub controller's "save automation rules" endpoint —
// previously this was a raw SQL exec inside the controller.
func UpdateAutomationRules(ctx context.Context, linkId uuid.UUID, rulesJSON string) error {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx,
		`UPDATE github_links SET automation_rules = $1, updated_at = NOW() WHERE id = $2`,
		rulesJSON, linkId)
	if err != nil {
		helpers.LogErrorWithContext(dbCtx, "models/UpdateAutomationRules Failed err: %+v", err)
	}
	return err
}

// UpdateBranchFormat sets the branch_format string for a link.
func UpdateBranchFormat(ctx context.Context, linkId uuid.UUID, branchFormat string) error {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx,
		`UPDATE github_links SET branch_format = $1, updated_at = NOW() WHERE id = $2`,
		branchFormat, linkId)
	if err != nil {
		helpers.LogErrorWithContext(dbCtx, "models/UpdateBranchFormat Failed err: %+v", err)
	}
	return err
}
