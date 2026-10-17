package notification

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
	followModels "github.com/akashc777/OneCamp/models/postgres/Follow"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

type toldWays struct {
	mu                   sync.Mutex
	written, live        []string
	delivered, emailed   []string
	deliverData          map[string]string
	deliverID            string
	keys                 map[string]bool
	emailType            EventType
	liveTitle, liveActor string
	liveKind             string
	deactivated          map[string]bool
	writes               int
	activeErr            error
	liveOpen             bool
	emailSubtitle        string
}

func withTellSeams(t *testing.T, w *toldWays) {
	t.Helper()
	oldW, oldD, oldL, oldE, oldOn := noticeWrite, noticeDeliver, noticeLive, noticeEmail, noticesOn
	t.Cleanup(func() {
		noticeWrite, noticeDeliver, noticeLive, noticeEmail, noticesOn = oldW, oldD, oldL, oldE, oldOn
	})
	w.keys = map[string]bool{}
	noticesOn = func() bool { return true }
	oldActive := noticeActive
	t.Cleanup(func() { noticeActive = oldActive })
	// Everyone is active but those the test deactivates.
	noticeActive = func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]*userModels.User, error) {
		out := map[uuid.UUID]*userModels.User{}
		for _, id := range ids {
			if !w.deactivated[id.String()] {
				out[id] = &userModels.User{Id: id}
			}
		}
		return out, w.activeErr
	}
	noticeWrite = func(_ context.Context, rows []*followModels.Notice) (map[uuid.UUID]bool, error) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.writes++
		out := map[uuid.UUID]bool{}
		for _, n := range rows {
			if w.keys[n.DedupKey] {
				continue
			}
			w.keys[n.DedupKey] = true
			w.written = append(w.written, n.UserUUID.String())
			out[n.UserUUID] = true
		}
		return out, nil
	}
	noticeDeliver = func(_ context.Context, to []string, data map[string]string, id string) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.delivered = append(w.delivered, to...)
		w.deliverData, w.deliverID = data, id
	}
	noticeLive = func(user string, item *dgraphModels.UnifiedActivityItem) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.live = append(w.live, user)
		w.liveKind = item.ActorKind
		w.liveTitle, w.liveOpen = item.Notice.Title, item.Notice.CanOpen
		if item.Notice.Actor != nil {
			w.liveActor = item.Notice.Actor.Uuid
		}
	}
	noticeEmail = func(_ context.Context, e Event) int {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.emailType, w.emailSubtitle = e.Type, e.Subtitle
		for _, r := range e.Recipients {
			w.emailed = append(w.emailed, r.UserUUID.String())
		}
		return len(e.Recipients)
	}
}

const (
	akash = "11111111-1111-4111-8111-111111111111"
	sam   = "22222222-2222-4222-8222-222222222222"
	maya  = "33333333-3333-4333-8333-333333333333"
)

func mention() Notice {
	return Notice{Kind: "doc.mentioned", ResourceKind: "doc", ResourceUUID: "d1", ResourceTitle: "Launch notes",
		Actor: &dgraphStruct.DgraphUser{Uuid: akash, UserFullName: "Akash", EmailID: "akash@example.test"},
		Title: "Akash mentioned you in Launch notes", Path: "/app/doc/d1", Email: EventDocMention,
		DedupKey: "doc.mentioned|d1|2026-10-12"}
}

// Each person is told once, never the actor; only a notice that was written
// goes further; an inbox-only recipient is never pushed or emailed.
func TestTellingWritesEachNoticeOnceAndSendsOnlyWhatItWrote(t *testing.T) {
	w := &toldWays{}
	withTellSeams(t, w)
	told := Tell(context.Background(), mention(), []NoticeRecipient{
		{UserUUID: sam, Reason: "mention"},
		{UserUUID: sam, Reason: "mention"},                       // twice in the list
		{UserUUID: akash, Reason: "mention"},                     // the actor
		{UserUUID: maya, Reason: "participant", InboxOnly: true}, // inbox alone
		{UserUUID: "not-a-uuid", Reason: "mention"},
	})
	if !slices.Equal(told, []string{sam, maya}) {
		t.Fatalf("told %v, want sam and maya", told)
	}
	if !slices.Equal(w.live, []string{sam, maya}) || w.liveTitle != "Launch notes" || w.liveActor != akash {
		t.Errorf("live updates went to %v (title %q, actor %q)", w.live, w.liveTitle, w.liveActor)
	}
	if !slices.Equal(w.delivered, []string{sam}) || !slices.Equal(w.emailed, []string{sam}) || w.emailType != EventDocMention {
		t.Errorf("pushed to %v and emailed %v (%s); maya is inbox-only", w.delivered, w.emailed, w.emailType)
	}

	// The same event again: everyone was told, so nothing more is sent.
	again := Tell(context.Background(), mention(), []NoticeRecipient{{UserUUID: sam, Reason: "mention"}})
	if len(again) != 0 || len(w.delivered) != 1 || len(w.emailed) != 1 {
		t.Errorf("a repeated telling sent more: told %v, pushed to %v, emailed %v", again, w.delivered, w.emailed)
	}
}

