package business

import (
	"context"
	"strings"

	principalBusiness "github.com/akashc777/OneCamp/business/Principal"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	"github.com/google/uuid"
)

// asLead is what a lead is made, as principalBusiness.Unassignable says it:
// "Sam can't be made the project's lead: their account is deactivated."
const asLead = "made the project's lead"

// LeadRule is who may lead the project, as callerUID (a Dgraph uid) reads
// it: one of its admins or members, never a guest. One rule for every way a
// lead is chosen: the plan's route and an import (business/Import
// projectPlan.go).
//
// The project is read for its caller (its member and admin counts are the
// caller's, so workAccess.Of over it would give anyone the caller's
// standing), so it is read again for the person, as a member's role change
// reads it (controllers/Project's SetMemberRole), with the counts made
// theirs; their role row then lowers them as Of does (a viewer or commenter
// doesn't lead). A deactivated member keeps their place (reactivating gives
// it back) but takes no new work, so they're refused by principalBusiness's
// one rule, in its words (a 409).
func LeadRule(project uuid.UUID, callerUID string) MayLead {
	return func(ctx context.Context, person uuid.UUID) (bool, error) {
		p, err := projectBusiness.GetBasicDgraphProjectInfoWithGivenMemberUUID(ctx, project.String(), callerUID, person.String())
		if err != nil || p == nil || len(p.Members) == 0 || !strings.EqualFold(p.Members[0].Uuid, person.String()) || p.Members[0].IsExternal {
			return false, err
		}
		if err := principalBusiness.Unassignable(p.Members[0], asLead); err != nil {
			return false, err
		}
		theirs := *p
		theirs.IsProjectMember, theirs.IsProjectAdmin, theirs.Team = 1, 0, nil
		if len(p.Admins) > 0 && strings.EqualFold(p.Admins[0].Uuid, person.String()) {
			theirs.IsProjectAdmin = 1
		}
		st, err := workAccess.Of(ctx, person.String(), &theirs)
		return st.Role == workAccess.RoleAdmin || st.Role == workAccess.RoleMember, err
	}
}
