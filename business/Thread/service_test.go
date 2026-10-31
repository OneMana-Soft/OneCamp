package business

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
)

// memStore keeps threads in memory, with the Postgres store's rules: one
// thread per id, the highest role kept, reads only move forward, and a status
// set once.
type memStore struct {
	mu      sync.Mutex
	threads map[string]Thread
	parts   map[string]map[string]Participant
	failIns bool
}

func newMemStore() *memStore {
	return &memStore{threads: map[string]Thread{}, parts: map[string]map[string]Participant{}}
}

var rank = map[Role]int{RoleReader: 0, RoleMentioned: 1, RoleReplier: 2, RoleAuthor: 3}

func (m *memStore) insert(_ context.Context, t Thread, ifAbsent bool) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failIns {
		return false, errors.New("store down")
	}
	if _, ok := m.threads[t.ID]; ok {
		if ifAbsent {
			return false, nil
		}
		return false, errors.New("duplicate id")
	}
	m.threads[t.ID] = t
	return true, nil
}
func (m *memStore) get(_ context.Context, id string) (*Thread, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.threads[id]; ok {
		return &t, nil
	}
	return nil, nil
}
func (m *memStore) list(_ context.Context, kind, res string) ([]Thread, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Thread
	for _, t := range m.threads {
		if t.ResourceKind == kind && t.ResourceUUID == res {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}
func (m *memStore) setStatus(_ context.Context, id string, s Status, by string, at time.Time) (*Thread, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.threads[id]
	if !ok {
		return nil, false, nil
	}
	if t.Status == s {
		return &t, false, nil
	}
	t.Status, t.ResolvedBy, t.ResolvedAt, t.UpdatedAt = s, by, nil, at
	if by != "" {
		t.ResolvedAt = &at
	}
	m.threads[id] = t
	return &t, true, nil
}
func (m *memStore) setTask(_ context.Context, id, task string, at time.Time) (*Thread, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.threads[id]
	if !ok || (t.TaskUUID != "" && t.TaskUUID != task) {
		return nil, nil
	}
	t.TaskUUID, t.UpdatedAt = task, at
	m.threads[id] = t
	return &t, nil
}

func (m *memStore) byTask(_ context.Context, task string) ([]Thread, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Thread
	for _, t := range m.threads {
		if t.TaskUUID == task {
			out = append(out, t)
		}
	}
	return out, nil
}

func (m *memStore) tied(_ context.Context, limit int) ([]Thread, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Thread
	for _, t := range m.threads {
		if t.TaskUUID != "" && len(out) < limit {
			out = append(out, t)
		}
	}
	return out, nil
}

func (m *memStore) setAnchor(_ context.Context, id string, a Anchor, at time.Time) (*Thread, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.threads[id]
	if !ok {
		return nil, nil
	}
	t.Anchor, t.UpdatedAt = a, at
	m.threads[id] = t
	return &t, nil
}
func (m *memStore) touch(_ context.Context, id string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.threads[id]; ok && at.After(t.UpdatedAt) {
		t.UpdatedAt = at
		m.threads[id] = t
	}
	return nil
}
func (m *memStore) remove(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.threads, id)
	delete(m.parts, id)
	return nil
}
func (m *memStore) join(_ context.Context, tid, user string, r Role, readAt *time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.parts[tid] == nil {
		m.parts[tid] = map[string]Participant{}
	}
	p, ok := m.parts[tid][user]
	if !ok {
		p = Participant{UserUUID: user, Role: r}
	} else if rank[r] > rank[p.Role] {
		p.Role = r
	}
	if readAt != nil && (p.LastReadAt == nil || readAt.After(*p.LastReadAt)) {
		at := *readAt
		p.LastReadAt = &at
	}
	m.parts[tid][user] = p
	return nil
}
func (m *memStore) participants(_ context.Context, tid string) ([]Participant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Participant
	for _, p := range m.parts[tid] {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UserUUID < out[j].UserUUID })
	return out, nil
}
func (m *memStore) participantsOf(ctx context.Context, kind, res string) (map[string][]Participant, error) {
	ts, _ := m.list(ctx, kind, res)
	out := map[string][]Participant{}
	for _, t := range ts {
		out[t.ID], _ = m.participants(ctx, t.ID)
	}
	return out, nil
}

// fakeDoc is a doc as threads see it: levels by person, its comments, and
// what the doc comment path would do.
type fakeDoc struct {
	mu       sync.Mutex
	levels   map[string]accessBusiness.Level
	deleted  bool
	comments []Comment
	now      func() time.Time
	failPost bool
	claimed  map[string]string
}

