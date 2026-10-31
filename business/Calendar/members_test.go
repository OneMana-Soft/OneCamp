package Calendar

import (
	"context"
	"errors"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
)

// A calendar's feed answers, a calendar is shared or laid over another's, and
// someone sees when others are busy, only for members now. Dex keeps his
// account but is no member any more (deactivated, or it is an outside guest's
// or a bot's): his feed answers nothing, nobody shares a calendar with him or
// lays his over theirs, and he doesn't see when Sam is busy. Sam still takes
// back the share he gave Dex before. Maya, a member, does all of it.
func TestACalendarGoesOnlyToMembers(t *testing.T) {
	h := newHarness(t)
	feedsOn(t, true)
	sam, maya, dex := h.member("Sam"), h.member("Maya"), h.member("Dex")
	ctx := context.Background()
	samCal, _ := h.store.personalCalendar(ctx, personOf(sam), "UTC")
	dexCal, _ := h.store.personalCalendar(ctx, personOf(dex), "UTC")
	dexFeed, err := h.s.MakeFeed(ctx, dex, dexCal.String(), models.FeedFull)
	if err != nil {
		t.Fatal(err)
	}
	level := func(who *model.UserInfo) accessBusiness.Level {
		t.Helper()
		d, err := h.s.decideCalendars(ctx, subjectOf(who), []string{samCal.String()})
		if err != nil {
			t.Fatal(err)
		}
		return d[samCal.String()].Level
	}
	if err := h.s.Share(ctx, sam, samCal.String(), personOf(dex).String(), models.ShareDetails); err != nil {
		t.Fatalf("Sam's share with Dex while he is a member: %v", err)
	}
	h.store.notMembers[personOf(dex)] = true

	if _, err := h.s.FeedICS(ctx, tokenOf(t, dexFeed)); !errors.Is(err, ErrEventNotFound) {
		t.Errorf("the feed of someone no longer a member answered: %v", err)
	}
	var in *EventInputError
	if err := h.s.Share(ctx, sam, samCal.String(), personOf(dex).String(), models.ShareEdit); !errors.As(err, &in) {
		t.Errorf("Sam's calendar was shared with someone no longer a member: %v", err)
	}
	if err := h.s.Share(ctx, sam, samCal.String(), personOf(dex).String(), ""); err != nil {
		t.Errorf("Sam took back his share with Dex: %v", err)
	}
	if shares, err := h.s.Shares(ctx, sam, samCal.String()); err != nil || len(shares) != 0 {
		t.Errorf("Sam's calendar is still shared: %+v %v", shares, err)
	}
	if got, err := h.s.PersonCalendar(ctx, maya, personOf(dex).String()); !errors.Is(err, ErrEventNotFound) {
		t.Errorf("the calendar of someone no longer a member was laid over Maya's: %s %v", got, err)
	}
	if got, err := h.s.PersonCalendar(ctx, dex, personOf(sam).String()); !errors.Is(err, ErrEventNotFound) {
		t.Errorf("someone no longer a member laid Sam's calendar over theirs: %s %v", got, err)
	}
	if got := level(dex); got != accessBusiness.LevelNone {
		t.Errorf("someone no longer a member sees when Sam is busy: level %v", got)
	}

	if got := level(maya); got != accessBusiness.LevelView {
		t.Errorf("Maya doesn't see when Sam is busy: level %v", got)
	}
	if got, err := h.s.PersonCalendar(ctx, maya, personOf(sam).String()); err != nil || got != samCal {
		t.Errorf("Sam's calendar laid over Maya's: %s %v", got, err)
	}
	if err := h.s.Share(ctx, sam, samCal.String(), personOf(maya).String(), models.ShareDetails); err != nil {
		t.Fatalf("Sam's share with Maya: %v", err)
	}
	mayaFeed, err := h.s.MakeFeed(ctx, maya, samCal.String(), models.FeedFull)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.FeedICS(ctx, tokenOf(t, mayaFeed)); err != nil {
		t.Errorf("Maya's feed of Sam's calendar: %v", err)
	}
}
