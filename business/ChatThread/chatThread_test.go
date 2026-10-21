package business

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	threadIndex "github.com/akashc777/OneCamp/business/ThreadIndex"
	domain "github.com/akashc777/OneCamp/domain/Message"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	models "github.com/akashc777/OneCamp/models/postgres/Thread"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

const (
	rootID = "00000000-0000-4000-8000-000000000001"
	maya   = "00000000-0000-4000-8000-0000000000a1"
	jonas  = "00000000-0000-4000-8000-0000000000a2"
)

var today = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

func person(id string) *userModels.UserInfo {
	return &userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uuid: id}}
}

// world is what the seams answer: the conversations each person can read, the
// graph's roots, and the writes asked for.
type world struct {
	canRead  map[string]bool // "kind:id" and "thread:root"
	rows     map[uuid.UUID]bool
	roots    map[string]domain.Message
	adopted  []threadIndex.Thread
	follows  []string
	reads    []string
	unreads  []string
	allowed  bool
	resolves int
	batches  int // how many times many conversations were asked about at once
}

func stubWorld(t *testing.T) *world {
	t.Helper()
	w := &world{canRead: map[string]bool{}, rows: map[uuid.UUID]bool{}, roots: map[string]domain.Message{}, allowed: true}
	old := struct {
		resolve    func(context.Context, *userModels.UserInfo, conversationBusiness.Ref) (accessBusiness.Decision, error)
		resolveAll func(context.Context, *userModels.UserInfo, []conversationBusiness.Ref) (map[conversationBusiness.Ref]accessBusiness.Decision, error)
		getThread  func(context.Context, uuid.UUID) (*models.Row, error)
		readRoots  func(context.Context, bool, []string) ([]domain.Message, error)
		adopt      func(context.Context, threadIndex.Thread) (uuid.UUID, error)
		setFollow  func(context.Context, uuid.UUID, uuid.UUID, string, time.Time) error
		markRead   func(context.Context, uuid.UUID, uuid.UUID, time.Time) (bool, error)
		markUnread func(context.Context, uuid.UUID, uuid.UUID) (bool, error)
		choiceOf   func(context.Context, uuid.UUID, uuid.UUID) (string, bool, *time.Time, error)
		allowWrite func(context.Context, uuid.UUID) bool
		now        func() time.Time
	}{resolve, resolveAll, getThread, readRoots, adopt, setFollow, markRead, markUnread, choiceOf, allowWrite, now}
	t.Cleanup(func() {
		resolve, resolveAll, getThread, readRoots, adopt, setFollow, markRead, markUnread, choiceOf, allowWrite, now =
			old.resolve, old.resolveAll, old.getThread, old.readRoots, old.adopt, old.setFollow, old.markRead, old.markUnread, old.choiceOf, old.allowWrite, old.now
	})
	resolve = func(_ context.Context, _ *userModels.UserInfo, ref conversationBusiness.Ref) (accessBusiness.Decision, error) {
		w.resolves++
		if w.canRead[ref.Kind+":"+ref.ID] {
			return accessBusiness.Decision{Level: conversationBusiness.LevelRead}, nil
		}
		return accessBusiness.Decision{}, conversationBusiness.ErrNotFound
	}
	resolveAll = func(_ context.Context, _ *userModels.UserInfo, refs []conversationBusiness.Ref) (map[conversationBusiness.Ref]accessBusiness.Decision, error) {
		w.batches++
		out := map[conversationBusiness.Ref]accessBusiness.Decision{}
		for _, ref := range refs {
			w.resolves++
			if w.canRead[ref.Kind+":"+ref.ID] {
				out[ref] = accessBusiness.Decision{Level: conversationBusiness.LevelRead}
			}
		}
		return out, nil
	}
	getThread = func(_ context.Context, id uuid.UUID) (*models.Row, error) {
		if w.rows[id] {
			return &models.Row{ID: id}, nil
		}
		return nil, nil
	}
	readRoots = func(_ context.Context, channel bool, ids []string) ([]domain.Message, error) {
		var out []domain.Message
		for _, id := range ids {
			if m, ok := w.roots[id]; ok && (m.In == "launch") == channel {
				out = append(out, m)
			}
		}
		return out, nil
	}
	adopt = func(_ context.Context, th threadIndex.Thread) (uuid.UUID, error) {
		w.adopted = append(w.adopted, th)
		id := uuid.MustParse(th.Root)
		w.rows[id] = true
		return id, nil
	}
	setFollow = func(_ context.Context, id, user uuid.UUID, follow string, readAt time.Time) error {
		if !readAt.Equal(today) {
			t.Errorf("following made her read to %v, want now", readAt)
		}
		w.follows = append(w.follows, user.String()+":"+follow)
		return nil
	}
	markRead = func(_ context.Context, id, user uuid.UUID, at time.Time) (bool, error) {
		if !at.Equal(today) {
			t.Errorf("read at %v, want now", at)
		}
		w.reads = append(w.reads, user.String())
		return true, nil
	}
	markUnread = func(_ context.Context, id, user uuid.UUID) (bool, error) {
		w.unreads = append(w.unreads, user.String())
		return true, nil
	}
	choiceOf = func(context.Context, uuid.UUID, uuid.UUID) (string, bool, *time.Time, error) {
		return models.FollowOn, true, nil, nil
	}
	allowWrite = func(context.Context, uuid.UUID) bool { return w.allowed }
	now = func() time.Time { return today }
	return w
}

