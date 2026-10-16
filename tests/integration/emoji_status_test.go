//go:build integration
// +build integration

package integration_test

// A person's status, against a real Dgraph and the profile cache (Redis).
//
//   - The status set last is the one in force. Set "Vacationing" for the week,
//     then "Lunch" for 30 minutes: your profile read again (through the cache
//     every request reads), the sidebar, and someone who has a DM with you all
//     say Lunch. Every read took the status that ENDS last, so a new status
//     gave way to last week's on the next load, for you and everyone.
//   - Clear brings nothing back. It ended only the status ending last, and an
//     earlier one still in force took its place.
//   - A status whose time has passed is not in force.
//   - Two statuses an older server left in force give way to the next one set.
//   - A person keeps five statuses, the newest.
//
// Run: go test -tags=integration ./tests/integration/ -run TestAStatus -v

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	adapterUser "github.com/akashc777/OneCamp/adapter/User"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	domain "github.com/akashc777/OneCamp/domain/User"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestAStatusSetLastIsTheOneInForce(t *testing.T) {
	ctx := context.Background()
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	integration.SetupRedis(t)

	// A person, and someone with a DM with them, who sees their status in
	// the DM list. Extra fields seed what the person already has.
	person := func(t *testing.T, extra map[string]any) (me, them string) {
		t.Helper()
		me, them = uuid.NewString(), uuid.NewString()
		mine := map[string]any{"uid": "_:me", "dgraph.type": "User", "user_uuid": me, "user_name": "me-" + me[:8], "user_full_name": "Maya"}
		for k, v := range extra {
			mine[k] = v
		}
		dg.Mutate(t, []map[string]any{
			mine,
			{"uid": "_:them", "dgraph.type": "User", "user_uuid": them, "user_name": "them-" + them[:8], "user_full_name": "Jonas",
				"user_dms": []map[string]any{{"uid": "_:dm"}}},
			{"uid": "_:dm", "dgraph.type": "Dm", "dm_grouping_id": me + them,
				"dm_participants": []map[string]any{{"uid": "_:me"}, {"uid": "_:them"}}},
		})
		return me, them
	}
	set := func(t *testing.T, me, emoji, words, clears string) {
		t.Helper()
		in := &adapterUser.UserEmojiStatusInput{EmojiUuid: emoji, StatusDesc: words, ExpiryTimeIn: clears, TimeZone: "Asia/Kolkata"}
		if err := userBusiness.UpdateUserEmojiStatus(ctx, &dgraphStruct.DgraphUser{Uuid: me}, in); err != nil {
			t.Fatalf("setting %q: %v", words, err)
		}
	}
	// What each reader is told, as the words of the status in force ("" for none).
	type told struct{ own, sidebar, theirDMs string }
	words := func(s []*dgraphStruct.DgraphUserStatusEmoji) string {
		if len(s) == 0 {
			return ""
		}
		return s[0].EmojiDesc
	}
	read := func(t *testing.T, me, them string) told {
		t.Helper()
		own, err := domain.GetActiveDgraphUserInfoByUUID(ctx, me)
		if err != nil {
			t.Fatalf("reading my profile: %v", err)
		}
		bar, err := domain.GetDgraphUserInfoByUUIDForSidebarNav(ctx, me)
		if err != nil {
			t.Fatalf("reading my sidebar: %v", err)
		}
		theirs, err := domain.GetDgraphUserInfoByUUIDForSidebarNav(ctx, them)
		if err != nil {
			t.Fatalf("reading their sidebar: %v", err)
		}
		seen := ""
		for _, dm := range theirs.DMs {
			for _, p := range dm.Participants {
				if p.Uuid == me {
					seen = words(p.StatusEmoji)
				}
			}
		}
		return told{own: words(own.StatusEmoji), sidebar: words(bar.StatusEmoji), theirDMs: seen}
	}
	everyone := func(w string) told { return told{own: w, sidebar: w, theirDMs: w} }

	t.Run("the status set last is the one in force, for you and for others", func(t *testing.T) {
		me, them := person(t, nil)
		set(t, me, "palm_tree", "Vacationing", "this_week")
		// The profile every request reads is cached now, with Vacationing.
		if got := read(t, me, them); got != everyone("Vacationing") {
			t.Fatalf("after Vacationing: %+v", got)
		}
		set(t, me, "sandwich", "Lunch", "30m")
		if got := read(t, me, them); got != everyone("Lunch") {
			t.Errorf("after Lunch for 30 minutes: %+v; want Lunch everywhere", got)
		}
		// The menu's list: newest first, and the week's status has ended.
		list, err := domain.GetAllUserEmojiStatusList(ctx, me)
		if err != nil || len(list.StatusEmoji) != 2 {
			t.Fatalf("the list: %v, %v", list, err)
		}
		if list.StatusEmoji[0].EmojiDesc != "Lunch" || !list.StatusEmoji[1].ExpiryAt.Before(time.Now().Add(time.Second)) {
			t.Errorf("the list holds %s then %s ending %v; want Lunch first, and Vacationing ended", list.StatusEmoji[0].EmojiDesc, list.StatusEmoji[1].EmojiDesc, list.StatusEmoji[1].ExpiryAt)
		}
	})

	t.Run("Clear brings nothing back", func(t *testing.T) {
		me, them := person(t, nil)
		set(t, me, "palm_tree", "Vacationing", "this_week")
		set(t, me, "spiral_calendar_pad", "In a meeting", "1h")
		if got := read(t, me, them); got != everyone("In a meeting") {
			t.Fatalf("before clearing: %+v", got)
		}
		if err := userBusiness.ClearUserEmojiStatus(ctx, me); err != nil {
			t.Fatal(err)
		}
		if got := read(t, me, them); got != everyone("") {
			t.Errorf("after Clear: %+v; want no status anywhere", got)
		}
		// Clearing with nothing in force is clear already.
		if err := userBusiness.ClearUserEmojiStatus(ctx, me); err != nil {
			t.Errorf("clearing again: %v", err)
		}
	})

	t.Run("a status whose time has passed is not in force", func(t *testing.T) {
		ended := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
		me, them := person(t, map[string]any{"user_emoji_statuses": []map[string]any{{
			"uid": "_:lunch", "dgraph.type": "Status", "status_user_emoji_id": "sandwich", "status_user_emoji_desc": "Lunch",
			"status_user_emoji_expiry_at": ended, "status_user_emoji_expiry_in": "30m", "status_user_emoji_set_at": ended,
		}}})
		if got := read(t, me, them); got != everyone("") {
			t.Errorf("an ended status: %+v; want none in force", got)
		}
	})

	t.Run("two left in force by an older server give way to the next one set", func(t *testing.T) {
		week := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
		hour := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		// No set time: written before set times were kept.
		me, them := person(t, map[string]any{"user_emoji_statuses": []map[string]any{
			{"uid": "_:away", "dgraph.type": "Status", "status_user_emoji_id": "palm_tree", "status_user_emoji_desc": "Away", "status_user_emoji_expiry_at": week, "status_user_emoji_expiry_in": "this_week"},
			{"uid": "_:call", "dgraph.type": "Status", "status_user_emoji_id": "telephone_receiver", "status_user_emoji_desc": "In a call", "status_user_emoji_expiry_at": hour, "status_user_emoji_expiry_in": "1h"},
		}})
		// One of the two is shown, the same to everyone, until the next set.
		if got := read(t, me, them); got.own == "" || got != everyone(got.own) {
			t.Fatalf("an older server's two: %+v; want one of them, alike everywhere", got)
		}
		set(t, me, "brain", "Deep work", "4h")
		if got := read(t, me, them); got != everyone("Deep work") {
			t.Errorf("after Deep work: %+v", got)
		}
		inForce := 0
		var res struct {
			Q []struct {
				S []struct {
					Ends time.Time `json:"status_user_emoji_expiry_at"`
				} `json:"user_emoji_statuses"`
			} `json:"q"`
		}
		if err := json.Unmarshal(dg.Query(t, `{ q(func: eq(user_uuid, "`+me+`")) { user_emoji_statuses { status_user_emoji_expiry_at } } }`), &res); err != nil || len(res.Q) != 1 {
			t.Fatalf("reading the statuses: %v", err)
		}
		for _, s := range res.Q[0].S {
			if s.Ends.After(time.Now()) {
				inForce++
			}
		}
		if inForce != 1 {
			t.Errorf("%d statuses in force; want 1", inForce)
		}
	})

	t.Run("a person keeps the five newest", func(t *testing.T) {
		mark := uuid.NewString()[:8]
		me, _ := person(t, map[string]any{"user_emoji_statuses": []map[string]any{
			{"uid": "_:old", "dgraph.type": "Status", "status_user_emoji_id": "mask", "status_user_emoji_desc": mark + " Sick", "status_user_emoji_expiry_at": "2026-09-01T00:00:00Z", "status_user_emoji_expiry_in": "today"},
		}})
		for _, w := range []string{"One", "Two", "Three", "Four", "Five", "Six"} {
			set(t, me, "sandwich", mark+" "+w, "30m")
		}
		var res struct {
			Q []struct {
				S []struct {
					Words string `json:"status_user_emoji_desc"`
				} `json:"user_emoji_statuses"`
			} `json:"q"`
		}
		q := `{ q(func: eq(user_uuid, "` + me + `")) { user_emoji_statuses (orderdesc: status_user_emoji_set_at) { status_user_emoji_desc } } }`
		if err := json.Unmarshal(dg.Query(t, q), &res); err != nil || len(res.Q) != 1 {
			t.Fatalf("reading the statuses: %v", err)
		}
		var kept []string
		for _, s := range res.Q[0].S {
			kept = append(kept, s.Words)
		}
		if want := []string{mark + " Six", mark + " Five", mark + " Four", mark + " Three", mark + " Two"}; len(kept) != len(want) || kept[0] != want[0] || kept[4] != want[4] {
			t.Errorf("kept %v; want %v", kept, want)
		}
		// The ones beyond five are gone from the graph, not only from the person.
		var left struct {
			Q []struct {
				Words string `json:"status_user_emoji_desc"`
			} `json:"q"`
		}
		if err := json.Unmarshal(dg.Query(t, `{ q(func: type(Status)) { status_user_emoji_desc } }`), &left); err != nil {
			t.Fatal(err)
		}
		for _, s := range left.Q {
			if s.Words == mark+" One" || s.Words == mark+" Sick" {
				t.Errorf("%q is still in the graph", s.Words)
			}
		}
	})
}