// A notice is pushed as every push is (Deliver): to whom the push gate lets
// through, by person, each with a desktop notice named by the telling's key
// (TN-BUG-12), and to their devices. Someone the gate holds back, here maya
// (deactivated, say), is neither pushed nor noticed. Tell's own loop asked a
// lookup that let a deactivated person's phone through, and sent no desktop
// notice at all.
func TestANoticeIsPushedThroughThePushGateWithADesktopNotice(t *testing.T) {
	r := recordMqtt(t)
	pushes := watchPushes(t)
	w := &toldWays{}
	withTellSeams(t, w)
	var asked []string
	noticeDeliver = func(ctx context.Context, to []string, data map[string]string, id string) {
		if err := deliver(ctx, to, data, id, func(_ context.Context, ids []string) ([]string, []string, error) {
			asked = ids
			return []string{sam}, []string{"tok-sam"}, nil
		}); err != nil {
			t.Error(err)
		}
	}
	n := mention()
	Tell(context.Background(), n, []NoticeRecipient{{UserUUID: sam, Reason: "mention"}, {UserUUID: maya, Reason: "follower"}})
	if !slices.Equal(asked, []string{sam, maya}) {
		t.Errorf("asked the gate about %v, want sam and maya", asked)
	}
	got := r.notices(t, sam, maya)
	if len(got) != 1 || got[sam].NoticeID != n.DedupKey || got[sam].Data[firebaseInit.FIREBASE_PUSH_DATA_TITLE] != n.Title {
		t.Errorf("desktop notices %+v, want sam's alone, named %q and saying %q", got, n.DedupKey, n.Title)
	}
	if calls := pushes(); len(calls) != 1 || !slices.Equal(calls[0], []string{"tok-sam"}) {
		t.Errorf("pushes %v, want sam's device alone", calls)
	}
}

// A push whose gate can't be read tells nobody, and the email still goes (B4).
func TestAPushThatFailsLeavesTheEmail(t *testing.T) {
	r := recordMqtt(t)
	pushes := watchPushes(t)
	w := &toldWays{}
	withTellSeams(t, w)
	noticeDeliver = func(ctx context.Context, to []string, data map[string]string, id string) {
		if err := deliver(ctx, to, data, id, func(context.Context, []string) ([]string, []string, error) {
			return nil, nil, errors.New("postgres down")
		}); err == nil {
			t.Error("a gate that couldn't be read was no failure")
		}
	}
	Tell(context.Background(), mention(), []NoticeRecipient{{UserUUID: sam, Reason: "mention"}})
	if !slices.Equal(w.emailed, []string{sam}) {
		t.Errorf("emailed %v, want sam", w.emailed)
	}
	if got := r.notices(t, sam); len(got) != 0 || len(pushes()) != 0 {
		t.Errorf("noticed %v and pushed %v with a gate that couldn't be read", got, pushes())
	}
}

// With no database (a unit test elsewhere, a build tool), telling does nothing.
func TestWithoutADatabaseNothingIsTold(t *testing.T) {
	old := noticesOn
	noticesOn = func() bool { return false }
	defer func() { noticesOn = old }()
	if told := Tell(context.Background(), mention(), []NoticeRecipient{{UserUUID: sam}}); told != nil {
		t.Errorf("told %v with no database", told)
	}
}

