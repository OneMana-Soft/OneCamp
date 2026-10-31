//go:build integration
// +build integration

package integration_test

// A calendar goes only to members, against real Postgres 12 and Dgraph: a
// deactivated person's feed answers nothing; nobody shares a calendar with, or
// lays over theirs the calendar of, someone deactivated, an outside guest's
// account or a bot's; and none of them sees when others are busy.
//
// Run: go test -tags=integration ./tests/integration/ -run '^TestACalendarGoesOnlyToMembers$' -v

import (
	"context"
	"errors"
	"path"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

func TestACalendarGoesOnlyToMembers(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	env, dg := calendarStores(t)
	ctx := context.Background()
	sam := seedCalendarPerson(t, env.PG, dg, "Sam")
	maya := seedCalendarPerson(t, env.PG, dg, "Maya")
	dex := seedCalendarPerson(t, env.PG, dg, "Dex")
	bot := seedCalendarPerson(t, env.PG, dg, "Agent")
	outsider := seedCalendarPerson(t, env.PG, dg, "Outsider")
	for q, who := range map[string]*userModels.UserInfo{
		`UPDATE users SET is_bot = true WHERE id = $1`:      bot,
		`UPDATE users SET is_external = true WHERE id = $1`: outsider,
	} {
		if _, err := env.PG.Exec(q, who.UserPostgresInfo.Id); err != nil {
			t.Fatal(err)
		}
	}
	events := calendarBusiness.Events
	own := func(who *userModels.UserInfo) string {
		t.Helper()
		id, err := events.PersonCalendar(ctx, who, who.UserDgraphInfo.Uuid)
		if err != nil {
			t.Fatalf("%s's own calendar: %v", who.UserDgraphInfo.UserName, err)
		}
		return id.String()
	}
	feedOf := func(who *userModels.UserInfo, cal string) string {
		t.Helper()
		v, err := events.MakeFeed(ctx, who, cal, models.FeedFull)
		if err != nil || v == nil {
			t.Fatalf("%s's feed: %+v %v", who.UserDgraphInfo.UserName, v, err)
		}
		return calendarBusiness.FeedTokenOf(path.Base(v.URL))
	}
	samCal, dexCal := own(sam), own(dex)
	dexFeed := feedOf(dex, dexCal)
	if _, err := events.FeedICS(ctx, dexFeed); err != nil {
		t.Fatalf("Dex's feed while he is a member: %v", err)
	}
	if err := events.Share(ctx, sam, samCal, dex.UserDgraphInfo.Uuid, models.ShareDetails); err != nil {
		t.Fatalf("Sam's share with Dex while he is a member: %v", err)
	}
	if err := userBusiness.DeactivateUser(ctx, dex.UserPostgresInfo.Id); err != nil {
		t.Fatalf("deactivate Dex: %v", err)
	}
	gone := map[string]*userModels.UserInfo{"deactivated": dex, "a bot's": bot, "an outside guest's": outsider}

	t.Run("a deactivated person's feed answers nothing", func(t *testing.T) {
		if _, err := events.FeedICS(ctx, dexFeed); !errors.Is(err, calendarBusiness.ErrEventNotFound) {
			t.Errorf("Dex's feed answered after he was deactivated: %v", err)
		}
	})
	t.Run("nobody shares a calendar with someone who isn't a member", func(t *testing.T) {
		for what, who := range gone {
			var in *calendarBusiness.EventInputError
			if err := events.Share(ctx, sam, samCal, who.UserDgraphInfo.Uuid, models.ShareEdit); !errors.As(err, &in) {
				t.Errorf("Sam shared his calendar with an account that is %s: %v", what, err)
			}
		}
		// The share he gave Dex before, he still takes back.
		if err := events.Share(ctx, sam, samCal, dex.UserDgraphInfo.Uuid, ""); err != nil {
			t.Errorf("Sam took back his share with Dex: %v", err)
		}
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM calendar_shares WHERE calendar_id = $1`, samCal).Scan(&n); err != nil || n != 0 {
			t.Errorf("Sam's calendar has %d shares (%v), want none", n, err)
		}
	})
	t.Run("nobody lays over theirs the calendar of someone who isn't a member", func(t *testing.T) {
		for what, who := range gone {
			if got, err := events.PersonCalendar(ctx, maya, who.UserDgraphInfo.Uuid); !errors.Is(err, calendarBusiness.ErrEventNotFound) {
				t.Errorf("Maya laid over hers the calendar of an account that is %s: %s %v", what, got, err)
			}
			if got, err := events.PersonCalendar(ctx, who, sam.UserDgraphInfo.Uuid); !errors.Is(err, calendarBusiness.ErrEventNotFound) {
				t.Errorf("an account that is %s laid Sam's calendar over theirs: %s %v", what, got, err)
			}
		}
	})
	t.Run("only a member sees when Sam is busy", func(t *testing.T) {
		for who, want := range map[*userModels.UserInfo]accessBusiness.Level{
			maya: accessBusiness.LevelView, dex: accessBusiness.LevelNone, bot: accessBusiness.LevelNone, outsider: accessBusiness.LevelNone,
		} {
			d, err := accessBusiness.Rules.Resolve(ctx, accessBusiness.Subject{UserUID: who.UserDgraphInfo.Uid, UserUUID: who.UserDgraphInfo.Uuid},
				accessBusiness.Resource{Kind: calendarBusiness.KindCalendar, UUID: samCal})
			if err != nil || d.Level != want {
				t.Errorf("%s on Sam's calendar: level %v (%v), want %v", who.UserDgraphInfo.UserName, d.Level, err, want)
			}
		}
	})
	t.Run("a member is shared with, laid over and fed", func(t *testing.T) {
		if err := events.Share(ctx, sam, samCal, maya.UserDgraphInfo.Uuid, models.ShareDetails); err != nil {
			t.Fatalf("Sam's share with Maya: %v", err)
		}
		if got, err := events.PersonCalendar(ctx, maya, sam.UserDgraphInfo.Uuid); err != nil || got.String() != samCal {
			t.Errorf("Sam's calendar for Maya: %s %v", got, err)
		}
		if _, err := events.FeedICS(ctx, feedOf(maya, samCal)); err != nil {
			t.Errorf("Maya's feed of Sam's calendar: %v", err)
		}
	})
}
