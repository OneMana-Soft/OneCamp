package provider

import (
	"context"

	"github.com/google/uuid"
)

// Rechecker is a provider that can read again what an old import brought, so
// an install applies the fixed importer's own rules to it (business/Import
// recheck.go): before each provider's fix (its key in business/Import
// importFixes.go), an import let every member of a project read what its
// source keeps to some people.
type Rechecker interface {
	// Recheck reads each task (by its source id) as the source has it now,
	// with owner's stored connection, and, of each task's comments asked about
	// (comments, by the task's source id), which the source keeps from some
	// of the people who see the task. It changes nothing at the source.
	Recheck(ctx context.Context, owner uuid.UUID, tasks []string, comments map[string][]string) (map[string]RecheckedTask, error)
}

// RecheckedTask is a task as its source has it now. Gone is a task the
// source no longer answers for: deleted, or hidden from the connection's
// person. Otherwise Task holds its people and why the source keeps it to some
// of them (ProjectSourceID, CreatedBy, AssigneeIds, Restricted and Audience),
// KeptComments why the source keeps each comment asked about that it keeps
// from some, and MissingComments those asked about it no longer has.
type RecheckedTask struct {
	Gone            bool
	Task            SourceTask
	KeptComments    map[string]string
	MissingComments []string
}
