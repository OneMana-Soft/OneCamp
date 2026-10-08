//go:build integration

package business

// Check-ins against Postgres 12 with every migration: a check-in is born with
// its next time; a sweep asks a due one once and moves it on; two workers
// sweeping at once ask it once; a time hours past (workers down) is moved on,
// not asked late; paused, deleted or archived-channel check-ins ask nothing;
// "Ask now" won't ask twice in a minute; and who may see and change them.
// Run: go test -tags=integration ./business/CheckIn/ -run TestCheckIns -v

import (
	"context"
	"sync"
	"testing"
	"time"

	botpost "github.com/akashc777/OneCamp/business/BotPost"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/CheckIn"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestCheckIns(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	channel, maya, bot := uuid.New(), uuid.New(), uuid.New()
	var mu sync.Mutex
	moderator, member, archived := 1, 1, false
	channelInfo = func(_ context.Context, _ string, _ string) (*dgraphStruct.DgraphChannel, error) {
		mu.Lock()
		defer mu.Unlock()
		ch := &dgraphStruct.DgraphChannel{Uuid: channel.String(), Name: "standup", IsMember: uint8(member), IsAdmin: uint8(moderator),
			Members: []*dgraphStruct.DgraphUser{{Uuid: maya.String()}, {Uuid: bot.String(), IsBot: true}}}
		if archived {
			gone := time.Now()
			ch.DeletedAt = &gone
		}
		return ch, nil
	}
	var posted []string
	postAs = func(_ context.Context, _ uuid.UUID, text string) (*botpost.Result, error) {
		mu.Lock()
		defer mu.Unlock()
		posted = append(posted, text)
		return &botpost.Result{PostUUID: uuid.NewString()}, nil
	}
	var told [][]string
	notify = func(_, _, _, _, _ string, people []string) {
		mu.Lock()
		defer mu.Unlock()
		told = append(told, people)
	}
	asks := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(posted)
	}

	user := &userModels.UserInfo{}
	user.UserPostgresInfo.Id = maya
	r := Reader{User: user}
	day := func(d, h, m int) time.Time { return time.Date(2026, 10, d, h, m, 0, 0, time.UTC) }

	// Born with its next time: today's 17:00 (Tuesday 6 Oct, from 10:00).
	in := Input{Question: "What did you work on today?", Days: []int{1, 2, 3, 4, 5}, Time: "17:00", TZ: "UTC"}
	v, err := Create(ctx, r, channel, in, day(6, 10, 0))
	if err != nil || v.NextRunAt == nil || !v.NextRunAt.Equal(day(6, 17, 0)) {
		t.Fatalf("created with today's 17:00: %+v %v", v, err)
	}
	id := uuid.MustParse(v.Id)

	// Moved to 18:00, its next time follows; nothing is due at 17:30.
	in.Time = "18:00"
	if v, err = Edit(ctx, r, id, in, day(6, 10, 0)); err != nil || !v.NextRunAt.Equal(day(6, 18, 0)) {
		t.Fatalf("edited to 18:00: %+v %v", v, err)
	}
	Sweep(ctx, day(6, 17, 30))
	if asks() != 0 {
		t.Fatalf("asked before its time")
	}

	// Due: asked once, the people told (not the bots), Wednesday's held.
	Sweep(ctx, day(6, 18, 0).Add(5*time.Second))
	if asks() != 1 || len(told) != 1 || len(told[0]) != 1 || told[0][0] != maya.String() {
		t.Fatalf("asked once and told the people: %d asks, %+v", asks(), told)
	}
	if c, _ := model.Get(id); c.LastPostUUID == nil || !c.NextRunAt.Equal(day(7, 18, 0)) {
		t.Fatalf("after asking, Wednesday's is held: %+v", c.NextRunAt)
	}

	// Two workers sweeping Wednesday's at once: one asks.
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			Sweep(ctx, day(7, 18, 0).Add(time.Second))
		}()
	}
	wg.Wait()
	if asks() != 2 {
		t.Fatalf("four workers sweeping at once asked %d times, want once", asks()-1)
	}

	// The workers were down: Thursday's 18:00 swept at 21:00 is moved on, not asked late.
	Sweep(ctx, day(8, 21, 0))
	if c, _ := model.Get(id); asks() != 2 || !c.NextRunAt.Equal(day(9, 18, 0)) {
		t.Fatalf("a time 3 hours past was asked or not moved on: %d asks, next %v", asks(), c.NextRunAt)
	}

	// Paused, nothing is due; resumed, the next time after now is held.
	if v, err = SetPaused(ctx, r, id, true, day(9, 10, 0)); err != nil || !v.Paused || v.NextRunAt != nil {
		t.Fatalf("paused: %+v %v", v, err)
	}
	Sweep(ctx, day(9, 18, 1))
	if asks() != 2 {
		t.Fatalf("a paused check-in asked")
	}
	if v, err = SetPaused(ctx, r, id, false, day(9, 19, 0)); err != nil || !v.NextRunAt.Equal(day(12, 18, 0)) {
		t.Fatalf("resumed on Friday evening, Monday's: %+v %v", v, err)
	}

	// Ask now asks at once (the sweep's ask was a moment ago, so two minutes
	// on), but not twice within a minute.
	later := time.Now().Add(2 * time.Minute)
	if _, err := AskNow(ctx, r, id, later); err != nil || asks() != 3 {
		t.Fatalf("ask now: %d asks, %v", asks(), err)
	}
	if _, err := AskNow(ctx, r, id, later); err == nil || asks() != 3 {
		t.Fatalf("asked twice in a minute: %d asks", asks())
	}

	// A member sees but can't change; someone not in the channel sees none, no error.
	moderator = 0
	if list, canEdit, err := List(ctx, r, channel); err != nil || canEdit || len(list) != 1 {
		t.Fatalf("a member sees, can't change: %d %v %v", len(list), canEdit, err)
	}
	if _, err := Edit(ctx, r, id, in, day(9, 10, 0)); err != ErrNotAllowed {
		t.Fatalf("a member edited a check-in: %v", err)
	}
	member = 0
	if list, canEdit, err := List(ctx, r, channel); err != nil || canEdit || len(list) != 0 {
		t.Fatalf("someone not in the channel: %d %v %v", len(list), canEdit, err)
	}
	moderator, member = 1, 1

	// The channel archived: its check-in pauses itself instead of failing each time.
	archived = true
	Sweep(ctx, day(12, 18, 0).Add(time.Second))
	if c, _ := model.Get(id); asks() != 3 || !c.Paused {
		t.Fatalf("an archived channel's check-in asked or kept running: %d asks, paused %v", asks(), c.Paused)
	}
	archived = false

	// Deleted, it asks nothing more.
	if _, err := SetPaused(ctx, r, id, false, day(12, 19, 0)); err != nil {
		t.Fatal(err)
	}
	if err := Delete(ctx, r, id); err != nil {
		t.Fatal(err)
	}
	Sweep(ctx, day(13, 18, 0).Add(time.Second))
	if asks() != 3 {
		t.Fatalf("a deleted check-in asked")
	}
}
