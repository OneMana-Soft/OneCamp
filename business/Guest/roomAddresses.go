package business

// Rooms at an address made from their owner's email (27 Oct 2026). A
// personal room's first address is its owner's name, which was read through
// the admin screens' lookup, and that falls back to the address: most
// members have no users.display_name, so most rooms stood at their owner's
// email's words, for anyone with the link to read. ownerName never gives an
// address now; at each start, every room still at one moves to the address a
// new room gets: its owner's name when free, else that with a few random
// letters, else "room". Beta and the demo only (no release made rooms), so
// the old address needn't lead anywhere. A room at an address its owner chose
// is left alone, and a moved room no longer matches, so later passes find
// nothing to do.

import (
	"context"
	"strings"
	"time"

	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	"github.com/akashc777/OneCamp/helpers"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

func init() {
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "guests.room-addresses", Every: 24 * time.Hour, RunAtStart: true, Run: MoveRoomsOffOwnersAddresses})
}

// Seams for tests: the rooms, and each owner's email, which a room's first
// address was made from when they had no display name. It's read as the
// email itself (CR-65, 27 Oct): the admin screens' name is the display name
// once one is set, and a room made before that was then missed for good.
var (
	standingRooms = guestModel.ListLiveStanding
	ownerEmails   = userModels.EmailsByUUIDs
)

// MoveRoomsOffOwnersAddresses moves every room whose address was made from its
// owner's email to the address a new room would get now. Nothing while
// guests or rooms are off: a room can't be published then, and the next start
// tries again.
func MoveRoomsOffOwnersAddresses(ctx context.Context) error {
	if !guestOn() || !roomsAllowed() {
		return nil
	}
	rooms, err := standingRooms(ctx)
	if err != nil || len(rooms) == 0 {
		return err
	}
	owners := make([]uuid.UUID, 0, len(rooms))
	for _, g := range rooms {
		owners = append(owners, g.CreatedBy)
	}
	emails, err := ownerEmails(ctx, owners)
	if err != nil {
		return err
	}
	moved := 0
	for _, g := range rooms {
		if g.Slug == nil || !madeFromAddress(*g.Slug, emails[g.CreatedBy]) {
			continue
		}
		if err := moveRoom(ctx, g); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Guest guests.room-addresses could not move room %s: %+v", g.Id, err)
			continue
		}
		moved++
	}
	// How many moved (CR-65 SF2): after the first start, none.
	if moved > 0 {
		helpers.LogInfoWithContext(ctx, "business/Guest guests.room-addresses moved %d of %d rooms off their owners' addresses", moved, len(rooms))
	}
	return nil
}

// moveRoom publishes a room again at the address a new room gets.
func moveRoom(ctx context.Context, g *guestModel.GuestGrant) error {
	slug, err := freshAddress(ctx, KindRoom, g)
	if err != nil {
		return err
	}
	_, err = PublishLink(ctx, g.CreatedBy, g.Id, PublishSettings{Slug: slug})
	return err
}

// madeFromAddress is whether a room's address was made from this email, as a
// room's first address was made from its owner's name: the email's words,
// or those cut to 48 with the eight random letters a taken address gets.
// Pure.
func madeFromAddress(slug, email string) bool {
	if !strings.Contains(email, "@") {
		return false
	}
	base := normaliseSlug(email)
	if base == "" {
		return false
	}
	if slug == base {
		return true
	}
	if len(base) > 48 {
		base = strings.Trim(base[:48], "-")
	}
	tail, ok := strings.CutPrefix(slug, base+"-")
	return ok && len(tail) == 8 && strings.Trim(tail, "0123456789abcdef") == ""
}
