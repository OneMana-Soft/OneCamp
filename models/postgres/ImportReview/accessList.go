package models

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
)

// AccessImportRow is one review of an old import's access with what the
// admins' card shows beside it: the import's site, when it first ran and who
// ran it, the project's name and whether it is archived, and who decided.
type AccessImportRow struct {
	AccessReview
	Site          string
	RanAt         time.Time
	RanBy         string
	ProjectName   string
	Archived      bool
	DecidedByName string
}

// AccessImportRows reads every review of an old import's access with its
// import and project: the newest import first, then its projects by name.
func AccessImportRows(ctx context.Context) ([]AccessImportRow, error) {
	return pgexec.Query(ctx, `
		SELECT r.id, r.import_id, r.provider, r.project_uuid, r.gap, r.state, r.summary, r.decided_by, r.decided_at, r.updated_at,
			COALESCE(j.source_workspace_name, ''), COALESCE(j.started_at, j.created_at),
			COALESCE(NULLIF(u.display_name, ''), u.username, ''),
			COALESCE(p.project_name, ''), p.deleted_at IS NOT NULL,
			COALESCE(NULLIF(d.display_name, ''), d.username, '')
		FROM import_access_reviews r
		JOIN import_jobs j ON j.id = r.import_id
		LEFT JOIN users u ON u.id = j.triggered_by
		JOIN projects p ON p.id = r.project_uuid
		LEFT JOIN users d ON d.id = r.decided_by
		ORDER BY COALESCE(j.started_at, j.created_at) DESC, r.import_id,
			lower(COALESCE(p.project_name, '')), r.project_uuid, r.gap`,
		func(row pgexec.Row) (AccessImportRow, error) {
			var a AccessImportRow
			err := row.Scan(&a.ID, &a.ImportID, &a.Provider, &a.ProjectUUID, &a.Gap, &a.State, &a.Summary,
				&a.DecidedBy, &a.DecidedAt, &a.UpdatedAt,
				&a.Site, &a.RanAt, &a.RanBy, &a.ProjectName, &a.Archived, &a.DecidedByName)
			return a, err
		})
}
