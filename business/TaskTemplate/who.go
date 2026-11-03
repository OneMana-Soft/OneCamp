package business

import (
	"context"

	principalBusiness "github.com/akashc777/OneCamp/business/Principal"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// Shown is a template as its readers get it: with the person or AI teammate
// its preset assignee is, as the graph knows them, so New task's chip and the
// manager show an AI teammate who isn't one of the project's people as they
// show a member (DB-92, 2 Nov: the chip looked only among the project's
// members, and an agent preset showed empty).
type Shown struct {
	Template
	AssigneeWho *dgraphStruct.DgraphUser `json:"assignee_who,omitempty"`
}

// Seams for tests: assignee (the shared rule) and a read of someone.
var (
	lookAssignee = assignee
	lookUser     = userBusiness.GetDgraphUserInfoByUUID
)

// Show is t with its assignee's who, while the shared rule lets them be
// given its tasks: in a project, assignee (one of its people or an AI
// teammate, whose account may be given work, as make asks); for the
// workspace's, an account that may be given work (Principal's
// CanBeAssigned), which each project's make then checks against its people.
// Anyone else is left out, and the chip says Assignee.
func Show(ctx context.Context, t Template, project uuid.UUID) Shown {
	out := Shown{Template: t}
	id := t.Shape.Assignee
	if id == "" {
		return out
	}
	var u *dgraphStruct.DgraphUser
	if project != uuid.Nil {
		u, _ = lookAssignee(ctx, id, project)
	} else if got, err := lookUser(ctx, id); err == nil && got != nil && principalBusiness.CanBeAssigned(got).Allowed {
		u = got
	}
	if u != nil && u.Uuid != "" {
		// Only what a chip draws: never the account's email or graph uid.
		out.AssigneeWho = &dgraphStruct.DgraphUser{Uuid: u.Uuid, UserName: u.UserName, UserFullName: u.UserFullName, ProfileKey: u.ProfileKey, IsBot: u.IsBot, BotKind: u.BotKind}
	}
	return out
}