// Someone whose feed lists the thing already is pushed and emailed, once, with
// a hidden row and no live inbox item.
func TestAListedRecipientIsToldWithoutASecondInboxItem(t *testing.T) {
	w := &toldWays{}
	withTellSeams(t, w)
	var reasons []string
	old := noticeWrite
	noticeWrite = func(ctx context.Context, rows []*followModels.Notice) (map[uuid.UUID]bool, error) {
		for _, n := range rows {
			reasons = append(reasons, n.Reason)
		}
		return old(ctx, rows)
	}
	Tell(context.Background(), mention(), []NoticeRecipient{{UserUUID: sam, Reason: "participant", Listed: true}})
	if len(w.live) != 0 || !slices.Equal(w.emailed, []string{sam}) || !slices.Equal(w.delivered, []string{sam}) {
		t.Errorf("live %v, emailed %v, pushed to %v", w.live, w.emailed, w.delivered)
	}
	if !slices.Equal(reasons, []string{followModels.ReasonListed}) {
		t.Errorf("the row's reason %v", reasons)
	}
	Tell(context.Background(), mention(), []NoticeRecipient{{UserUUID: sam, Reason: "participant", Listed: true}})
	if len(w.emailed) != 1 {
		t.Error("a listed recipient was told twice")
	}
}

// A notice's push opened a doc, whatever it was about: pushNotice hard-coded
// the doc type, so booking pages copied the push loop and a guest taken off
// an event wasn't pushed at all. A push target says where it opens.
func TestANoticePushOpensWhereItSays(t *testing.T) {
	w := &toldWays{}
	withTellSeams(t, w)
	n := mention()
	Tell(context.Background(), n, []NoticeRecipient{{UserUUID: sam, Reason: "mention"}})
	if w.deliverData[firebaseInit.FIREBASE_PUSH_DATA_TYPE] != firebaseInit.FIREBASE_PUSH_DATA_TYPE_DOC_COMMENT || w.deliverData[firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID] != "d1" {
		t.Errorf("a doc's push %v", w.deliverData)
	}
	if w.deliverID != n.DedupKey {
		t.Errorf("a doc's push is noticed as %q, want the telling's key %q", w.deliverID, n.DedupKey)
	}

	n = Notice{Kind: "booking.created", ResourceKind: "event", ResourceUUID: "e1", DedupKey: "b1",
		Title: "Alex Kim booked Intro call", Body: "Wednesday",
		Push: &Push{Type: firebaseInit.FIREBASE_PUSH_DATA_TYPE_CALENDAR, ID: "e1", Tag: "booking-b1", From: "Alex Kim"}}
	Tell(context.Background(), n, []NoticeRecipient{{UserUUID: maya, Reason: "owner"}})
	for k, want := range map[string]string{
		firebaseInit.FIREBASE_PUSH_DATA_TYPE:     firebaseInit.FIREBASE_PUSH_DATA_TYPE_CALENDAR,
		firebaseInit.FIREBASE_PUSH_DATA_TYPE_ID:  "e1",
		firebaseInit.FIREBASE_PUSH_DATA_TAG:      "booking-b1",
		firebaseInit.FIREBASE_PUSH_DATA_USERNAME: "Alex Kim",
		firebaseInit.FIREBASE_PUSH_DATA_TITLE:    "Alex Kim booked Intro call",
	} {
		if w.deliverData[k] != want {
			t.Errorf("push %s = %q, want %q", k, w.deliverData[k], want)
		}
	}
	if w.deliverID != "b1" {
		t.Errorf("a booking's push is noticed as %q, want its telling's key b1", w.deliverID)
	}
}

