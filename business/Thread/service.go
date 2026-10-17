package business

import (
	"context"
	"errors"
	"sort"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	commentBusiness "github.com/akashc777/OneCamp/business/Comment"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// service is the threads service over a store and the registered resources.
type service struct {
	store     store
	resources *Registry
	now       func() time.Time
	newID     func() string
}

func newService(s store, r *Registry) *service {
	return &service{store: s, resources: r, now: func() time.Time { return time.Now().UTC() }, newID: uuid.NewString}
}

// resource is the adapter for a kind, or ErrUnavailable.
func (s *service) resource(kind string) (Resource, error) {
	r, ok := s.resources.of(kind)
	if !ok {
		return nil, ErrUnavailable
	}
	return r, nil
}

// decide asks the resource for who's decision and applies the rule for a.
func (s *service) decide(ctx context.Context, r Resource, who accessBusiness.Subject, res accessBusiness.Resource, a act, author bool) (accessBusiness.Decision, error) {
	d, err := r.Access(ctx, who, res)
	if err != nil {
		return d, err
	}
	return d, allowed(d, a, author)
}

func (s *service) Create(ctx context.Context, who accessBusiness.Subject, res accessBusiness.Resource, anchor Anchor, body Body) (View, error) {
	r, err := s.resource(res.Kind)
	if err != nil {
		return View{}, err
	}
	if anchor, err = validAnchor(anchor, r.Anchors()); err != nil {
		return View{}, err
	}
	if body, err = validBody(body); err != nil {
		return View{}, err
	}
	d, err := s.decide(ctx, r, who, res, actPost, true)
	if err != nil {
		return View{}, err
	}
	now := s.now()
	t := Thread{
		ID: s.newID(), ResourceKind: res.Kind, ResourceUUID: res.UUID, Anchor: anchor,
		Status: StatusOpen, CreatedBy: who.UserUUID, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := s.store.insert(ctx, t, false); err != nil {
		return View{}, err
	}
	posted, err := r.Post(ctx, who, res, t.ID, body)
	if err != nil {
		// Nothing was said, so there is no thread.
		if rerr := s.store.remove(context.WithoutCancel(ctx), t.ID); rerr != nil {
			helpers.LogErrorWithContext(ctx, "business/Thread Create: removing empty thread %s failed: %+v", t.ID, rerr)
		}
		return View{}, err
	}
	s.joinAll(ctx, t.ID, who.UserUUID, RoleAuthor, posted.Mentioned, now)
	s.emit(ctx, ThreadCreated, t, who, without(posted.Mentioned, who.UserUUID), posted.Comment.ID)
	ps, _ := s.store.participants(ctx, t.ID)
	return View{
		Thread: t, Comments: []Comment{posted.Comment}, Participants: ps,
		CanReply: true, CanResolve: allowed(d, actSettle, true) == nil, CanModerate: mayModerate(ctx, who, d),
	}, nil
}

// joinAll records the writer (read up to now) and whoever they mentioned.
func (s *service) joinAll(ctx context.Context, threadID, writer string, role Role, mentioned []string, now time.Time) {
	if err := s.store.join(ctx, threadID, writer, role, &now); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Thread: recording %s in thread %s failed: %+v", writer, threadID, err)
	}
	for _, m := range mentioned {
		if m == writer {
			continue
		}
		if err := s.store.join(ctx, threadID, m, RoleMentioned, nil); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Thread: recording mention of %s in thread %s failed: %+v", m, threadID, err)
		}
	}
}

