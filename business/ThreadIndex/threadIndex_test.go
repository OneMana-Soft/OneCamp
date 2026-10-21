package business

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	models "github.com/akashc777/OneCamp/models/postgres/Thread"
	"github.com/google/uuid"
)

const (
	root  = "00000000-0000-4000-8000-000000000001"
	maya  = "00000000-0000-4000-8000-0000000000a1"
	jonas = "00000000-0000-4000-8000-0000000000a2"
	sam   = "00000000-0000-4000-8000-0000000000a3"
)

// index is the threads table as the seams see it.
type index struct {
	rows    map[uuid.UUID]*models.Row
	people  map[uuid.UUID]models.Participant // by user
	touched []time.Time
	failAdd error
	writes  int // how many people writes were made
}

func stubIndex(t *testing.T) *index {
	t.Helper()
	ix := &index{rows: map[uuid.UUID]*models.Row{}, people: map[uuid.UUID]models.Participant{}}
	oldInsert, oldAdd, oldTouch, oldNow := insertThread, addPeople, touchReply, now
	t.Cleanup(func() { insertThread, addPeople, touchReply, now = oldInsert, oldAdd, oldTouch, oldNow })
	insertThread = func(_ context.Context, r *models.Row, ifAbsent bool) (bool, error) {
		if !ifAbsent {
			t.Error("a chat thread is inserted only if absent")
		}
		if _, ok := ix.rows[r.ID]; ok {
			return false, nil
		}
		ix.rows[r.ID] = r
		return true, nil
	}
	addPeople = func(_ context.Context, people []models.Participant) error {
		if ix.failAdd != nil {
			return ix.failAdd
		}
		ix.writes++
		ranks := []string{"reader", "mentioned", "replier", "author"}
		for _, p := range people {
			old, ok := ix.people[p.UserUUID]
			if ok {
				if slices.Index(ranks, old.Role) > slices.Index(ranks, p.Role) {
					p.Role = old.Role
				}
				if p.LastReadAt == nil || (old.LastReadAt != nil && old.LastReadAt.After(*p.LastReadAt)) {
					p.LastReadAt = old.LastReadAt
				}
			}
			ix.people[p.UserUUID] = p
		}
		return nil
	}
	touchReply = func(_ context.Context, id uuid.UUID, at time.Time) (bool, error) {
		if _, ok := ix.rows[id]; !ok {
			return false, nil
		}
		ix.touched = append(ix.touched, at)
		return true, nil
	}
	now = func() time.Time { return time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC) }
	return ix
}

func (ix *index) person(t *testing.T, id string) models.Participant {
	t.Helper()
	p, ok := ix.people[uuid.MustParse(id)]
	if !ok {
		t.Fatalf("%s is none of the thread's people (%v)", id, ix.people)
	}
	return p
}

var rootAt = time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)

func thread() Thread {
	return Thread{Kind: KindChannel, ID: "launch", Root: root, RootBy: maya, RootAt: rootAt}
}

// The first reply makes the thread a row: its root's author read to the root,
// the writer read to their reply, those mentioned taking part unread.
func TestTheFirstReplyMakesTheThreadARow(t *testing.T) {
	ix := stubIndex(t)
	at := rootAt.Add(time.Hour)
	Replied(context.Background(), Reply{Thread: thread(), By: jonas, Mentioned: []string{sam, jonas, "not-a-uuid"}, At: at})
	row := ix.rows[uuid.MustParse(root)]
	if row == nil || row.ResourceKind != KindChannel || row.ResourceUUID != "launch" || row.AnchorKind != "message" ||
		row.CreatedBy != uuid.MustParse(maya) || !row.CreatedAt.Equal(rootAt) || string(row.AnchorMeta) != `{"message":"`+root+`"}` {
		t.Fatalf("row %+v", row)
	}
	if len(ix.touched) != 1 || !ix.touched[0].Equal(at) {
		t.Errorf("latest reply recorded %v, want once at %v", ix.touched, at)
	}
	if p := ix.person(t, maya); p.Role != "author" || p.LastReadAt == nil || !p.LastReadAt.Equal(rootAt) {
		t.Errorf("the author %+v: want read to the root, so the reply is new to her", p)
	}
	if p := ix.person(t, jonas); p.Role != "replier" || p.LastReadAt == nil || !p.LastReadAt.Equal(at) {
		t.Errorf("the writer %+v: want read to their own reply", p)
	}
	if p := ix.person(t, sam); p.Role != "mentioned" || p.LastReadAt != nil {
		t.Errorf("sam, mentioned, %+v: want taking part and unread", p)
	}
	if len(ix.people) != 3 {
		t.Errorf("people %v: the writer mentioning themselves, and an id that isn't one, add nobody", ix.people)
	}
	// The author when the thread became a row, then the writer and everyone
	// the reply mentions in one write: a reply mentioning ten people is two
	// statements, not eleven.
	if ix.writes != 2 {
		t.Errorf("%d writes of the thread's people, want 2 (the author's, then the reply's)", ix.writes)
	}
}

// A later reply is one write when the thread is a row: no insert, no author.
func TestALaterReplyTouchesTheRowItHas(t *testing.T) {
	ix := stubIndex(t)
	Replied(context.Background(), Reply{Thread: thread(), By: jonas, At: rootAt.Add(time.Hour)})
	inserts := 0
	insertThread = func(context.Context, *models.Row, bool) (bool, error) { inserts++; return false, nil }
	Replied(context.Background(), Reply{Thread: thread(), By: sam, At: rootAt.Add(2 * time.Hour)})
	if inserts != 0 {
		t.Errorf("a reply in a thread already a row inserted %d times", inserts)
	}
	if p := ix.person(t, sam); p.Role != "replier" {
		t.Errorf("sam %+v", p)
	}
}

