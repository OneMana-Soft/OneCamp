package business

import (
	"context"
	"testing"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/AI"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// Opening a conversation moves its last-seen to now, and "Catch me up" read
// that same last-seen afterwards: it found nothing and the banner went for an
// hour (SA-BUG-7). Given how many were unread as the reader opened it, the
// recap starts just before the oldest of them, never before the lookback.
func TestCatchUpStartsAtTheUnreadTheReaderSaw(t *testing.T) {
	ch := uuid.NewString()
	grp := "0123456789abcdef0123456789abcdef"
	user := &userModels.UserInfo{}
	user.UserPostgresInfo.Id = uuid.New()
	user.UserDgraphInfo = dgraphStruct.DgraphUser{
		Channels: []*dgraphStruct.DgraphChannel{{Uuid: ch, Name: "launch"}},
		DMs:      []*dgraphStruct.DgraphDm{{GroupingId: grp}},
	}
	now := time.Now()
	oldestUnread := now.Add(-2 * time.Hour)
	asked := 0
	old := []any{lastSeenInChannel, lastSeenInChat, unreadBoundaryChannel, unreadBoundaryChat}
	t.Cleanup(func() {
		lastSeenInChannel = old[0].(func(context.Context, uuid.UUID, uuid.UUID) (time.Time, bool, error))
		lastSeenInChat = old[1].(func(context.Context, uuid.UUID, string) (time.Time, bool, error))
		unreadBoundaryChannel = old[2].(func(context.Context, uuid.UUID, uuid.UUID, int) (time.Time, bool, error))
		unreadBoundaryChat = old[3].(func(context.Context, uuid.UUID, string, int) (time.Time, bool, error))
	})
	// The open just moved last-seen to now.
	lastSeenInChannel = func(context.Context, uuid.UUID, uuid.UUID) (time.Time, bool, error) { return now, true, nil }
	lastSeenInChat = func(context.Context, uuid.UUID, string) (time.Time, bool, error) { return now, true, nil }
	unreadBoundaryChannel = func(_ context.Context, _ uuid.UUID, _ uuid.UUID, n int) (time.Time, bool, error) {
		asked = n
		return oldestUnread, true, nil
	}
	unreadBoundaryChat = func(_ context.Context, _ uuid.UUID, _ string, n int) (time.Time, bool, error) {
		asked = n
		return oldestUnread, true, nil
	}

	since, _, _, _, err := catchUpBoundary(context.Background(), user, adapter.CatchUpRequest{ScopeType: "channel", ChannelUUID: ch, Unread: 12})
	if err != nil {
		t.Fatal(err)
	}
	if asked != 12 || !since.Equal(oldestUnread.Add(-time.Second)) {
		t.Errorf("12 unread on opening: asked for %d, recap starts %v; want 12 and a second before the oldest (%v)", asked, since, oldestUnread)
	}
	since, _, _, _, err = catchUpBoundary(context.Background(), user, adapter.CatchUpRequest{ScopeType: "chat", ChatGrpID: grp, Unread: 12})
	if err != nil || !since.Equal(oldestUnread.Add(-time.Second)) {
		t.Errorf("in a group, 12 unread: recap starts %v (%v); want a second before the oldest", since, err)
	}
	// Without the count, the last-seen the open moved: nothing to recap.
	if since, _, _, _, _ := catchUpBoundary(context.Background(), user, adapter.CatchUpRequest{ScopeType: "channel", ChannelUUID: ch}); !since.Equal(now) {
		t.Errorf("with no count the recap starts %v; want the last-seen, %v", since, now)
	}
	// Never past the lookback, however old the unread.
	oldestUnread = now.Add(-60 * 24 * time.Hour)
	since, _, _, _, _ = catchUpBoundary(context.Background(), user, adapter.CatchUpRequest{ScopeType: "channel", ChannelUUID: ch, Unread: 900})
	if since.Before(now.Add(-catchUpLookbackFloor - time.Minute)) {
		t.Errorf("a two-month-old unread pulled the recap back to %v, past the lookback", since)
	}
	// A count past the cap asks for the cap.
	_, _, _, _, _ = catchUpBoundary(context.Background(), user, adapter.CatchUpRequest{ScopeType: "channel", ChannelUUID: ch, Unread: 1 << 20})
	if asked != catchUpMaxUnread {
		t.Errorf("an unread count of a million asked for %d", asked)
	}
}
