package business

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	models "github.com/akashc777/OneCamp/models/postgres/Thread"
	"github.com/google/uuid"
)

// memComments keeps stored comments and their reactions in memory, with the
// Postgres store's rules: a removed comment is gone from every read and takes
// no change, and one reaction a person a comment.
type memComments struct {
	mu        sync.Mutex
	rows      []*models.Comment
	reactions []models.Reaction
}

func (m *memComments) install(t *testing.T) {
	t.Helper()
	oldWrite, oldRead, oldReactions := storedWrite, storedRead, storedReactions
	oldGet, oldEdit, oldRemove, oldSet, oldUnset, oldOne := commentGet, commentEdit, commentRemove, reactionSet, reactionRemove, reactionsOfOne
	oldPeople, oldMentioned := storedPeople, storedMentioned
	t.Cleanup(func() {
		storedWrite, storedRead, storedReactions = oldWrite, oldRead, oldReactions
		commentGet, commentEdit, commentRemove, reactionSet, reactionRemove, reactionsOfOne = oldGet, oldEdit, oldRemove, oldSet, oldUnset, oldOne
		storedPeople, storedMentioned = oldPeople, oldMentioned
	})
	storedWrite = func(_ context.Context, c *models.Comment) error {
		m.mu.Lock()
		defer m.mu.Unlock()
		cp := *c
		m.rows = append(m.rows, &cp)
		return nil
	}
	storedRead = func(_ context.Context, kind, res string) ([]*models.Comment, error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		var out []*models.Comment
		for _, r := range m.rows {
			if r.ResourceKind == kind && r.ResourceUUID == res && r.DeletedAt == nil {
				cp := *r
				out = append(out, &cp)
			}
		}
		return out, nil
	}
	reactions := func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID][]models.Reaction, error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		out := map[uuid.UUID][]models.Reaction{}
		for _, id := range ids {
			for _, r := range m.reactions {
				if r.CommentID == id {
					out[id] = append(out[id], r)
				}
			}
		}
		return out, nil
	}
	storedReactions, reactionsOfOne = reactions, reactions
	commentGet = func(_ context.Context, id uuid.UUID) (*models.Comment, error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, r := range m.rows {
			if r.ID == id {
				cp := *r
				return &cp, nil
			}
		}
		return nil, nil
	}
	commentEdit = func(_ context.Context, id uuid.UUID, body string, at time.Time) (bool, error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, r := range m.rows {
			if r.ID == id && r.DeletedAt == nil {
				r.Body, r.EditedAt = body, &at
				return true, nil
			}
		}
		return false, nil
	}
	commentRemove = func(_ context.Context, id, by uuid.UUID, at time.Time) (bool, error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, r := range m.rows {
			if r.ID == id && r.DeletedAt == nil {
				r.DeletedAt, r.DeletedBy = &at, &by
				return true, nil
			}
		}
		return false, nil
	}
	reactionSet = func(_ context.Context, id, comment, user uuid.UUID, emoji string, at time.Time) (uuid.UUID, bool, error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		for i, r := range m.reactions {
			if r.CommentID == comment && r.UserUUID == user {
				m.reactions[i].EmojiID = emoji
				return r.ID, true, nil
			}
		}
		m.reactions = append(m.reactions, models.Reaction{ID: id, CommentID: comment, UserUUID: user, EmojiID: emoji, CreatedAt: at})
		return id, true, nil
	}
	reactionRemove = func(_ context.Context, comment, reaction, user uuid.UUID) (bool, error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		for i, r := range m.reactions {
			if r.ID == reaction && r.CommentID == comment && r.UserUUID == user {
				m.reactions = append(m.reactions[:i], m.reactions[i+1:]...)
				return true, nil
			}
		}
		return false, nil
	}
	storedPeople = func(_ context.Context, ids []string) (map[string]*dgraphStruct.DgraphUser, error) {
		out := map[string]*dgraphStruct.DgraphUser{}
		for _, id := range ids {
			if name, ok := people[id]; ok {
				out[id] = &dgraphStruct.DgraphUser{Uuid: id, UserName: name}
			}
		}
		return out, nil
	}
	// Mentions name people by graph uid: "0x" and their uuid's first block.
	storedMentioned = func(_ context.Context, uids []string) ([]*dgraphStruct.DgraphUser, error) {
		var out []*dgraphStruct.DgraphUser
		for _, uid := range uids {
			for id := range people {
				if "0x"+id[:8] == uid {
					out = append(out, &dgraphStruct.DgraphUser{Uuid: id, Uid: uid})
				}
			}
		}
		return out, nil
	}
}

