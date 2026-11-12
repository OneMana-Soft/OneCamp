package business

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

func ringPerson(id string) *dgraphStruct.DgraphUser {
	return &dgraphStruct.DgraphUser{Uuid: id, UserName: id}
}

// ringIDs is who was rung, each with ":quiet" when rung without a tone. Pure.
func ringIDs(rs []rung) string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		if r.Quiet {
			out = append(out, r.UUID+":quiet")
		} else {
			out = append(out, r.UUID)
		}
	}
	return strings.Join(out, ",")
}

// slack-06 H2: a call rings the conversation's people but the caller; never a
// bot (V-126), an external guest or someone deactivated; a group of at most
// RingGroupMax people, and nobody in a bigger one; quietly for whoever the
// conversation's push doesn't reach now.
func TestWhoACallRings(t *testing.T) {
	gone := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	bot := ringPerson("bot")
	bot.IsBot, bot.IsExternal = true, true
	guest := ringPerson("guest")
	guest.IsExternal = true
	left := ringPerson("left")
	left.DeletedAt = &gone
	live := ringPerson("live")
	live.DeletedAt = &time.Time{} // a live person's zero deletion time
	helper := ringPerson("helper")
	helper.IsBot = true // a bot that isn't external by class

	group := func(n int) []*dgraphStruct.DgraphUser {
		out := []*dgraphStruct.DgraphUser{ringPerson("maya")}
		for i := 1; i < n; i++ {
			out = append(out, ringPerson(fmt.Sprintf("p%d", i)))
		}
		return out
	}
	everyone := func(people []*dgraphStruct.DgraphUser) []string {
		var out []string
		for _, p := range people {
			out = append(out, p.Uuid)
		}
		return out
	}

	for _, c := range []struct {
		name   string
		people []*dgraphStruct.DgraphUser
		caller string
		loud   []string
		want   string
	}{
		{"a DM rings the other person", []*dgraphStruct.DgraphUser{ringPerson("maya"), ringPerson("jonas")}, "maya", []string{"jonas"}, "jonas"},
		{"the caller is never rung, whatever their id's case", []*dgraphStruct.DgraphUser{ringPerson("MAYA"), ringPerson("jonas")}, "maya", []string{"maya", "jonas"}, "jonas"},
		{"a bot, a guest and someone deactivated are never rung", []*dgraphStruct.DgraphUser{ringPerson("maya"), bot, guest, left, live, nil, ringPerson("ana")}, "maya", []string{"bot", "guest", "left", "live", "ana"}, "live,ana"},
		{"a DM with a bot rings nobody", []*dgraphStruct.DgraphUser{ringPerson("maya"), bot}, "maya", []string{"bot"}, ""},
		{"a muted or paused person is rung quietly", []*dgraphStruct.DgraphUser{ringPerson("maya"), ringPerson("jonas"), ringPerson("ana")}, "maya", []string{"ANA"}, "jonas:quiet,ana"},
		{"nobody the push reaches: every one quietly", []*dgraphStruct.DgraphUser{ringPerson("maya"), ringPerson("jonas")}, "maya", nil, "jonas:quiet"},
		{"a group of 8 rings its 7 others", group(8), "maya", everyone(group(8)), "p1,p2,p3,p4,p5,p6,p7"},
		{"a group of 9 rings nobody", group(9), "maya", everyone(group(9)), ""},
		{"a bot isn't one of a group's people: 8 and a bot ring the 7, never the bot", append(group(8), helper), "maya", everyone(group(8)), "p1,p2,p3,p4,p5,p6,p7"},
		{"a guest is: 8 and a guest ring nobody", append(group(8), guest), "maya", everyone(group(8)), ""},
		{"no people, nobody", nil, "maya", nil, ""},
	} {
		if got := ringIDs(whoRings(c.people, c.caller, c.loud)); got != c.want {
			t.Errorf("%s: rang %q, want %q", c.name, got, c.want)
		}
	}
	if RingGroupMax != 8 || RingFor != 30*time.Second {
		t.Errorf("a group of at most %d rings for %s; want 8 and 30s", RingGroupMax, RingFor)
	}
}

// A join stops the ring for whoever joined, and the end for everyone rung: in
// a DM's or a group's call only. Its start rings (chatCallStarted).
func TestWhatStopsACallRinging(t *testing.T) {
	for _, c := range []struct {
		kind     resourceEventBusiness.Kind
		resource string
		want     bool
	}{
		{resourceEventBusiness.CallJoined, "dm", true},
		{resourceEventBusiness.CallEnded, "group", true},
		{resourceEventBusiness.CallJoined, "channel", false},
		{resourceEventBusiness.CallEnded, "meeting", false},
		{resourceEventBusiness.CallStarted, "dm", false},
		{resourceEventBusiness.CallLeft, "dm", false},
	} {
		if got := chatCallRingStops(resourceEventBusiness.Event{Kind: c.kind, ResourceKind: c.resource}); got != c.want {
			t.Errorf("%s in a %s's call stops the ring: %v, want %v", c.kind, c.resource, got, c.want)
		}
	}
}

// Who a call rings carries what its starter's "ringing" names them by: the
// name people see and their picture.
func TestWhoACallRingsIsNamed(t *testing.T) {
	jonas := ringPerson("jonas")
	jonas.UserFullName = "Jonas Berg"
	key := "profiles/jonas.png"
	jonas.ProfileKey = &key
	got := whoRings([]*dgraphStruct.DgraphUser{ringPerson("maya"), jonas}, "maya", nil)
	if len(got) != 1 || got[0].Name != jonas.DisplayName() || got[0].Profile != key || !got[0].Quiet {
		t.Errorf("Jonas rung as %+v, want named %q with picture %q, quietly", got, jonas.DisplayName(), key)
	}
}

// Someone in a call may ring its people again, but never while its last ring
// still rings, and at most RingAgainMax times.
func TestRingingAgainHasLimits(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name string
		st   ringState
		want error
	}{
		{"the first ring is over", ringState{Until: now.Add(-time.Second)}, nil},
		{"its last ring ends just now", ringState{Until: now}, nil},
		{"the first ring still rings", ringState{Until: now.Add(time.Second)}, ErrStillRinging},
		{"a ring again still rings", ringState{Until: now.Add(20 * time.Second), Again: 1}, ErrStillRinging},
		{"rung again twice, over", ringState{Until: now.Add(-time.Minute), Again: 2}, nil},
		{"rung again three times", ringState{Until: now.Add(-time.Minute), Again: 3}, ErrRingLimit},
		{"rung again three times, still ringing: the limit is the answer", ringState{Until: now.Add(time.Second), Again: 3}, ErrRingLimit},
	} {
		if got := mayRingAgain(c.st, now); !errors.Is(got, c.want) || (c.want == nil && got != nil) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	if RingAgainMax != 3 {
		t.Errorf("rung again at most %d times, want 3", RingAgainMax)
	}
}
