package models

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
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

	var link GitHubLink
	var installationId sql.NullInt64
	var webhookSecret, labelMapping, automationRules sql.NullString
	var deletedAt sql.NullTime

	row := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, id)
	err := row.Scan(&link.Id, &link.ProjectId, &link.RepoOwner, &link.RepoName, &installationId, &webhookSecret, &link.SyncIssues, &link.SyncPRs, &link.AutoCreateTasks, &link.DefaultTaskStatus, &labelMapping, &automationRules, &link.BranchFormat, &link.CreatedBy, &link.CreatedAt, &link.UpdatedAt, &deletedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		helpers.LogErrorWithContext(ctx, "models/GetGitHubLinkById Failed err: %+v", err)
		return nil, err
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
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

func GetGitHubLinksByProjectId(query string, projectId uuid.UUID) ([]*GitHubLink, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query, projectId)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetGitHubLinksByProjectId Failed err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var links []*GitHubLink
	for rows.Next() {
		var link GitHubLink
		var installationId sql.NullInt64
		var webhookSecret, labelMapping, automationRules sql.NullString
		var deletedAt sql.NullTime

		if err := rows.Scan(&link.Id, &link.ProjectId, &link.RepoOwner, &link.RepoName, &installationId, &webhookSecret, &link.SyncIssues, &link.SyncPRs, &link.AutoCreateTasks, &link.DefaultTaskStatus, &labelMapping, &automationRules, &link.BranchFormat, &link.CreatedBy, &link.CreatedAt, &link.UpdatedAt, &deletedAt); err != nil {
			helpers.LogErrorWithContext(ctx, "models/GetGitHubLinksByProjectId scan Failed err: %+v", err)
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

		links = append(links, &link)
	}
	// Iteration can stop on a mid-query failure (dropped connection, server-side
	// error) rather than on end-of-rows. Without it this returns a PARTIAL result
	// with a nil error, and the caller cannot tell truncated data from a short list.
	if err := rows.Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "models/GitHubLink rows iteration failed err: %+v", err)
		return nil, err
	}

	return links, nil
}

// GetGitHubLinkByRepo finds a GitHub link by repo owner and name.
func GetGitHubLinkByRepo(repoOwner string, repoName string) (*GitHubLink, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	query := `SELECT ` + GITHUB_LINK_COLS + ` FROM github_links WHERE repo_owner = $1 AND repo_name = $2 AND deleted_at IS NULL LIMIT 1`
	row := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, repoOwner, repoName)

	var link GitHubLink
	var installationId sql.NullInt64
	var webhookSecret, labelMapping, automationRules sql.NullString
	var deletedAt sql.NullTime

	if err := row.Scan(&link.Id, &link.ProjectId, &link.RepoOwner, &link.RepoName, &installationId, &webhookSecret, &link.SyncIssues, &link.SyncPRs, &link.AutoCreateTasks, &link.DefaultTaskStatus, &labelMapping, &automationRules, &link.BranchFormat, &link.CreatedBy, &link.CreatedAt, &link.UpdatedAt, &deletedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		helpers.LogErrorWithContext(ctx, "models/GetGitHubLinkByRepo Failed err: %+v", err)
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

func GetAllGitHubLinks(query string) ([]*GitHubLink, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetAllGitHubLinks Failed err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var links []*GitHubLink
	for rows.Next() {
		var link GitHubLink
		var installationId sql.NullInt64
		var webhookSecret, labelMapping, automationRules sql.NullString
		var deletedAt sql.NullTime

		if err := rows.Scan(&link.Id, &link.ProjectId, &link.RepoOwner, &link.RepoName, &installationId, &webhookSecret, &link.SyncIssues, &link.SyncPRs, &link.AutoCreateTasks, &link.DefaultTaskStatus, &labelMapping, &automationRules, &link.BranchFormat, &link.CreatedBy, &link.CreatedAt, &link.UpdatedAt, &deletedAt); err != nil {
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

		links = append(links, &link)
	}
	// Iteration can stop on a mid-query failure (dropped connection, server-side
	// error) rather than on end-of-rows. Without it this returns a PARTIAL result
	// with a nil error, and the caller cannot tell truncated data from a short list.
	if err := rows.Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "models/GitHubLink rows iteration failed err: %+v", err)
		return nil, err
	}

	return links, nil
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

func BatchClearGitHubFieldsByProject(query string, projectId uuid.UUID) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, query, projectId)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/BatchClearGitHubFieldsByProject Failed err: %+v", err)
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