func rejection(t *testing.T, err error, status int) {
	t.Helper()
	rj, ok := sendBusiness.AsRejection(err)
	if !ok || rj.Status != status {
		t.Errorf("err %v, want a refusal in words with status %d", err, status)
	}
}

// ChatThread follows the kinds ThreadIndex records, spelt as the Access
// Resolver spells them, and the two agree on which conversation is a DM.
func TestTheIndexSpellsKindsAsTheResolver(t *testing.T) {
	if threadIndex.KindChannel != conversationBusiness.KindChannel || threadIndex.KindDM != conversationBusiness.KindDM || threadIndex.KindGroup != conversationBusiness.KindGroup {
		t.Error("ThreadIndex's kinds are spelt otherwise than the conversation contract's")
	}
	for _, grp := range []string{maya + " " + jonas, "a1b2c3"} {
		if threadIndex.KindOfChat(grp) != conversationBusiness.ChatRef(grp).Kind {
			t.Errorf("%q: the index says %q, the contract %q", grp, threadIndex.KindOfChat(grp), conversationBusiness.ChatRef(grp).Kind)
		}
	}
}

// Following a thread from before replies were indexed makes it a row from
// its root, then sets the person's choice.
func TestFollowingAThreadAdoptsItFromItsRoot(t *testing.T) {
	w := stubWorld(t)
	w.canRead["thread:"+rootID] = true
	at := today.Add(-time.Hour)
	w.roots[rootID] = domain.Message{ID: rootID, In: maya + " " + jonas, By: &domain.Person{UUID: jonas}, CreatedAt: &at}
	choice, err := Follow(context.Background(), person(maya), rootID, true)
	if err != nil {
		t.Fatal(err)
	}
	want := threadIndex.Thread{Kind: threadIndex.KindDM, ID: maya + " " + jonas, Root: rootID, RootBy: jonas, RootAt: at}
	if len(w.adopted) != 1 || w.adopted[0] != want {
		t.Errorf("adopted %+v, want %+v", w.adopted, want)
	}
	if !slices.Equal(w.follows, []string{maya + ":on"}) || !choice.Following || choice.Root != rootID {
		t.Errorf("follows %v, choice %+v", w.follows, choice)
	}
	// Unfollowing it: no second adoption.
	if _, err := Follow(context.Background(), person(maya), rootID, false); err != nil || len(w.adopted) != 1 || w.follows[1] != maya+":off" {
		t.Errorf("unfollow: err %v, adopted %d, follows %v", err, len(w.adopted), w.follows)
	}
}

