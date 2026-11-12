// Package business (Release) is Releases
// (design/projects-vs-asana-jira/PROGRAM.md §4.2 #98 and D-PJ-16; P10-1b): a
// project's release with its tasks, a target day, notes, and the stages its
// repository's builds and deploys report.
//
// WHO. A release is read as its reader: only in a project they may view, and
// only with the tasks they may see (a restricted task kept from them is
// neither listed nor counted, models/taskread). Every change is its project's
// admins' (NeedManage, checked by the routes as cycles' are), and none while
// the project is archived: its releases then read as before, with can_edit
// false (Editable). A task goes in, or comes out, only when it is a live task
// of that project its admin may see. The one exception is a task's own
// release field (field.go): whoever may change the task sets it, as its
// cycle, but a release that's out still changes only by the admins.
package business

import (
	"context"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	cycleBusiness "github.com/akashc777/OneCamp/business/Cycle"
	releaseModel "github.com/akashc777/OneCamp/models/postgres/Release"
	"github.com/google/uuid"
)

// Release is one release as one reader sees it.
type Release struct {
	releaseModel.Release
	// Progress counts the tasks its reader may see by where they stand.
	Progress cycleBusiness.Progress `json:"progress"`
	// Unfinished is how many of them are still to do: what Mark released
	// moves on when asked.
	Unfinished int `json:"unfinished"`
	// Tasks are those tasks, in the order they joined: Get's answer only.
	Tasks  []Task               `json:"tasks,omitempty"`
	Stages []releaseModel.Stage `json:"stages"`
	// CanEdit is whether the reader runs its project, whose admins change it.
	CanEdit bool `json:"can_edit"`
}

// Task is a task in a release as its reader sees it.
type Task struct {
	UUID    string    `json:"task_uuid"`
	Key     string    `json:"task_key,omitempty"`
	Name    string    `json:"task_name"`
	Status  string    `json:"task_status"`
	Source  string    `json:"source"` // manual, git
	AddedAt time.Time `json:"added_at"`
}

// Listing is a project's releases as one reader sees them, and whether they
// may make one.
type Listing struct {
	Releases []Release `json:"releases"`
	CanEdit  bool      `json:"can_edit"`
	// Next is the cursor of the page after this one; "" on the last page.
	Next string `json:"next_cursor"`
}

// Book keeps releases. A project or release the reader may not see is
// helpers.ErrNotFound, as one that isn't there.
type Book interface {
	// List is a page of a project's releases: limit of them (PageSize when
	// it is below one, PageMost at most), after the page whose Next cursor
	// is ("" for the first).
	List(ctx context.Context, who accessBusiness.Subject, projectID uuid.UUID, cursor string, limit int) (Listing, error)
	Get(ctx context.Context, who accessBusiness.Subject, id uuid.UUID) (*Release, error)
	// Burndown is a release's burndown in loc's days, over the tasks the
	// reader may see.
	Burndown(ctx context.Context, who accessBusiness.Subject, id uuid.UUID, now time.Time, loc *time.Location) (*BurndownView, error)
}

// Releases is the book everything asks.
var Releases Book = book{}
