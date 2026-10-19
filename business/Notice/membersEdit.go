package business

import (
	"context"
	"database/sql"
	"errors"
	"time"

	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
	projectModel "github.com/akashc777/OneCamp/models/postgres/Project"
	"github.com/google/uuid"
)

// MembersEdit is A1's notice (Work Access) for the admins of projects made
// before members could change tasks: "Members can now change tasks in your
// projects. Choose who can, per project". With a project they run, it names
// the first of them, whose "Who can change tasks" it links to; a workspace
// admin who runs none is sent to the workspace's default.
const MembersEdit = "projects.members_edit"

// membersEditSince is the system config migration 320 wrote: when members
// began to change tasks on this install.
const membersEditSince = "projects.members_edit.since"

// Seams for tests: what the notice decides by.
var (
	editWho = func(ctx context.Context) string {
		return policyBusiness.Policies.Choice(ctx, policyBusiness.ProjectsEditWho)
	}
	since = func(ctx context.Context) (time.Time, bool, error) {
		c, err := configModel.GetConfigByKey(membersEditSince)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && c == nil) {
			return time.Time{}, false, nil
		}
		if err != nil {
			return time.Time{}, false, err
		}
		t, perr := time.Parse(time.RFC3339, c.Value)
		if perr != nil {
			return time.Time{}, false, nil
		}
		return t, true, nil
	}
	adminOf = func(ctx context.Context, userUID string) ([]uuid.UUID, error) {
		ps, err := projectDomain.GetDgraphProjectListByAdminDgraphUID(ctx, userUID)
		if err != nil {
			return nil, err
		}
		out := make([]uuid.UUID, 0, len(ps))
		for _, p := range ps {
			if p == nil {
				continue
			}
			if id, err := uuid.Parse(p.Uuid); err == nil {
				out = append(out, id)
			}
		}
		return out, nil
	}
	firstMadeBefore = projectModel.FirstMadeBefore
	anyMadeBefore   = projectModel.AnyMadeBefore
)

var membersEdit = notice{key: MembersEdit, shows: func(ctx context.Context, p Person) (*Shown, error) {
	// A workspace that keeps tasks with its admins has nothing new to say,
	// and an install with no "since" never had projects before it.
	if editWho(ctx) == workAccess.EditAdmins {
		return nil, nil
	}
	at, ok, err := since(ctx)
	if err != nil || !ok {
		return nil, err
	}
	if p.UserUID != "" {
		ids, err := adminOf(ctx, p.UserUID)
		if err != nil {
			return nil, err
		}
		id, name, found, err := firstMadeBefore(ctx, ids, at)
		if err != nil {
			return nil, err
		}
		if found {
			return &Shown{Key: MembersEdit, Data: map[string]string{"project_uuid": id.String(), "project_name": name}}, nil
		}
	}
	if !p.WorkspaceAdmin {
		return nil, nil
	}
	old, err := anyMadeBefore(ctx, at)
	if err != nil || !old {
		return nil, err
	}
	return &Shown{Key: MembersEdit}, nil
}}