func (f *fakeDoc) Anchors() []AnchorKind { return []AnchorKind{AnchorYRange, AnchorBlock, AnchorNone} }
func (f *fakeDoc) Access(_ context.Context, who accessBusiness.Subject, _ accessBusiness.Resource) (accessBusiness.Decision, error) {
	return accessBusiness.Decision{Level: f.levels[who.UserUUID], Deleted: f.deleted}, nil
}
func (f *fakeDoc) Comments(context.Context, accessBusiness.Subject, accessBusiness.Resource) ([]Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Comment(nil), f.comments...), nil
}
func (f *fakeDoc) Post(_ context.Context, who accessBusiness.Subject, _ accessBusiness.Resource, threadID string, b Body) (Posted, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPost {
		return Posted{}, errors.New("graph down")
	}
	c := Comment{ID: fmt.Sprintf("c%d", len(f.comments)+1), ThreadID: threadID, Text: b.HTML, By: Person{UUID: who.UserUUID}, CreatedAt: f.now()}
	f.comments = append(f.comments, c)
	p := Posted{Comment: c}
	if b.HTML == "<p>@sam</p>" {
		p.Mentioned = []string{"sam"}
	}
	return p, nil
}
func (f *fakeDoc) Legacy(_ context.Context, id string) (Thread, Comment, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.comments {
		if c.ID == id && (c.ThreadID == "" || c.ThreadID == c.ID) {
			c.ThreadID = c.ID
			return legacyThread("doc", "d1", c), c, true, nil
		}
	}
	return Thread{}, Comment{}, false, nil
}
func (f *fakeDoc) Claim(_ context.Context, tid string, root Comment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimed == nil {
		f.claimed = map[string]string{}
	}
	f.claimed[root.ID] = tid
	for i := range f.comments {
		if f.comments[i].ID == root.ID {
			f.comments[i].ThreadID = tid
		}
	}
	return nil
}

type events struct {
	mu  sync.Mutex
	got []resourceEventBusiness.Event
}

func (e *events) Emit(_ context.Context, ev resourceEventBusiness.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.got = append(e.got, ev)
}

type world struct {
	svc   *service
	store *memStore
	doc   *fakeDoc
	ev    *events
	res   accessBusiness.Resource
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{store: newMemStore(), ev: &events{}, res: accessBusiness.Resource{Kind: "doc", UUID: "d1"}}
	w.doc = &fakeDoc{
		levels: map[string]accessBusiness.Level{
			"owner": accessBusiness.LevelFull, "editor": accessBusiness.LevelEdit,
			"ann": accessBusiness.LevelComment, "bob": accessBusiness.LevelComment,
			"sam": accessBusiness.LevelComment, "viewer": accessBusiness.LevelView,
		},
	}
	reg := &Registry{byKind: map[string]Resource{}}
	reg.Register("doc", w.doc)
	w.svc = newService(w.store, reg)
	n := 0
	var idMu sync.Mutex
	w.svc.newID = func() string {
		idMu.Lock()
		defer idMu.Unlock()
		n++
		return fmt.Sprintf("00000000-0000-0000-0000-%012d", n)
	}
	var mu sync.Mutex
	clock := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	w.svc.now = func() time.Time { mu.Lock(); defer mu.Unlock(); clock = clock.Add(time.Second); return clock }
	w.doc.now = w.svc.now
	old := resourceEventBusiness.Bus
	resourceEventBusiness.Bus = w.ev
	t.Cleanup(func() { resourceEventBusiness.Bus = old })
	return w
}

func who(id string) accessBusiness.Subject {
	return accessBusiness.Subject{UserUUID: id, UserUID: "0x" + id}
}

var ctx = context.Background()

func rangeAnchor() Anchor {
	return Anchor{Kind: AnchorYRange, Start: []byte{1}, End: []byte{2}, Lineage: "L1", Quote: "Ship on Thursday"}
}

