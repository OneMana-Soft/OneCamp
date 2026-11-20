package Calendar

import (
	"testing"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// A room the workspace adds itself (no admin: the demo seed) is audited as
// the server's own change and names no one, not a zero uuid; an admin's
// room names the admin.
func TestARoomTheWorkspaceAddsNamesNoOne(t *testing.T) {
	if got := roomActor(nil); got != (resourceEventBusiness.Actor{System: true}) {
		t.Fatalf("the workspace's room is audited as %+v, want the server's own change naming no one", got)
	}
	admin := &model.UserInfo{}
	admin.UserPostgresInfo.Id = uuid.New()
	if got := roomActor(admin); got != (resourceEventBusiness.Actor{UserUUID: admin.UserPostgresInfo.Id.String()}) {
		t.Fatalf("an admin's room is audited as %+v, want the admin", got)
	}
}
