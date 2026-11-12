package business

import (
	"context"
	"errors"
	"testing"
	"time"

	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// Opening the Inbox answers the badge after the write and tells the person's
// own activity topic (read.moved, kind "inbox"), so every tab and device they
// have open reads its badge again (C6). Nobody else's topic hears of it.
func TestOpeningTheInboxTellsTheirOwnTabsAndDevices(t *testing.T) {
	savedSeen, savedUnread, savedPublish := markInboxSeen, inboxUnread, publish
	t.Cleanup(func() { markInboxSeen, inboxUnread, publish = savedSeen, savedUnread, savedPublish })

	me := uuid.New()
	who := &userModels.UserInfo{}
	who.UserPostgresInfo.Id = me
	who.UserDgraphInfo.Uuid, who.UserDgraphInfo.Uid = me.String(), "0x7"

	type told struct {
		topic string
		typ   int8
		ev    MovedEvent
	}
	var events []told
	publish = func(user string, typ int8, data interface{}) {
		ev, _ := data.(MovedEvent)
		events = append(events, told{user, typ, ev})
	}
	var wrote []uuid.UUID
	var failWrite, failCount bool
	markInboxSeen = func(_ context.Context, user uuid.UUID) error {
		if failWrite {
			return errors.New("postgres is down")
		}
		wrote = append(wrote, user)
		return nil
	}
	inboxUnread = func(_ context.Context, user uuid.UUID, uid string) (uint64, error) {
		if failCount {
			return 0, errors.New("dgraph is down")
		}
		if len(wrote) == 0 || user != me || uid != "0x7" {
			t.Errorf("the badge was counted for %s/%s before the last look was written (%d writes)", user, uid, len(wrote))
		}
		return 1, nil // something arrived since
	}

	n, err := InboxSeen(context.Background(), who)
	if err != nil || n == nil || *n != 1 {
		t.Fatalf("opening the Inbox answered %v, %v; want the badge after the write, 1", n, err)
	}
	if len(events) != 1 {
		t.Fatalf("opening the Inbox told %d events; want one read.moved", len(events))
	}
	ev := events[0]
	seen, perr := time.Parse(time.RFC3339Nano, ev.ev.Seen)
	if ev.topic != me.String() || ev.typ != mqttStruct.MESSAGE_READ_MOVED || ev.ev.Kind != KindInbox || ev.ev.Unread != 1 || perr != nil || time.Since(seen) > time.Minute {
		t.Errorf("opening the Inbox told %+v; want read.moved kind inbox, unread 1 and when, on their own topic %s", ev, me)
	}

	// A count that can't be read answers no number (never a 0 the server
	// didn't count), and still tells their tabs to read it again.
	events, failCount = nil, true
	n, err = InboxSeen(context.Background(), who)
	if err != nil || n != nil || len(events) != 1 || events[0].ev.Kind != KindInbox {
		t.Errorf("with the count failing, opening the Inbox answered %v, %v and told %+v; want no number, no error, one inbox event", n, err, events)
	}

	// A last look that can't be written is the error, and tells nobody.
	events, failCount, failWrite = nil, false, true
	if n, err = InboxSeen(context.Background(), who); err == nil || n != nil || len(events) != 0 {
		t.Errorf("with the write failing, opening the Inbox answered %v, %v and told %d events; want the error and nothing told", n, err, len(events))
	}
}