// A thread someone can't read is as missing as one that isn't there; a root
// gone from the graph is too; and a burst of writes is refused in words.
func TestFollowingIsRefusedInWords(t *testing.T) {
	w := stubWorld(t)
	_, err := Follow(context.Background(), person(maya), rootID, true)
	rejection(t, err, http.StatusNotFound)
	if len(w.follows) != 0 {
		t.Errorf("followed a thread she can't read: %v", w.follows)
	}
	w.canRead["thread:"+rootID] = true
	_, err = Follow(context.Background(), person(maya), rootID, true)
	rejection(t, err, http.StatusNotFound) // no root in the graph
	_, err = Follow(context.Background(), person(maya), "not-a-uuid", true)
	rejection(t, err, http.StatusNotFound)
	w.allowed = false
	_, err = Follow(context.Background(), person(maya), rootID, true)
	rejection(t, err, http.StatusTooManyRequests)
	if err := SetRead(context.Background(), person(maya), rootID, true); err == nil {
		t.Error("a read past the limit went through")
	}
	if _, err := Follow(context.Background(), &userModels.UserInfo{}, rootID, true); !errors.Is(err, conversationBusiness.ErrNotFound) {
		t.Errorf("nobody followed: %v", err)
	}
}

// Reading a thread moves the read to now; marking it unread moves it back.
func TestReadingAndMarkingUnread(t *testing.T) {
	w := stubWorld(t)
	w.canRead["thread:"+rootID] = true
	if err := SetRead(context.Background(), person(maya), rootID, true); err != nil {
		t.Fatal(err)
	}
	if err := SetRead(context.Background(), person(maya), rootID, false); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(w.reads, []string{maya}) || !slices.Equal(w.unreads, []string{maya}) {
		t.Errorf("reads %v, unreads %v", w.reads, w.unreads)
	}
	w.canRead["thread:"+rootID] = false
	rejection(t, SetRead(context.Background(), person(jonas), rootID, true), http.StatusNotFound)
}

