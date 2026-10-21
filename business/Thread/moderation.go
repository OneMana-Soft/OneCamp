package business

// Who may remove anyone's comment in a resource's threads (docs programme B7,
// owner moderation): A5's rule (commentBusiness.CanModerate), the one the
// comment route uses, so the two never differ. Threads say so to the web app
// (View.CanModerate), which offers Delete on other people's comments to them
// alone. A workspace admin counts only acting themselves: an agent never
// moderates. (commentModeration.go is A5's file less CanRemoveComment: on the merge
// with A5 take A5's.)

import (
	"context"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	commentBusiness "github.com/akashc777/OneCamp/business/Comment"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// mayModerate is whether who, with decision d, may remove anyone's comment.
func mayModerate(ctx context.Context, who accessBusiness.Subject, d accessBusiness.Decision) bool {
	return commentBusiness.CanModerate(moderatorOf(ctx, who, d))
}

// moderatorOf is who, with decision d, as moderating is decided by: the
// workspace admin flag counts only for someone acting themselves, never through
// an API token (a token acts as a member, helpers.ActsThroughToken) or as an
// agent. Removing one stored comment asks the same (commentChanges.go,
// CanRemoveComment).
func moderatorOf(ctx context.Context, who accessBusiness.Subject, d accessBusiness.Decision) commentBusiness.Moderator {
	u, ok := userModels.FromContext(ctx)
	admin := who.AgentUUID == "" && ok && u.UserPostgresInfo.IsAdmin && !helpers.ActsThroughToken(ctx) && u.UserDgraphInfo.Uuid == who.UserUUID
	return commentBusiness.Moderator{
		Owner:      d.Owner,
		FullAccess: d.Level >= accessBusiness.LevelFull,
		Admin:      admin,
		Deleted:    d.Deleted,
	}
}

// CanModerateDoc is whether who may remove anyone's comment on a doc,
// guests' included (docs C5-2): the rule above, for the doc as who is decided.
func CanModerateDoc(ctx context.Context, who accessBusiness.Subject, docUUID string) (bool, error) {
	d, err := accessBusiness.Rules.Resolve(ctx, who, accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: docUUID})
	if err != nil {
		return false, err
	}
	return mayModerate(ctx, who, d), nil
}