func (s *service) Reply(ctx context.Context, who accessBusiness.Subject, threadID string, body Body) (Comment, error) {
	body, err := validBody(body)
	if err != nil {
		return Comment{}, err
	}
	t, r, err := s.writable(ctx, who, threadID, actPost)
	if err != nil {
		return Comment{}, err
	}
	// A reply's comment is told by the thread's own event (thread.replied: the
	// thread's people and the doc's followers), not doc-wide as a page's
	// comment is, so nobody hears of it twice (business/Comment ThreadReplyKey).
	posted, err := r.Post(context.WithValue(ctx, commentBusiness.ThreadReplyKey, t.ID), who, resOf(t), t.ID, body)
	if err != nil {
		return Comment{}, err
	}
	now := s.now()
	if err := s.store.touch(ctx, t.ID, now); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Thread Reply: touching %s failed: %+v", t.ID, err)
	}
	before, _ := s.store.participants(ctx, t.ID)
	s.joinAll(ctx, t.ID, who.UserUUID, RoleReplier, posted.Mentioned, now)
	s.emit(ctx, ThreadReplied, t, who, without(append(takingPart(before), posted.Mentioned...), who.UserUUID), posted.Comment.ID)
	return posted.Comment, nil
}

func (s *service) Resolve(ctx context.Context, who accessBusiness.Subject, threadID string) (Thread, error) {
	return s.settle(ctx, who, threadID, StatusResolved)
}

func (s *service) Reopen(ctx context.Context, who accessBusiness.Subject, threadID string) (Thread, error) {
	return s.settle(ctx, who, threadID, StatusOpen)
}

func (s *service) settle(ctx context.Context, who accessBusiness.Subject, threadID string, to Status) (Thread, error) {
	t, _, err := s.writable(ctx, who, threadID, actSettle)
	if err != nil {
		return Thread{}, err
	}
	by := ""
	if to == StatusResolved {
		by = who.UserUUID
	}
	got, changed, err := s.store.setStatus(ctx, t.ID, to, by, s.now())
	if err != nil {
		return Thread{}, err
	}
	if got == nil {
		return Thread{}, ErrNotFound
	}
	if changed {
		kind := ThreadReopened
		if to == StatusResolved {
			kind = ThreadResolved
		}
		ps, _ := s.store.participants(ctx, t.ID)
		s.emit(ctx, kind, *got, who, without(takingPart(ps), who.UserUUID), "")
	}
	return *got, nil
}

func (s *service) Reanchor(ctx context.Context, who accessBusiness.Subject, threadID string, anchor Anchor) (Thread, error) {
	t, r, err := s.loadAllowed(ctx, who, threadID, actSettle)
	if err != nil {
		return Thread{}, err
	}
	if anchor, err = validAnchor(anchor, r.Anchors()); err != nil {
		return Thread{}, err
	}
	if err := s.adoptIfLegacy(ctx, r, &t); err != nil {
		return Thread{}, err
	}
	got, err := s.store.setAnchor(ctx, t.ID, anchor, s.now())
	if err != nil {
		return Thread{}, err
	}
	if got == nil {
		return Thread{}, ErrNotFound
	}
	ps, _ := s.store.participants(ctx, t.ID)
	s.emit(ctx, ThreadReanchored, *got, who, without(takingPart(ps), who.UserUUID), "")
	return *got, nil
}

func (s *service) MarkRead(ctx context.Context, who accessBusiness.Subject, threadID string) error {
	t, _, err := s.loadAllowed(ctx, who, threadID, actRead)
	if err != nil {
		return err
	}
	if t.legacy {
		return nil // a comment from before threads is never unread
	}
	now := s.now()
	return s.store.join(ctx, t.ID, who.UserUUID, RoleReader, &now)
}

func (s *service) List(ctx context.Context, who accessBusiness.Subject, res accessBusiness.Resource, f Filter) ([]View, error) {
	r, err := s.resource(res.Kind)
	if err != nil {
		return nil, err
	}
	d, err := s.decide(ctx, r, who, res, actRead, false)
	if err != nil {
		return nil, err
	}
	// Every thread, open or resolved, so a resolved thread's comments are
	// never mistaken for comments in no thread; the filter comes after.
	threads, err := s.store.list(ctx, res.Kind, res.UUID)
	if err != nil {
		return nil, err
	}
	comments, err := r.Comments(ctx, who, res)
	if err != nil {
		return nil, err
	}
	parts, err := s.store.participantsOf(ctx, res.Kind, res.UUID)
	if err != nil {
		return nil, err
	}
	views := assemble(res, threads, comments, parts, who.UserUUID, d, mayModerate(ctx, who, d))
	out := views[:0]
	for _, v := range views {
		if f.Status == "" || v.Status == f.Status {
			out = append(out, v)
		}
	}
	return out, nil
}