// A page of someone's threads: those in conversations they can't read now, or
// whose root is gone, are left out; each says how many replies are new from
// others since they read it, and shows its newest two.
func TestAPageOfThreads(t *testing.T) {
	w := stubWorld(t)
	oldMine, oldCount, oldReplies := mineOf, unreadCount, readReplies
	t.Cleanup(func() { mineOf, unreadCount, readReplies = oldMine, oldCount, oldReplies })
	ids := []string{rootID, "00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-000000000003", "00000000-0000-4000-8000-000000000004"}
	read := today.Add(-2 * time.Hour)
	mineOf = func(_ context.Context, user uuid.UUID, kinds []string, limit, offset int) ([]models.Mine, error) {
		if limit != PageSize+1 || offset != 0 || !slices.Equal(kinds, Kinds) {
			t.Errorf("asked %d from %d of %v", limit, offset, kinds)
		}
		return []models.Mine{
			{ThreadID: uuid.MustParse(ids[0]), Kind: "channel", Conversation: "launch", LastReadAt: &read, Unread: true, Follow: "auto"},
			{ThreadID: uuid.MustParse(ids[1]), Kind: "channel", Conversation: "secret", Unread: true, Follow: "auto"},
			{ThreadID: uuid.MustParse(ids[2]), Kind: "channel", Conversation: "launch", Follow: "on"},
			{ThreadID: uuid.MustParse(ids[3]), Kind: "group", Conversation: "grp", Follow: "auto"},
		}, nil
	}
	unreadCount = func(context.Context, uuid.UUID, []string) (int, error) { return 2, nil }
	w.canRead["channel:launch"] = true
	w.canRead["group:grp"] = true
	gone := today.Add(-time.Minute)
	w.roots[ids[0]] = domain.Message{ID: ids[0], In: "launch", HTML: "<p>Launch plan</p>", Place: domain.Place{Name: "launch"}}
	w.roots[ids[2]] = domain.Message{ID: ids[2], In: "launch", DeletedAt: &gone}
	w.roots[ids[3]] = domain.Message{ID: ids[3], In: "grp"}
	at := func(h int) *time.Time { v := today.Add(-time.Duration(h) * time.Minute); return &v }
	readReplies = func(_ context.Context, channel bool, roots []string, latest int) (map[string]domain.Replies, error) {
		if latest != domain.MaxLatest {
			t.Errorf("latest %d", latest)
		}
		if !channel {
			return map[string]domain.Replies{ids[3]: {Count: 1, Latest: []domain.Reply{{ID: "g1", CreatedAt: at(5)}}}}, nil
		}
		return map[string]domain.Replies{ids[0]: {Count: 9, Latest: []domain.Reply{
			{ID: "r4", CreatedAt: at(10), By: &domain.Person{UUID: jonas}},
			{ID: "r3", CreatedAt: at(20), By: &domain.Person{UUID: maya}}, // hers: not new to her
			{ID: "r2", CreatedAt: at(30), By: &domain.Person{UUID: jonas}},
			{ID: "r1", CreatedAt: at(300), By: &domain.Person{UUID: jonas}}, // before she read
		}}}, nil
	}
	page, err := Mine(context.Background(), person(maya), 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.Unread != 2 || page.Next != 0 {
		t.Errorf("unread %d, next %d", page.Unread, page.Next)
	}
	if len(page.Threads) != 2 || page.Threads[0].Root != ids[0] || page.Threads[1].Root != ids[3] {
		t.Fatalf("threads %+v: want launch's and the group's; never secret's (can't read) or a deleted root's", page.Threads)
	}
	first := page.Threads[0]
	if first.New != 2 || first.NewMore || first.Replies != 9 || len(first.Latest) != 2 || first.Latest[0].ID != "r4" || first.Place.Name != "launch" || first.Message.HTML != "<p>Launch plan</p>" {
		t.Errorf("first %+v", first)
	}
	if w.resolves != 3 || w.batches != 1 {
		t.Errorf("asked about %d conversations in %d asks, want each of three once, in one ask", w.resolves, w.batches)
	}
}

// A full page says where the next starts, up to MaxThreads.
func TestThreadsPage(t *testing.T) {
	stubWorld(t)
	oldMine, oldCount := mineOf, unreadCount
	t.Cleanup(func() { mineOf, unreadCount = oldMine, oldCount })
	var asked []int
	mineOf = func(_ context.Context, _ uuid.UUID, _ []string, limit, offset int) ([]models.Mine, error) {
		asked = append(asked, offset)
		return make([]models.Mine, limit), nil
	}
	unreadCount = func(context.Context, uuid.UUID, []string) (int, error) { return 0, nil }
	page, err := Mine(context.Background(), person(maya), 40)
	if err != nil || page.Next != 60 {
		t.Errorf("next %d, err %v", page.Next, err)
	}
	if page, _ := Mine(context.Background(), person(maya), MaxThreads-PageSize); page.Next != 0 {
		t.Errorf("past MaxThreads, next %d", page.Next)
	}
	if _, _ = Mine(context.Background(), person(maya), -5); asked[len(asked)-1] != 0 {
		t.Errorf("a negative offset read from %d", asked[len(asked)-1])
	}
}

// How many replies are new: after the read, from someone else, "20+" when
// every one of the latest read is.
func TestNewReplies(t *testing.T) {
	read := today
	after := func(m int) *time.Time { v := today.Add(time.Duration(m) * time.Minute); return &v }
	other, me := &domain.Person{UUID: jonas}, &domain.Person{UUID: maya}
	full := make([]domain.Reply, domain.MaxLatest)
	for i := range full {
		full[i] = domain.Reply{CreatedAt: after(100 - i), By: other}
	}
	for _, tc := range []struct {
		name   string
		latest []domain.Reply
		read   *time.Time
		n      int
		more   bool
	}{
		{"none", nil, &read, 0, false},
		{"never read", []domain.Reply{{CreatedAt: after(-5), By: other}}, nil, 1, false},
		{"one after", []domain.Reply{{CreatedAt: after(5), By: other}, {CreatedAt: after(-5), By: other}}, &read, 1, false},
		{"mine are not new", []domain.Reply{{CreatedAt: after(5), By: me}, {CreatedAt: after(3), By: other}}, &read, 1, false},
		{"twenty and more", full, &read, domain.MaxLatest, true},
	} {
		n, more := newReplies(tc.latest, tc.read, maya)
		if n != tc.n || more != tc.more {
			t.Errorf("%s: %d %v, want %d %v", tc.name, n, more, tc.n, tc.more)
		}
	}
}
