package business

import (
	"context"
	"errors"
	"testing"

	callStateModel "github.com/akashc777/OneCamp/models/postgres/CallState"
	"github.com/google/uuid"
)

// Members who came in through a link are put out of its call when the link
// ends (revoked, rooms or guest access turned off, a room closed), unless
// something else still lets them in: their conversation, their own meeting,
// another live link. A check that can't be answered puts them out: they can
// come back by whatever still lets them in (HU13).
func TestMembersLetInByALinkLeaveWhenItEnds(t *testing.T) {
	oldMay, oldOut := memberMayBeIn, removeFromCall
	t.Cleanup(func() { memberMayBeIn, removeFromCall = oldMay, oldOut })
	still := map[string]bool{"0x2": true}
	memberMayBeIn = func(_ context.Context, room, identity string) (bool, error) {
		if identity == "0x3" {
			return false, errors.New("down")
		}
		return still[identity], nil
	}
	var out []string
	removeFromCall = func(_ context.Context, room, identity string) error {
		out = append(out, room+"/"+identity)
		return nil
	}
	putOutLinkMembers(context.Background(), []callStateModel.Admission{
		{Room: "channel-1", Identity: "0x1", UserUUID: uuid.New(), GrantID: uuid.New()},
		{Room: "channel-1", Identity: "0x2", UserUUID: uuid.New(), GrantID: uuid.New()},
		{Room: "meet-m1", Identity: "0x3", UserUUID: uuid.New(), GrantID: uuid.New()},
	})
	if len(out) != 2 || out[0] != "channel-1/0x1" || out[1] != "meet-m1/0x3" {
		t.Errorf("put out %v; want the one whose right went, and the one that couldn't be checked", out)
	}
}
