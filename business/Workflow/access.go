package business

// What a workflow may hear.
//
// A workflow acts with its owner's authority: its steps go through executors
// that re-check what the owner may do. What sets it off is now held to the
// same rule. A workflow scoped to "any channel" heard every channel's
// messages, private ones its owner wasn't in included, and its steps then
// copied them into a task (create_task), quoted them into a review channel
// (flag_to_channel) or replied in the channel (reply). One scoped to "any
// project" posted the names of tasks moving in projects its owner wasn't in.
//
// So "any channel" means any channel the owner can read, and "any project"
// any project they're in: before a step runs, an event from anywhere else is
// skipped (allows). It's the rule an agent's scope follows (an empty scope is
// "the owner's full accessible scope"), and it holds for admins, who can't
// read a private channel they aren't in either. Making "any channel" an
// admin-only choice instead would have left it open: an admin's workflow
// would still have heard every private channel. Who may create workflows at
// all stays the workflow.manage capability's to say.

import (
	"context"
	"errors"

	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	workflowModel "github.com/akashc777/OneCamp/models/postgres/Workflow"
	"github.com/google/uuid"
)

// ownerAccess answers whether a workflow's owner can read a channel, and
// whether they're in a project. The graph answers in production (access);
// tests answer without one.
type ownerAccess struct {
	readsChannel func(ctx context.Context, ownerUUID, channelID string) (bool, error)
	inProject    func(ctx context.Context, ownerUUID, projectID string) (bool, error)
}

var access = ownerAccess{readsChannel: channelBusiness.ReadableBy, inProject: projectBusiness.HasMember}

var (
	errScopeChannel = errors.New("choose a channel the workflow's owner can read")
	errScopeProject = errors.New("choose a project the workflow's owner is in")
)

// allows reports whether a workflow owned by ownerUUID may act on ev: its
// owner can read the channel ev names (where it happened, or where the
// workflow replies) and is in the project it came from. A lookup that fails
// is a no.
func (a ownerAccess) allows(ctx context.Context, ownerUUID string, ev triggerEvent) bool {
	if ev.channelID != "" {
		if ok, err := a.readsChannel(ctx, ownerUUID, ev.channelID); err != nil || !ok {
			return false
		}
	}
	if ev.projectID != "" {
		if ok, err := a.inProject(ctx, ownerUUID, ev.projectID); err != nil || !ok {
			return false
		}
	}
	return true
}

// checkScope refuses to save a workflow scoped to a channel its owner can't
// read, or to moves in a project they aren't in: it would never run, and it
// isn't theirs to name. An id that names nothing is refused the same way.
func (a ownerAccess) checkScope(ctx context.Context, ownerUUID string, channelID *uuid.UUID, triggerType string, triggerConfig map[string]interface{}) error {
	if channelID != nil {
		if ok, err := a.readsChannel(ctx, ownerUUID, channelID.String()); err != nil || !ok {
			return errScopeChannel
		}
	}
	if triggerType == workflowModel.TriggerTaskStatusChanged {
		if projectID, _ := triggerConfig["project_id"].(string); projectID != "" {
			if ok, err := a.inProject(ctx, ownerUUID, projectID); err != nil || !ok {
				return errScopeProject
			}
		}
	}
	return nil
}