// Only a channel's, a DM's or a group's thread is indexed; and a failure is
// logged, never the reply's.
func TestOnlyChatThreadsAreIndexed(t *testing.T) {
	ix := stubIndex(t)
	tt := thread()
	tt.Kind = "doc"
	Replied(context.Background(), Reply{Thread: tt, By: jonas})
	tt = thread()
	tt.ID = ""
	Replied(context.Background(), Reply{Thread: tt, By: jonas})
	if len(ix.rows) != 0 {
		t.Errorf("indexed %v", ix.rows)
	}
	ix.failAdd = errors.New("down")
	Replied(context.Background(), Reply{Thread: thread(), By: jonas}) // logged, no panic
	if err := replied(context.Background(), Reply{Thread: thread(), By: jonas}); err == nil {
		t.Error("a failed write said nothing")
	}
}

// A root whose time isn't known: the author hasn't read any of it.
func TestAnAuthorWithNoRootTimeHasReadNothing(t *testing.T) {
	ix := stubIndex(t)
	tt := thread()
	tt.RootAt = time.Time{}
	if _, err := Adopt(context.Background(), tt); err != nil {
		t.Fatal(err)
	}
	if p := ix.person(t, maya); p.LastReadAt != nil {
		t.Errorf("the author read to %v; with no root time, every reply is new to her", p.LastReadAt)
	}
	if row := ix.rows[uuid.MustParse(root)]; !row.CreatedAt.Equal(now()) {
		t.Errorf("made at %v, want now", row.CreatedAt)
	}
}

// An earlier thread joins the index read to its latest reply, by its author
// and repliers; one already a row is left alone, so nobody's unread is lost.
func TestAnEarlierThreadIsAdoptedAsRead(t *testing.T) {
	ix := stubIndex(t)
	last := rootAt.Add(3 * time.Hour)
	made, err := AdoptAsRead(context.Background(), thread(), last, []string{jonas, maya, sam, jonas, "x"})
	if err != nil || !made {
		t.Fatalf("made %v, err %v", made, err)
	}
	for _, id := range []string{maya, jonas, sam} {
		if p := ix.person(t, id); p.LastReadAt == nil || !p.LastReadAt.Equal(last) {
			t.Errorf("%s read to %v, want %v", id, p.LastReadAt, last)
		}
	}
	if p := ix.person(t, maya); p.Role != "author" {
		t.Errorf("maya, its author and a replier, is %q", p.Role)
	}
	if len(ix.touched) != 1 || !ix.touched[0].Equal(last) {
		t.Errorf("latest reply %v, want %v", ix.touched, last)
	}
	// Later, one of its people has an unread reply: adopting again changes
	// nothing.
	unread := rootAt
	ix.people[uuid.MustParse(sam)] = models.Participant{ThreadID: uuid.MustParse(root), UserUUID: uuid.MustParse(sam), Role: "replier", LastReadAt: &unread}
	made, err = AdoptAsRead(context.Background(), thread(), last.Add(time.Hour), []string{sam})
	if err != nil || made {
		t.Fatalf("adopted a thread already a row: made %v, err %v", made, err)
	}
	if p := ix.person(t, sam); !p.LastReadAt.Equal(unread) {
		t.Errorf("sam's unread was lost: read to %v", p.LastReadAt)
	}
}

// A post's and a message's threads, as the reply paths name them.
func TestThreadsOfPostsAndMessages(t *testing.T) {
	post := &dgraphStruct.DgraphPost{Uuid: root, PostBy: &dgraphStruct.DgraphUser{Uuid: maya}, Channel: &dgraphStruct.DgraphChannel{Uuid: "launch"}, CreatedAt: &rootAt}
	if got := OfPost(post); got != thread() {
		t.Errorf("OfPost %+v, want %+v", got, thread())
	}
	if got := OfPost(&dgraphStruct.DgraphPost{Uuid: root}); got.ID != "" || got.RootBy != "" || !got.RootAt.IsZero() {
		t.Errorf("a post with nothing known %+v", got)
	}
	dm := maya + " " + jonas
	chat := &dgraphStruct.DgraphChat{Uuid: root, From: &dgraphStruct.DgraphUser{Uuid: jonas}, CreatedAt: &rootAt}
	if got := OfChat(dm, chat); got != (Thread{Kind: KindDM, ID: dm, Root: root, RootBy: jonas, RootAt: rootAt}) {
		t.Errorf("OfChat in a DM %+v", got)
	}
	if got := OfChat("grp", chat); got.Kind != KindGroup {
		t.Errorf("OfChat in a group %+v", got)
	}
	if OfPost(nil) != (Thread{}) || OfChat("grp", nil) != (Thread{}) {
		t.Error("nothing to go on made a thread")
	}
}

// Who follows a thread, as uuids.
func TestFollowingReadsTheIndex(t *testing.T) {
	old := followersOf
	t.Cleanup(func() { followersOf = old })
	followersOf = func(_ context.Context, id uuid.UUID) ([]uuid.UUID, []uuid.UUID, error) {
		if id != uuid.MustParse(root) {
			t.Errorf("asked about %s", id)
		}
		return []uuid.UUID{uuid.MustParse(jonas)}, []uuid.UUID{uuid.MustParse(maya)}, nil
	}
	on, off, err := Following(context.Background(), root)
	if err != nil || !slices.Equal(on, []string{jonas}) || !slices.Equal(off, []string{maya}) {
		t.Errorf("on %v, off %v, err %v", on, off, err)
	}
	if on, off, err := Following(context.Background(), "not-a-uuid"); on != nil || off != nil || err != nil {
		t.Errorf("a root that isn't a uuid: %v %v %v", on, off, err)
	}
}