func (s *service) Get(ctx context.Context, who accessBusiness.Subject, threadID string) (View, error) {
	t, r, err := s.loadAllowed(ctx, who, threadID, actRead)
	if err != nil {
		return View{}, err
	}
	res := resOf(t.Thread)
	d, err := r.Access(ctx, who, res)
	if err != nil {
		return View{}, err
	}
	comments, err := r.Comments(ctx, who, res)
	if err != nil {
		return View{}, err
	}
	var threads []Thread
	parts := map[string][]Participant{}
	if !t.legacy {
		threads = []Thread{t.Thread}
		if parts[t.ID], err = s.store.participants(ctx, t.ID); err != nil {
			return View{}, err
		}
	}
	for _, v := range assemble(res, threads, comments, parts, who.UserUUID, d, mayModerate(ctx, who, d)) {
		if v.ID == t.ID {
			return v, nil
		}
	}
	return View{}, ErrNotFound // every comment in it is gone
}

// loaded is a thread, and whether it is a legacy comment not yet a thread.
type loaded struct {
	Thread
	legacy bool
	root   Comment
}

// load finds a thread by id: a stored one, or a legacy comment read as one.
func (s *service) load(ctx context.Context, threadID string) (loaded, Resource, error) {
	if _, err := uuid.Parse(threadID); err != nil {
		return loaded{}, nil, ErrNotFound
	}
	t, err := s.store.get(ctx, threadID)
	if err != nil {
		return loaded{}, nil, err
	}
	if t != nil {
		r, err := s.resource(t.ResourceKind)
		return loaded{Thread: *t}, r, err
	}
	for _, r := range s.resources.kinds() {
		lt, root, ok, err := r.Legacy(ctx, threadID)
		if err != nil {
			return loaded{}, nil, err
		}
		if ok {
			return loaded{Thread: lt, legacy: true, root: root}, r, nil
		}
	}
	return loaded{}, nil, ErrNotFound
}

// loadAllowed loads a thread and checks who may do a with it.
func (s *service) loadAllowed(ctx context.Context, who accessBusiness.Subject, threadID string, a act) (loaded, Resource, error) {
	t, r, err := s.load(ctx, threadID)
	if err != nil {
		return loaded{}, nil, err
	}
	if _, err := s.decide(ctx, r, who, resOf(t.Thread), a, t.CreatedBy == who.UserUUID); err != nil {
		return loaded{}, nil, err
	}
	return t, r, nil
}

// writable loads a thread who may do a with, making a legacy comment a
// thread first: a write needs a row.
func (s *service) writable(ctx context.Context, who accessBusiness.Subject, threadID string, a act) (Thread, Resource, error) {
	t, r, err := s.loadAllowed(ctx, who, threadID, a)
	if err != nil {
		return Thread{}, nil, err
	}
	if err := s.adoptIfLegacy(ctx, r, &t); err != nil {
		return Thread{}, nil, err
	}
	return t.Thread, r, nil
}

// adoptIfLegacy makes a legacy comment a page-level thread under the
// comment's own id. Two people doing so at once make one thread.
func (s *service) adoptIfLegacy(ctx context.Context, r Resource, t *loaded) error {
	if !t.legacy {
		return nil
	}
	if _, err := s.store.insert(ctx, t.Thread, true); err != nil {
		return err
	}
	if t.CreatedBy != "" {
		read := t.CreatedAt
		if err := s.store.join(ctx, t.ID, t.CreatedBy, RoleAuthor, &read); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Thread: recording the author of %s failed: %+v", t.ID, err)
		}
	}
	// The tag is tidiness: the comment is the thread's by its id already.
	if err := r.Claim(ctx, t.ID, t.root); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Thread: tagging legacy comment %s failed: %+v", t.ID, err)
	}
	stored, err := s.store.get(ctx, t.ID)
	if err != nil {
		return err
	}
	if stored == nil {
		return errors.New("threads: an adopted thread was not stored")
	}
	t.Thread, t.legacy = *stored, false
	return nil
}

