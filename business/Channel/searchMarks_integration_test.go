//go:build integration

package business

// A channel's history in search follows its archive, in the order it was
// archived and restored, against a real Postgres and Dgraph.
//
// Every update that wasn't an archive (a rename, a privacy change, the Slack
// import's tidy step) cleared the archived mark over all of the channel's
// posts, replies and files, a settled update-by-query each time, and brought
// back what an older archive had marked deleted, though the channel had never
// been archived. And an archive and a restore each started their mark on a
// goroutine of its own, so a restore whose unmark finished before the
// archive's mark left a live channel's whole history marked Archived.
//
// Run: go test -tags=integration ./business/Channel/ -run TestAChannelsSearchMarksFollowItsArchive -v

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Channel"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

// markStep is one mark asked of search: whether it marked or cleared, and
// whether it is the mark starting or ending.
type markStep struct {
	archived, ended bool
}

func TestAChannelsSearchMarksFollowItsArchive(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	mod := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at)
		VALUES ($1, 'mod@example.test', 'mod', 'mod', NOW(), NOW())`, mod); err != nil {
		t.Fatal(err)
	}
	launch := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO channels (id, ch_name, ch_private, created_by) VALUES ($1, 'launch', false, $2)`, launch, mod); err != nil {
		t.Fatal(err)
	}
	dg.Mutate(t, []map[string]any{
		{"uid": "_:mod", "dgraph.type": "User", "user_uuid": mod.String(), "user_name": "mod",
			"user_channels": []map[string]any{{"uid": "_:launch"}}},
		{"uid": "_:launch", "dgraph.type": "Channel", "ch_uuid": launch.String(), "ch_name": "launch",
			"ch_private": false, "ch_deleted_at": "0001-01-01T00:00:00Z",
			"ch_members":    []map[string]any{{"uid": "_:mod"}},
			"ch_moderators": []map[string]any{{"uid": "_:mod"}}},
	})

	// Every mark asked of search, as it starts and as it ends. The first
	// archive's mark holds until let go, as it does on a channel with a long
	// history.
	var (
		mu      sync.Mutex
		steps   []markStep
		started = make(chan bool, 16)
		letGo   = make(chan struct{})
		held    sync.Once
	)
	seen := func() []markStep {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(steps)
	}
	forget := func() {
		mu.Lock()
		defer mu.Unlock()
		steps = nil
		for len(started) > 0 {
			<-started
		}
	}
	before := markInSearch
	t.Cleanup(func() {
		helpers.SettleBackground(time.Minute)
		markInSearch = before
	})
	markInSearch = func(channelUUID string, archived bool) {
		if channelUUID != launch.String() {
			t.Errorf("a mark for %s, not #launch", channelUUID)
		}
		mu.Lock()
		steps = append(steps, markStep{archived: archived})
		mu.Unlock()
		started <- archived
		if archived {
			held.Do(func() { <-letGo })
		}
		mu.Lock()
		steps = append(steps, markStep{archived: archived, ended: true})
		mu.Unlock()
	}

	// Settle waits for work started with GoSafe. noMarkStarts also gives a
	// mark started any other way, as these were, the time to show itself:
	// with the change, none is ever started, so the wait only ever runs out.
	const grace = 2 * time.Second
	noMarkStarts := func(after string) {
		t.Helper()
		integration.Settle(t)
		select {
		case archived := <-started:
			t.Errorf("%s: search was asked to mark #launch's history (archived %v); want no mark", after, archived)
		case <-time.After(grace):
		}
	}
	update := func(what string, change adapter.UpdateChannelInfo) {
		t.Helper()
		change.ChannelUuid = launch.String()
		if err := UpdateChannelInfo(ctx, &change, launch); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}

	// A live channel renamed: its history was never marked, so there is
	// nothing to clear.
	update("renaming the live #launch", adapter.UpdateChannelInfo{ChannelName: "launch-room"})
	noMarkStarts("renaming the live #launch")
	if got := seen(); len(got) != 0 {
		t.Errorf("renaming the live #launch asked search for %+v; want nothing", got)
	}
	forget()

	// Archived, and restored while the archive's mark is still running: the
	// restore's unmark waits its turn, and the history ends unmarked.
	update("archiving #launch", adapter.UpdateChannelInfo{ChannelName: "launch-room", ChannelArchived: true})
	select {
	case archived := <-started:
		if !archived {
			t.Fatal("archiving #launch cleared its history's mark")
		}
	case <-time.After(time.Minute):
		t.Fatal("archiving #launch never marked its history")
	}
	update("restoring #launch", adapter.UpdateChannelInfo{ChannelName: "launch-room"})
	select {
	case <-started:
		t.Error("the restore's unmark started while the archive's mark was still running")
	case <-time.After(grace):
	}
	close(letGo)
	deadline := time.Now().Add(time.Minute)
	for len(seen()) < 4 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	want := []markStep{{archived: true}, {archived: true, ended: true}, {archived: false}, {archived: false, ended: true}}
	if got := seen(); !slices.Equal(got, want) {
		t.Errorf("archived then restored, search was asked for %+v; want the mark, then the unmark after it", got)
	}
	forget()

	// Live again, renamed: nothing more.
	update("renaming #launch after its restore", adapter.UpdateChannelInfo{ChannelName: "launch-again"})
	noMarkStarts("renaming #launch after its restore")
	if got := seen(); len(got) != 0 {
		t.Errorf("renaming the restored #launch asked search for %+v; want nothing", got)
	}
}