// A closed notice (a request for access that wasn't approved, told to someone
// who can't open the doc) names nothing and opens nothing live, and its email
// has no subtitle: as the inbox reads it back. An open one names its doc.
func TestAClosedNoticeNamesNothingLiveOrByEmail(t *testing.T) {
	w := &toldWays{}
	withTellSeams(t, w)
	n := mention()
	Tell(context.Background(), n, []NoticeRecipient{{UserUUID: sam, Reason: "mention"}})
	if w.liveTitle != "Launch notes" || !w.liveOpen || w.emailSubtitle != "Launch notes" {
		t.Errorf("an open notice: live %q (opens %v), email subtitle %q", w.liveTitle, w.liveOpen, w.emailSubtitle)
	}

	w = &toldWays{}
	withTellSeams(t, w)
	n = Notice{Kind: "doc.access_answered", ResourceKind: "doc", ResourceUUID: "d1", ResourceTitle: "Launch notes", Closed: true,
		Title: "Your request for access wasn't approved", Path: "/app/doc/d1", Email: EventDocShared, DedupKey: "doc.access_answered|r1"}
	Tell(context.Background(), n, []NoticeRecipient{{UserUUID: sam, Reason: "subject"}})
	if !slices.Equal(w.live, []string{sam}) || w.liveTitle != "" || w.liveOpen || w.emailSubtitle != "" || !slices.Equal(w.emailed, []string{sam}) {
		t.Errorf("a closed notice: live %v %q (opens %v), email subtitle %q, emailed %v", w.live, w.liveTitle, w.liveOpen, w.emailSubtitle, w.emailed)
	}
}

// A notice's live inbox item says what kind of actor made the change as the
// feed reads it back (business/Follow, activity.ActorKind): an app for an
// automation (check-ins), an agent for an agent's principal. The live item said
// "person" for any actor without an agent, so a bot's notice moved between
// the people and agents filters on a reload.
func TestALiveNoticeSaysWhatKindOfActorActed(t *testing.T) {
	for _, c := range []struct {
		email, want string
	}{
		{userDomain.CheckInBotEmail, "app"},
		// The Slack bridge carries people's own words: a person, live as read back.
		{userDomain.SlackBridgeBotEmail, "person"},
		{userDomain.AgentBotPrefix + "release-captain" + userDomain.BotEmailDomain, "agent"},
		{"akash@example.test", "person"},
	} {
		w := &toldWays{}
		withTellSeams(t, w)
		n := mention()
		n.Actor = &dgraphStruct.DgraphUser{Uuid: akash, UserFullName: "Release Captain", EmailID: c.email, IsBot: c.email != "akash@example.test"}
		Tell(context.Background(), n, []NoticeRecipient{{UserUUID: sam, Reason: "mention"}})
		if w.liveKind != c.want {
			t.Errorf("a notice from %s is a %q's live item, want %q", c.email, w.liveKind, c.want)
		}
	}
}

// A deactivated person keeps their grants and follows, so the people a doc's
// change concerns can include them: they're told nothing, no inbox row among
// it, as an email isn't sent them (Dispatch reads active people alone). And a
// read of who is active that fails tells nobody, as a push and an email don't.
func TestTellingSkipsSomeoneDeactivated(t *testing.T) {
	w := &toldWays{deactivated: map[string]bool{maya: true}}
	withTellSeams(t, w)
	told := Tell(context.Background(), mention(), []NoticeRecipient{{UserUUID: sam, Reason: "mention"}, {UserUUID: maya, Reason: "mention"}})
	if !slices.Equal(told, []string{sam}) || !slices.Equal(w.written, []string{sam}) || !slices.Equal(w.live, []string{sam}) {
		t.Errorf("told %v, wrote %v, live %v; want sam alone: maya is deactivated", told, w.written, w.live)
	}
	if slices.Contains(w.delivered, maya) || slices.Contains(w.emailed, maya) {
		t.Errorf("maya, deactivated, was pushed (%v) or emailed (%v)", w.delivered, w.emailed)
	}

	down := &toldWays{activeErr: errors.New("postgres: connection refused")}
	withTellSeams(t, down)
	if told := Tell(context.Background(), mention(), []NoticeRecipient{{UserUUID: sam, Reason: "mention"}}); len(told) != 0 || len(down.written) != 0 {
		t.Errorf("with who is active unknown, told %v and wrote %v; want nobody", told, down.written)
	}
}

// A telling writes its notices in one statement, however many people it
// tells: it wrote one row a statement, a round trip per person.
func TestATellingWritesItsNoticesInOneStatement(t *testing.T) {
	w := &toldWays{}
	withTellSeams(t, w)
	told := Tell(context.Background(), mention(), []NoticeRecipient{{UserUUID: sam, Reason: "mention"}, {UserUUID: maya, Reason: "mention"},
		{UserUUID: "44444444-4444-4444-8444-444444444444", Reason: "mention"}})
	if w.writes != 1 || len(told) != 3 {
		t.Errorf("telling three people made %d writes and told %v, want one write telling all three", w.writes, told)
	}
}