func TestAThreadIsStartedRepliedToAndResolved(t *testing.T) {
	w := newWorld(t)
	v, err := w.svc.Create(ctx, who("ann"), w.res, rangeAnchor(), Body{HTML: "<p>@sam</p>"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != StatusOpen || len(v.Comments) != 1 || v.Anchor.Quote != "Ship on Thursday" || !v.CanResolve || v.Unread {
		t.Fatalf("a new thread: %+v", v)
	}
	if ev := w.ev.got[0]; ev.Kind != ThreadCreated || fmt.Sprint(ev.Subjects) != "[sam]" || ev.Data["thread_id"] != v.ID || ev.Data["comment_id"] == "" {
		t.Errorf("the created event: %+v", ev)
	}
	for _, ev := range w.ev.got {
		for k, val := range ev.Data {
			if val == "<p>@sam</p>" {
				t.Errorf("an event carries the comment's text under %s", k)
			}
		}
	}

	if _, err := w.svc.Reply(ctx, who("bob"), v.ID, Body{HTML: "<p>agreed</p>"}); err != nil {
		t.Fatal(err)
	}
	if ev := w.ev.got[1]; ev.Kind != ThreadReplied || fmt.Sprint(ev.Subjects) != "[ann sam]" {
		t.Errorf("the reply tells the author and who was mentioned, not the replier: %+v", ev.Subjects)
	}

	// Bob didn't start it and can only comment; the editor can resolve it.
	if _, err := w.svc.Resolve(ctx, who("bob"), v.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("a commenter resolved someone else's thread: %v", err)
	}
	got, err := w.svc.Resolve(ctx, who("editor"), v.ID)
	if err != nil || got.Status != StatusResolved || got.ResolvedBy != "editor" || got.ResolvedAt == nil {
		t.Fatalf("resolve: %+v, %v", got, err)
	}
	// Twice is once: no second event.
	n := len(w.ev.got)
	if _, err := w.svc.Resolve(ctx, who("ann"), v.ID); err != nil || len(w.ev.got) != n {
		t.Errorf("resolving a resolved thread: %v, %d new events", err, len(w.ev.got)-n)
	}
	// The author reopens it.
	if got, err = w.svc.Reopen(ctx, who("ann"), v.ID); err != nil || got.Status != StatusOpen || got.ResolvedAt != nil || got.ResolvedBy != "" {
		t.Fatalf("reopen: %+v, %v", got, err)
	}

	open, _ := w.svc.List(ctx, who("bob"), w.res, Filter{Status: StatusOpen})
	resolved, _ := w.svc.List(ctx, who("bob"), w.res, Filter{Status: StatusResolved})
	if len(open) != 1 || len(resolved) != 0 || len(open[0].Comments) != 2 {
		t.Fatalf("open %d resolved %d", len(open), len(resolved))
	}
	if open[0].CanResolve || !open[0].CanReply {
		t.Errorf("bob may reply but not resolve: %+v", open[0])
	}
}

func TestNoThreadIsStartedWithoutItsFirstComment(t *testing.T) {
	w := newWorld(t)
	w.doc.failPost = true
	if _, err := w.svc.Create(ctx, who("ann"), w.res, rangeAnchor(), Body{HTML: "<p>hi</p>"}); err == nil {
		t.Fatal("a thread was started though its comment failed")
	}
	if len(w.store.threads) != 0 || len(w.ev.got) != 0 {
		t.Errorf("left %d threads and %d events behind", len(w.store.threads), len(w.ev.got))
	}
}

func TestWhoCannotTakePart(t *testing.T) {
	w := newWorld(t)
	v, _ := w.svc.Create(ctx, who("ann"), w.res, rangeAnchor(), Body{HTML: "<p>hi</p>"})
	if _, err := w.svc.Create(ctx, who("viewer"), w.res, rangeAnchor(), Body{HTML: "<p>hi</p>"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("a viewer started a thread: %v", err)
	}
	if _, err := w.svc.Reply(ctx, who("viewer"), v.ID, Body{HTML: "<p>hi</p>"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("a viewer replied: %v", err)
	}
	if _, err := w.svc.List(ctx, who("stranger"), w.res, Filter{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("a stranger listed the threads: %v", err)
	}
	if _, err := w.svc.Get(ctx, who("stranger"), v.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("a stranger opened the thread: %v", err)
	}
	if _, err := w.svc.Reanchor(ctx, who("bob"), v.ID, rangeAnchor()); !errors.Is(err, ErrForbidden) {
		t.Errorf("a commenter reattached someone else's thread: %v", err)
	}
	if _, err := w.svc.Reopen(ctx, who("bob"), v.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("a commenter reopened someone else's thread: %v", err)
	}
	if err := w.svc.MarkRead(ctx, who("stranger"), v.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("a stranger marked the thread read: %v", err)
	}
	old := Threads
	Threads = w.svc
	t.Cleanup(func() { Threads = old })
	if _, err := LinkTask(ctx, who("bob"), v.ID, "task-1"); !errors.Is(err, ErrForbidden) {
		t.Errorf("a commenter tied someone else's thread to a task: %v", err)
	}
	if _, err := LinkTask(ctx, who("stranger"), v.ID, "task-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a stranger tied the thread to a task: %v", err)
	}
	if tied, err := LinkTask(ctx, who("ann"), v.ID, "task-1"); err != nil || tied.TaskUUID != "task-1" {
		t.Errorf("ann tying her thread to a task: %+v, %v", tied, err)
	}
	w.doc.deleted = true
	if _, err := w.svc.Reply(ctx, who("owner"), v.ID, Body{HTML: "<p>hi</p>"}); !errors.Is(err, ErrGone) {
		t.Errorf("the owner replied in the trash: %v", err)
	}
	if _, err := w.svc.Get(ctx, who("x"), "not-a-uuid"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a malformed id: %v", err)
	}
	if _, err := w.svc.List(ctx, who("ann"), accessBusiness.Resource{Kind: "board", UUID: "b"}, Filter{}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("a kind with no adapter: %v", err)
	}
}

// Today's flat comments read as page-level threads under their own ids, and
// become threads on the first write, once, however many write at once.
func TestFlatCommentsReadAsThreadsAndAreAdoptedOnWrite(t *testing.T) {
	w := newWorld(t)
	w.doc.comments = []Comment{
		{ID: "11111111-1111-1111-1111-111111111111", Text: "<p>old one</p>", By: Person{UUID: "bob"}, CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
		{ID: "22222222-2222-2222-2222-222222222222", Text: "<p>old two</p>", By: Person{UUID: "ann"}, CreatedAt: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)},
	}
	v, _ := w.svc.Create(ctx, who("ann"), w.res, rangeAnchor(), Body{HTML: "<p>new</p>"})

	list, err := w.svc.List(ctx, who("ann"), w.res, Filter{})
	if err != nil || len(list) != 3 {
		t.Fatalf("list: %d, %v", len(list), err)
	}
	old := list[0]
	if !old.Legacy || old.ID != "11111111-1111-1111-1111-111111111111" || old.Anchor.Kind != AnchorNone || old.Status != StatusOpen || old.Unread || old.CreatedBy != "bob" {
		t.Fatalf("a flat comment reads as: %+v", old)
	}
	if list[2].ID != v.ID || list[2].Legacy {
		t.Errorf("the new thread comes last and isn't legacy: %+v", list[2])
	}
	if len(w.store.threads) != 1 {
		t.Errorf("reading wrote %d threads", len(w.store.threads)-1)
	}
	// Marking a flat comment read writes nothing either.
	if err := w.svc.MarkRead(ctx, who("ann"), old.ID); err != nil || len(w.store.threads) != 1 {
		t.Errorf("mark read on a flat comment: %v, %d threads", err, len(w.store.threads))
	}

	// Ann replies to Bob's old comment, and Sam does at the same moment.
	var wg sync.WaitGroup
	for _, p := range []string{"ann", "sam"} {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			if _, err := w.svc.Reply(ctx, who(p), old.ID, Body{HTML: "<p>reply</p>"}); err != nil {
				t.Errorf("%s replying to a flat comment: %v", p, err)
			}
		}(p)
	}
	wg.Wait()
	got, err := w.svc.Get(ctx, who("bob"), old.ID)
	if err != nil || got.Legacy || len(got.Comments) != 3 || got.CreatedBy != "bob" {
		t.Fatalf("the adopted thread: %+v, %v", got, err)
	}
	if w.doc.claimed[old.ID] != old.ID {
		t.Errorf("the old comment was not put in its thread: %v", w.doc.claimed)
	}
	roles := map[string]Role{}
	for _, p := range got.Participants {
		roles[p.UserUUID] = p.Role
	}
	if roles["bob"] != RoleAuthor || roles["ann"] != RoleReplier || roles["sam"] != RoleReplier {
		t.Errorf("roles: %v", roles)
	}
	if !got.Unread || !got.CanResolve {
		t.Errorf("bob has news and may resolve his own thread: %+v", got)
	}
	// Bob (the author of the old comment) may resolve it; Ann may not.
	if _, err := w.svc.Resolve(ctx, who("ann"), "22222222-2222-2222-2222-222222222222"); err != nil {
		t.Errorf("ann resolving her own old comment: %v", err)
	}
	if _, err := w.svc.Resolve(ctx, who("sam"), old.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("sam resolved bob's: %v", err)
	}
}

// A comment whose tag never landed still belongs to its thread by its id, and
// a comment tagged with a thread that isn't stored reads on its own.
func TestCommentsFindTheirThreads(t *testing.T) {
	res := accessBusiness.Resource{Kind: "doc", UUID: "d1"}
	at := func(m int) time.Time { return time.Date(2026, 10, 1, 0, m, 0, 0, time.UTC) }
	threads := []Thread{
		{ID: "t1", CreatedAt: at(1), CreatedBy: "ann", Status: StatusResolved},
		{ID: "c9", CreatedAt: at(2), CreatedBy: "bob", Status: StatusOpen}, // adopted, tag lost
		{ID: "t3", CreatedAt: at(3), CreatedBy: "ann", Status: StatusOpen}, // every comment deleted
	}
	comments := []Comment{
		{ID: "c2", ThreadID: "t1", CreatedAt: at(5), By: Person{UUID: "bob"}},
		{ID: "c1", ThreadID: "t1", CreatedAt: at(1), By: Person{UUID: "ann"}},
		{ID: "c9", CreatedAt: at(2), By: Person{UUID: "bob"}},
		{ID: "c5", ThreadID: "gone", CreatedAt: at(0), By: Person{UUID: "sam"}},
	}
	read := at(4)
	parts := map[string][]Participant{"t1": {{UserUUID: "ann", Role: RoleAuthor, LastReadAt: &read}}}
	views := assemble(res, threads, comments, parts, "ann", accessBusiness.Decision{Level: accessBusiness.LevelComment}, false)
	var ids []string
	for _, v := range views {
		ids = append(ids, v.ID)
	}
	if fmt.Sprint(ids) != "[c5 t1 c9]" {
		t.Fatalf("threads shown: %v", ids)
	}
	if v := views[1]; len(v.Comments) != 2 || v.Comments[0].ID != "c1" || !v.Unread || !v.CanResolve {
		t.Errorf("t1: %+v", v)
	}
	if v := views[2]; v.Legacy || len(v.Comments) != 1 || v.Comments[0].ThreadID != "c9" || v.CanResolve {
		t.Errorf("c9: %+v", v)
	}
	if v := views[0]; !v.Legacy || v.Unread || v.CreatedBy != "sam" || v.Anchor.Kind != AnchorNone {
		t.Errorf("c5: %+v", v)
	}
}

func TestReadingClearsUnreadAndReattachingIsExplicit(t *testing.T) {
	w := newWorld(t)
	v, _ := w.svc.Create(ctx, who("ann"), w.res, rangeAnchor(), Body{HTML: "<p>hi</p>"})
	if _, err := w.svc.Reply(ctx, who("bob"), v.ID, Body{HTML: "<p>yo</p>"}); err != nil {
		t.Fatal(err)
	}
	g, _ := w.svc.Get(ctx, who("ann"), v.ID)
	if !g.Unread {
		t.Fatal("ann has a reply she hasn't read")
	}
	if err := w.svc.MarkRead(ctx, who("ann"), v.ID); err != nil {
		t.Fatal(err)
	}
	if g, _ = w.svc.Get(ctx, who("ann"), v.ID); g.Unread {
		t.Error("still unread after reading")
	}
	// Someone who only read is a reader, and isn't told of what happens.
	if err := w.svc.MarkRead(ctx, who("viewer"), v.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.Resolve(ctx, who("owner"), v.ID); err != nil {
		t.Fatal(err)
	}
	if ev := w.ev.got[len(w.ev.got)-1]; fmt.Sprint(ev.Subjects) != "[ann bob]" {
		t.Errorf("resolving tells %v", ev.Subjects)
	}

	moved := Anchor{Kind: AnchorYRange, Start: []byte{9}, End: []byte{10}, Lineage: "L2", Quote: "Ship on Thursday"}
	got, err := w.svc.Reanchor(ctx, who("ann"), v.ID, moved)
	if err != nil || got.Anchor.Lineage != "L2" || string(got.Anchor.Start) != string([]byte{9}) {
		t.Fatalf("reattach: %+v, %v", got, err)
	}
	if ev := w.ev.got[len(w.ev.got)-1]; ev.Kind != ThreadReanchored {
		t.Errorf("last event %s", ev.Kind)
	}
	var inv *Invalid
	if _, err := w.svc.Reanchor(ctx, who("ann"), v.ID, Anchor{Kind: AnchorYRange}); !errors.As(err, &inv) {
		t.Errorf("a reattach with no range: %v", err)
	}
}