// The people of these tests, by uuid; "gone" is no longer in the workspace.
const (
	maya   = "11111111-1111-4111-8111-111111111111"
	bo     = "22222222-2222-4222-8222-222222222222"
	viewer = "33333333-3333-4333-8333-333333333333"
	owner  = "44444444-4444-4444-8444-444444444444"
	admin  = "55555555-5555-4555-8555-555555555555"
	gone   = "66666666-6666-4666-8666-666666666666"
	rowID  = "77777777-7777-4777-8777-777777777777"
)

var people = map[string]string{maya: "Maya", bo: "Bo", viewer: "Vik", owner: "Ola", admin: "Ada"}

// fakeRow is a table's row as threads see it: levels by person, its owner,
// and its comments stored here.
type fakeRow struct {
	StoredComments
	levels  map[string]accessBusiness.Level
	reasons map[string][]string
	deleted bool
}

func (f *fakeRow) Anchors() []AnchorKind { return []AnchorKind{AnchorNone, AnchorCell} }
func (f *fakeRow) Access(_ context.Context, who accessBusiness.Subject, _ accessBusiness.Resource) (accessBusiness.Decision, error) {
	l := f.levels[who.UserUUID]
	d := accessBusiness.Decision{Level: l, Owner: who.UserUUID == owner, Deleted: f.deleted, Reasons: f.reasons[who.UserUUID]}
	if f.deleted {
		d.Was, d.Level = l, accessBusiness.LevelNone
	}
	return d, nil
}
func (f *fakeRow) Post(ctx context.Context, who accessBusiness.Subject, res accessBusiness.Resource, threadID string, b Body) (Posted, error) {
	return f.Write(ctx, who, res, threadID, b)
}

type rowWorld struct {
	*world
	row      *fakeRow
	rowRes   accessBusiness.Resource
	comments *memComments
	changes  commentChanges
}

func newRowWorld(t *testing.T) *rowWorld {
	t.Helper()
	w := newWorld(t)
	rw := &rowWorld{world: w, comments: &memComments{}, rowRes: accessBusiness.Resource{Kind: "table_row", UUID: rowID}}
	rw.row = &fakeRow{
		StoredComments: StoredComments{Kind: "table_row"},
		levels: map[string]accessBusiness.Level{
			maya: accessBusiness.LevelEdit, bo: accessBusiness.LevelEdit, viewer: accessBusiness.LevelView,
			owner: accessBusiness.LevelFull, admin: accessBusiness.LevelFull, gone: accessBusiness.LevelEdit,
		},
		reasons: map[string][]string{admin: {"admin"}, owner: {"owner"}},
	}
	w.svc.resources.Register("table_row", rw.row)
	rw.comments.install(t)
	rw.changes = commentChanges{s: w.svc}
	n := 0
	oldID, oldClock := storedNewID, storedClock
	t.Cleanup(func() { storedNewID, storedClock = oldID, oldClock })
	storedNewID = func() string { n++; return uuid.NewSHA1(uuid.Nil, []byte{byte(n)}).String() }
	storedClock = w.svc.now
	return rw
}

func (rw *rowWorld) start(t *testing.T, by, html string) View {
	t.Helper()
	v, err := rw.svc.Create(context.Background(), who(by), rw.rowRes, Anchor{Kind: AnchorNone}, Body{HTML: html})
	if err != nil {
		t.Fatalf("%s starting a thread: %v", by, err)
	}
	return v
}

