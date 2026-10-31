package models

import (
	"strings"

	"github.com/google/uuid"
)

// ReaderProjectsVar is the query block TASK_BLOCKED_OPEN needs (DG10b): the
// variable readerProjects, every project the reader (a user uuid) is in, by a
// member or an admin edge. A query that counts blocked tasks puts it at its
// top level. Anyone else (no reader, or not a uuid) is in no project, so
// nothing counts for them: the count fails closed.
func ReaderProjectsVar(readerUUID string) string {
	reader := ""
	if id, err := uuid.Parse(strings.TrimSpace(readerUUID)); err == nil {
		reader = id.String()
	}
	return `
				var(func: eq(user_uuid, "` + reader + `")) { readerInProject as ~project_members readerRunsProject as ~project_admins }
				readerProjects as var(func: uid(readerInProject, readerRunsProject))`
}
