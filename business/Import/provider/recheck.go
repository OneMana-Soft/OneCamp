package provider

import (
	"context"

	"github.com/google/uuid"

	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
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

// PeopleRechecker is a provider whose old imports made people members of
// projects its source keeps them out of (Basecamp's clients): a re-check reads
// who they are now, and an admin decides about each (business/Import
// recheckPeople.go).
type PeopleRechecker interface {
	// RecheckPeople reads, with owner's stored connection to the job's
	// account, which of projects (by source id) answer there, and the
	// account's people: whether the source keeps each out of its projects.
	// It changes nothing at the source.
	RecheckPeople(ctx context.Context, owner uuid.UUID, job *importModels.Job, projects []string) (RecheckedPeople, error)
}

// RecheckedPeople is an account's people as its source has them now:
// Answered holds the projects asked about that answer, KeptOut each person it
// lists, by source id, true when it keeps them out of its projects. A person
// it no longer lists (deleted, or hidden from the connection's person) is
// absent.
type RecheckedPeople struct {
	Answered map[string]bool
	KeptOut  map[string]bool
}