// A stored comment is read through the content codec before it's written: no
// scripts, no unsafe links, mentions kept, and the people it mentions read
// from what's stored.
func TestAStoredCommentIsSafeAndTellsWhoItMentions(t *testing.T) {
	rw := newRowWorld(t)
	html := `<p>Hi <script>alert(1)</script><a href="javascript:alert(2)">look</a> ` +
		`<span data-type="mention" data-id="` + bo + `@0x22222222" data-label="Bo">@Bo</span></p>`
	v := rw.start(t, maya, html)
	got := v.Comments[0].Text
	for _, bad := range []string{"<script", "alert(1)", "javascript:"} {
		if strings.Contains(got, bad) {
			t.Errorf("the stored body keeps %q: %s", bad, got)
		}
	}
	if !strings.Contains(got, `data-type="mention"`) || !strings.Contains(got, bo) {
		t.Errorf("the mention went: %s", got)
	}
	var told []string
	for _, e := range rw.ev.got {
		if e.Kind == ThreadCreated {
			told = e.Subjects
		}
	}
	if len(told) != 1 || told[0] != bo {
		t.Errorf("the new thread tells %v, want Bo alone", told)
	}
	// Nothing left to read once the codec has read it: refused, as an empty one is.
	if _, err := rw.svc.Create(context.Background(), who(maya), rw.rowRes, Anchor{Kind: AnchorNone}, Body{HTML: "<p><script>x()</script></p>"}); !isInvalid(err) {
		t.Errorf("a comment of only a script: %v", err)
	}
	// Someone mentioned who isn't in the workspace is refused, and nothing is written.
	before := len(rw.comments.rows)
	stranger := `<p><span data-type="mention" data-id="99999999-9999-4999-8999-999999999999@0x99999999">@Nobody</span> hi</p>`
	if _, err := rw.svc.Create(context.Background(), who(maya), rw.rowRes, Anchor{Kind: AnchorNone}, Body{HTML: stranger}); !isInvalid(err) {
		t.Errorf("a mention of a stranger: %v", err)
	}
	if len(rw.comments.rows) != before {
		t.Error("a refused comment was written")
	}
}

func isInvalid(err error) bool {
	var invalid *Invalid
	return errors.As(err, &invalid)
}

// Files aren't stored with a row's comments yet: refused, with words that say so.
func TestAStoredCommentRefusesFilesForNow(t *testing.T) {
	rw := newRowWorld(t)
	_, err := rw.svc.Create(context.Background(), who(maya), rw.rowRes, Anchor{Kind: AnchorNone},
		Body{HTML: "<p>see</p>", Attachments: json.RawMessage(`[{"attachment_obj_key":"k"}]`)})
	var invalid *Invalid
	if !errors.As(err, &invalid) || !strings.Contains(invalid.Msg, "Files") {
		t.Errorf("a comment with a file: %v", err)
	}
}

// A row's comments read with their writers' names and their reactions, in the
// doc comment's shape; a writer who left the workspace is a former member.
func TestStoredCommentsNameTheirPeople(t *testing.T) {
	rw := newRowWorld(t)
	v := rw.start(t, maya, "<p>One</p>")
	if _, err := rw.svc.Reply(context.Background(), who(gone), v.ID, Body{HTML: "<p>Two</p>"}); err != nil {
		t.Fatal(err)
	}
	// A reaction is an emoji's picker id and nothing else: it is served to
	// every reader (email and MCP too), so text and markup are refused.
	for _, bad := range []string{"<b>yes</b>", "looks good", ""} {
		if _, err := rw.changes.React(context.Background(), who(bo), v.ID, v.Comments[0].ID, bad); !isInvalid(err) {
			t.Errorf("reacting with %q: %v", bad, err)
		}
	}
	if _, err := rw.changes.React(context.Background(), who(bo), v.ID, v.Comments[0].ID, ":thumbs_up:"); err != nil {
		t.Fatal(err)
	}
	list, err := rw.svc.List(context.Background(), who(bo), rw.rowRes, Filter{})
	if err != nil || len(list) != 1 || len(list[0].Comments) != 2 {
		t.Fatalf("the row's threads: %+v, %v", list, err)
	}
	first, second := list[0].Comments[0], list[0].Comments[1]
	if first.By.Name != "Maya" || second.By.Name != formerMember {
		t.Errorf("writers %q and %q", first.By.Name, second.By.Name)
	}
	var reactions []map[string]any
	if err := json.Unmarshal(first.Reactions, &reactions); err != nil || len(reactions) != 1 {
		t.Fatalf("reactions %s: %v", first.Reactions, err)
	}
	r := reactions[0]
	by, _ := r["reaction_added_by"].(map[string]any)
	if r["uid"] == "" || r["reaction_emoji_id"] != "thumbs_up" || by["user_uuid"] != bo || by["user_name"] != "Bo" {
		t.Errorf("a reaction reads %v", r)
	}
	// One a person: reacting again changes the emoji.
	if _, err := rw.changes.React(context.Background(), who(bo), v.ID, first.ID, "heart"); err != nil {
		t.Fatal(err)
	}
	if n := len(rw.comments.reactions); n != 1 || rw.comments.reactions[0].EmojiID != "heart" {
		t.Errorf("reactions after a second: %+v", rw.comments.reactions)
	}
}

