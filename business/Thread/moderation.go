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
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// mayModerate is whether who, with decision d, may remove anyone's comment.
func mayModerate(ctx context.Context, who accessBusiness.Subject, d accessBusiness.Decision) bool {
	u, ok := userModels.FromContext(ctx)
	admin := who.AgentUUID == "" && ok && u.UserPostgresInfo.IsAdmin && u.UserDgraphInfo.Uuid == who.UserUUID
	return commentBusiness.CanModerate(commentBusiness.Moderator{
		Owner:      d.Owner,
		FullAccess: d.Level >= accessBusiness.LevelFull,
		Admin:      admin,
		Deleted:    d.Deleted,
	})
}