// legacyThread is the page-level thread a comment from before threads reads
// as: its id the comment's, open, started by its author when it was written.
func legacyThread(kind, resourceUUID string, root Comment) Thread {
	return Thread{
		ID: root.ID, ResourceKind: kind, ResourceUUID: resourceUUID, Anchor: Anchor{Kind: AnchorNone},
		Status: StatusOpen, CreatedBy: root.By.UUID, CreatedAt: root.CreatedAt, UpdatedAt: root.CreatedAt,
	}
}

// assemble groups a resource's comments into its threads, as viewer sees
// them. A comment belongs to the thread it is tagged with; a comment with no
// tag whose id is a thread's is that thread's first (a legacy comment made a
// thread whose tag didn't land); any other comment in no known thread reads
// as a page-level thread of its own. Threads with no comments left are not
// shown. Oldest first, by when each was started. moderate is whether the
// viewer may remove anyone's comment (mayModerate). Pure.
func assemble(res accessBusiness.Resource, threads []Thread, comments []Comment, parts map[string][]Participant, viewer string, d accessBusiness.Decision, moderate bool) []View {
	byID := make(map[string]*View, len(threads))
	order := make([]*View, 0, len(threads))
	for _, t := range threads {
		v := &View{Thread: t, Participants: parts[t.ID]}
		byID[t.ID] = v
		order = append(order, v)
	}
	sort.SliceStable(comments, func(i, j int) bool { return comments[i].CreatedAt.Before(comments[j].CreatedAt) })
	for _, c := range comments {
		home := byID[c.ThreadID]
		if home == nil {
			home = byID[c.ID]
		}
		if home == nil {
			lt := legacyThread(res.Kind, res.UUID, c)
			home = &View{Thread: lt, Legacy: true}
			byID[lt.ID] = home
			order = append(order, home)
		}
		c.ThreadID = home.ID
		home.Comments = append(home.Comments, c)
	}
	out := make([]View, 0, len(order))
	for _, v := range order {
		if len(v.Comments) == 0 {
			continue
		}
		if v.Participants == nil {
			v.Participants = []Participant{}
		}
		v.Unread = !v.Legacy && unread(v.Comments, v.Participants, viewer)
		v.CanReply = allowed(d, actPost, false) == nil
		v.CanResolve = allowed(d, actSettle, v.CreatedBy == viewer) == nil
		v.CanModerate = moderate
		out = append(out, *v)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// unread is whether someone other than viewer commented after viewer last
// read the thread (never, if they never opened it).
func unread(comments []Comment, parts []Participant, viewer string) bool {
	var read time.Time
	for _, p := range parts {
		if p.UserUUID == viewer && p.LastReadAt != nil {
			read = *p.LastReadAt
		}
	}
	for _, c := range comments {
		if c.By.UUID != viewer && c.CreatedAt.After(read) {
			return true
		}
	}
	return false
}

// takingPart is who takes part beyond reading: told of what happens.
func takingPart(ps []Participant) []string {
	var out []string
	for _, p := range ps {
		if p.Role != RoleReader {
			out = append(out, p.UserUUID)
		}
	}
	return out
}

// without is ids, each once, in order, leaving out one.
func without(ids []string, drop string) []string {
	seen := map[string]bool{drop: true, "": true}
	var out []string
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func resOf(t Thread) accessBusiness.Resource {
	return accessBusiness.Resource{Kind: t.ResourceKind, UUID: t.ResourceUUID}
}
