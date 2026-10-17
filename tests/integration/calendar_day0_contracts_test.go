//go:build integration
// +build integration

package integration_test

// The calendar programme's day-0 contracts answer as today's code does,
// against real Postgres and Dgraph (design/calendar-vs-gcal/PROGRAM.md §5, and
// §9's "contracts guarded"): Events is the /event routes' functions with
// their rules, StatusFor says focus and time off from the person's events,
// and the event and booking kinds of the Access Resolver decide as today.
//
// Run: go test -tags=integration ./tests/integration/ -run TestCalendarDay0ContractsAnswerAsToday -v

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	calendarAdapter "github.com/akashc777/OneCamp/adapter/Calendar"
	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

func TestCalendarDay0ContractsAnswerAsToday(t *testing.T) {
	env, dg := calendarStores(t)
	ctx := context.Background()
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	jonas := seedCalendarPerson(t, env.PG, dg, "Jonas")
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	events := calendarBusiness.Events

	start := time.Now().Add(2 * time.Hour).Truncate(time.Minute)
	created, err := events.Create(ctx, maya, calendarAdapter.CreateOrUpdateEventInput{Title: "Design review",
		StartTime: rfc(start), EndTime: rfc(start.Add(30 * time.Minute)), Participants: []string{jonas.UserDgraphInfo.Uuid}})
	if err != nil || created == nil {
		t.Fatalf("create: %+v %v", created, err)
	}
	id := uuid.MustParse(created.EventUuid)

	// Who sees it, by the service and by the Access Resolver alike.
	resource := accessBusiness.Resource{Kind: calendarBusiness.KindEvent, UUID: id.String()}
	for _, c := range []struct {
		who   *userModels.UserInfo
		sees  bool
		level accessBusiness.Level
	}{{maya, true, accessBusiness.LevelFull}, {jonas, true, accessBusiness.LevelView}, {sam, false, accessBusiness.LevelNone}} {
		_, err := events.Get(ctx, c.who, id.String())
		if c.sees != (err == nil) || (!c.sees && !errors.Is(err, calendarBusiness.ErrEventNotFound)) {
			t.Errorf("%s getting it: %v", c.who.UserDgraphInfo.UserName, err)
		}
		d, err := accessBusiness.Rules.Resolve(ctx, accessBusiness.Subject{UserUID: c.who.UserDgraphInfo.Uid, UserUUID: c.who.UserDgraphInfo.Uuid}, resource)
		if err != nil || d.Level != c.level {
			t.Errorf("%s's level: %+v %v, want %s", c.who.UserDgraphInfo.UserName, d, err, c.level)
		}
	}
	listed, err := events.Range(ctx, jonas, start.Add(-time.Hour), start.Add(time.Hour))
	found := false
	for _, e := range listed {
		found = found || e.Uuid == id.String()
	}
	if err != nil || !found {
		t.Errorf("Jonas's range doesn't list it: %d events, %v", len(listed), err)
	}

	// Only its creator changes it; a guest's one answer is leaving.
	if err := events.Update(ctx, jonas, id, calendarAdapter.CreateOrUpdateEventInput{Title: "Mine now", StartTime: rfc(start), EndTime: rfc(start.Add(time.Hour))}); !errors.Is(err, calendarBusiness.ErrOnlyCreatorChanges) {
		t.Errorf("Jonas changing it: %v", err)
	}
	if err := events.Respond(ctx, maya, id, calendarBusiness.Declined); !errors.Is(err, calendarBusiness.ErrCreatorCannotLeave) {
		t.Errorf("Maya declining her own event: %v", err)
	}
	if err := events.Respond(ctx, jonas, id, calendarBusiness.Declined); err != nil {
		t.Fatalf("Jonas declining: %v", err)
	}
	if _, err := events.Get(ctx, jonas, id.String()); !errors.Is(err, calendarBusiness.ErrEventNotFound) {
		t.Errorf("Jonas still sees the event he left: %v", err)
	}
	if err := events.Delete(ctx, maya, id); err != nil {
		t.Fatalf("Maya deleting it: %v", err)
	}
	if _, err := events.Get(ctx, maya, id.String()); !errors.Is(err, calendarBusiness.ErrEventNotFound) {
		t.Errorf("a deleted event is still there: %v", err)
	}

	// StatusFor: focus time, then time off over it; a title never shows.
	yes := true
	now := time.Now()
	focusEnd := now.Add(40 * time.Minute).Truncate(time.Minute)
	if _, err := events.Create(ctx, maya, calendarAdapter.CreateOrUpdateEventInput{Title: "Write the launch post",
		StartTime: rfc(now.Add(-20 * time.Minute)), EndTime: rfc(focusEnd), IsFocus: &yes}); err != nil {
		t.Fatal(err)
	}
	if s, err := calendarBusiness.Statuses.StatusFor(ctx, maya.UserDgraphInfo.Uuid, time.Now()); err != nil || s.Kind != calendarBusiness.StatusFocus || s.Until == nil || !s.Until.Equal(focusEnd) {
		t.Errorf("Maya in focus time: %+v %v", s, err)
	}
	if s, err := calendarBusiness.Statuses.StatusFor(ctx, sam.UserDgraphInfo.Uuid, time.Now()); err != nil || s.Kind != "" {
		t.Errorf("Sam's free calendar says %+v %v", s, err)
	}
	awayEnd := now.Add(72 * time.Hour).Truncate(time.Minute)
	if _, err := events.Create(ctx, maya, calendarAdapter.CreateOrUpdateEventInput{Title: "Holiday",
		StartTime: rfc(now.Add(-time.Hour)), EndTime: rfc(awayEnd), IsAway: &yes}); err != nil {
		t.Fatal(err)
	}
	if s, err := calendarBusiness.Statuses.StatusFor(ctx, maya.UserDgraphInfo.Uuid, time.Now()); err != nil || s.Kind != calendarBusiness.StatusAway || !s.Until.Equal(awayEnd) {
		t.Errorf("Maya away: %+v %v", s, err)
	}

	// A booking page: its owner manages it, and a live one is anyone's to see.
	page, err := calendarBusiness.SaveBookingPage(ctx, maya.UserPostgresInfo.Id, calendarBusiness.PageInput{
		Slug: "maya-intro", Title: "Intro call", DurationMinutes: 30, MaxDaysAhead: 30, Active: true,
		Hours: calendarBusiness.WorkingHours{Days: []int{1, 2, 3, 4, 5}, Start: "09:00", End: "17:00", TZ: "UTC"},
	})
	if err != nil {
		t.Fatal(err)
	}
	pageRes := accessBusiness.Resource{Kind: calendarBusiness.KindBooking, UUID: page.Id.String()}
	for who, want := range map[accessBusiness.Subject]accessBusiness.Level{
		{UserUUID: maya.UserDgraphInfo.Uuid}: accessBusiness.LevelFull,
		{UserUUID: sam.UserDgraphInfo.Uuid}:  accessBusiness.LevelView,
		{}:                                   accessBusiness.LevelView,
	} {
		if d, err := accessBusiness.Rules.Resolve(ctx, who, pageRes); err != nil || d.Level != want {
			t.Errorf("%q on the page: %+v %v, want %s", who.UserUUID, d, err, want)
		}
	}
	if _, _, err := calendarBusiness.DeleteBookingPage(ctx, maya.UserPostgresInfo.Id, page.Id); err != nil {
		t.Fatal(err)
	}
	if d, err := accessBusiness.Rules.Resolve(ctx, accessBusiness.Subject{}, pageRes); err != nil || d.Level != accessBusiness.LevelNone || !d.Deleted {
		t.Errorf("a deleted page: %+v %v", d, err)
	}
}