// Who may change a stored comment: its writer edits it; its writer removes it,
// and a moderator removes anyone's; anyone who may comment reacts. Each change
// is a Resource Event with ids alone.
func TestAStoredCommentChangesAsItsRulesSay(t *testing.T) {
	rw := newRowWorld(t)
	v := rw.start(t, maya, "<p>Draft</p>")
	c := v.Comments[0].ID
	bg := context.Background()

	if _, err := rw.changes.Edit(bg, who(bo), v.ID, c, Body{HTML: "<p>Mine now</p>"}); !isRefusal(err) {
		t.Errorf("someone else edits it: %v", err)
	}
	edited, err := rw.changes.Edit(bg, who(maya), v.ID, c, Body{HTML: `<p>Final <script>x()</script></p>`})
	if err != nil || strings.Contains(edited.Text, "script") || edited.UpdatedAt == nil {
		t.Errorf("its writer edits it: %+v, %v", edited, err)
	}
	if !emitted(rw.ev, ThreadCommentEdited, c) {
		t.Error("an edit wasn't told to the row's open tabs")
	}
	if _, err := rw.changes.React(bg, who(viewer), v.ID, c, "eyes"); !isRefusal(err) {
		t.Errorf("someone who may only view reacts: %v", err)
	}
	if _, err := rw.changes.Remove(bg, who(bo), v.ID, c); !isRefusal(err) {
		t.Errorf("someone else deletes it: %v", err)
	}
	if _, err := rw.changes.Edit(bg, who(maya), "00000000-0000-0000-0000-000000000099", c, Body{HTML: "<p>x</p>"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("named through a thread it isn't in: %v", err)
	}

	removed, err := rw.changes.Remove(bg, who(maya), v.ID, c)
	if err != nil || removed.Moderated || removed.ByAdmin {
		t.Errorf("its writer deletes it: %+v, %v", removed, err)
	}
	if _, err := rw.changes.Edit(bg, who(maya), v.ID, c, Body{HTML: "<p>back</p>"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("a deleted comment edited: %v", err)
	}

	// The row's owner removes anyone's; an admin does too, and that is an admin act.
	two := rw.start(t, bo, "<p>Off topic</p>")
	removed, err = rw.changes.Remove(bg, who(owner), two.ID, two.Comments[0].ID)
	if err != nil || !removed.Moderated || removed.ByAdmin {
		t.Errorf("the owner removes Bo's: %+v, %v", removed, err)
	}
	three := rw.start(t, bo, "<p>Again</p>")
	removed, err = rw.changes.Remove(asUser(admin, true), who(admin), three.ID, three.Comments[0].ID)
	if err != nil || !removed.Moderated || !removed.ByAdmin {
		t.Errorf("an admin removes Bo's: %+v, %v", removed, err)
	}

	// A doc's comments change through the doc: unavailable here.
	doc, err := rw.svc.Create(ctx, who("ann"), rw.res, rangeAnchor(), Body{HTML: "<p>x</p>"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rw.changes.Edit(ctx, who("ann"), doc.ID, doc.Comments[0].ID, Body{HTML: "<p>y</p>"}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("a doc comment edited here: %v", err)
	}

	// A row in the trash takes no change.
	four := rw.start(t, maya, "<p>Later</p>")
	rw.row.deleted = true
	if _, err := rw.changes.React(bg, who(maya), four.ID, four.Comments[0].ID, "eyes"); !errors.Is(err, ErrGone) {
		t.Errorf("a reaction on a row in the trash: %v", err)
	}
}

func isRefusal(err error) bool {
	var refusal *Refusal
	return errors.As(err, &refusal)
}

func emitted(ev *events, kind resourceEventBusiness.Kind, commentID string) bool {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	for _, e := range ev.got {
		if e.Kind == kind && e.Data["comment_id"] == commentID && e.Data["body"] == "" {
			return true
		}
	}
	return false
}

// Only a level that comes from being an admin, and nothing else, makes a
// removal an admin act.
func TestOnlyAnAdminsOwnSayIsAnAdminAct(t *testing.T) {
	for _, c := range []struct {
		reasons []string
		want    bool
	}{
		{[]string{"admin"}, true},
		{[]string{"admin", "workspace"}, false},
		{[]string{"owner"}, false},
		{nil, false},
	} {
		if got := onlyAsAdmin(accessBusiness.Decision{Reasons: c.reasons}); got != c.want {
			t.Errorf("%v: %v", c.reasons, got)
		}
	}
}
