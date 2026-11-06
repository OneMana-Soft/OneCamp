package business

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	v1api "github.com/akashc777/OneCamp/business/PublicAPI"
	tableAccess "github.com/akashc777/OneCamp/business/TableAccess"
	threadBusiness "github.com/akashc777/OneCamp/business/Thread"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// fakeThreads is the threads service as a row's comments ops call it: each
// call, what it was asked, and what it answers (errs by call name).
type fakeThreads struct {
	threadBusiness.Service // a call the ops shouldn't make panics
	calls                  []string
	who                    []accessBusiness.Subject
	res                    []accessBusiness.Resource
	anchors                []threadBusiness.Anchor
	bodies                 []threadBusiness.Body
	filters                []threadBusiness.Filter
	threads                []string
	got                    threadBusiness.View
	listed                 []threadBusiness.View
	errs                   map[string]error
}

func (f *fakeThreads) asked(call string, who accessBusiness.Subject) error {
	f.calls, f.who = append(f.calls, call), append(f.who, who)
	return f.errs[call]
}

func (f *fakeThreads) List(_ context.Context, who accessBusiness.Subject, res accessBusiness.Resource, flt threadBusiness.Filter) ([]threadBusiness.View, error) {
	f.res, f.filters = append(f.res, res), append(f.filters, flt)
	if err := f.asked("List", who); err != nil {
		return nil, err
	}
	return f.listed, nil
}

func (f *fakeThreads) Create(_ context.Context, who accessBusiness.Subject, res accessBusiness.Resource, anchor threadBusiness.Anchor, body threadBusiness.Body) (threadBusiness.View, error) {
	f.res, f.anchors, f.bodies = append(f.res, res), append(f.anchors, anchor), append(f.bodies, body)
	if err := f.asked("Create", who); err != nil {
		return threadBusiness.View{}, err
	}
	t := threadBusiness.Thread{ID: "t-new", ResourceKind: res.Kind, ResourceUUID: res.UUID, Anchor: anchor}
	return threadBusiness.View{Thread: t, Comments: []threadBusiness.Comment{{ID: "c-first", ThreadID: t.ID, Text: body.HTML}}}, nil
}

func (f *fakeThreads) Get(_ context.Context, who accessBusiness.Subject, threadID string) (threadBusiness.View, error) {
	f.threads = append(f.threads, threadID)
	if err := f.asked("Get", who); err != nil {
		return threadBusiness.View{}, err
	}
	return f.got, nil
}

func (f *fakeThreads) Reply(_ context.Context, who accessBusiness.Subject, threadID string, body threadBusiness.Body) (threadBusiness.Comment, error) {
	f.threads, f.bodies = append(f.threads, threadID), append(f.bodies, body)
	if err := f.asked("Reply", who); err != nil {
		return threadBusiness.Comment{}, err
	}
	return threadBusiness.Comment{ID: "c-reply", ThreadID: threadID, Text: body.HTML}, nil
}

// commentWorld is a row's comments ops with their seams set: the threads
// service, where rows live, the row's decision and the table's fields.
type commentWorld struct {
	threads            *fakeThreads
	member, table, row uuid.UUID
	field              uuid.UUID
	agent              string
	homes              map[uuid.UUID]model.RowHome
	homesErr           error
	level              accessBusiness.Level
	fieldReads         int
}

func newCommentWorld(t *testing.T) *commentWorld {
	t.Helper()
	w := &commentWorld{threads: &fakeThreads{}, member: uuid.New(), table: uuid.New(), row: uuid.New(), field: uuid.New(),
		agent: uuid.NewString(), level: accessBusiness.LevelComment}
	w.homes = map[uuid.UUID]model.RowHome{w.row: {Table: w.table}}
	oldThreads, oldHomes, oldDecide, oldFields := threadBusiness.Threads, rowHomes, decideRow, fieldsOf
	t.Cleanup(func() {
		threadBusiness.Threads, rowHomes, decideRow, fieldsOf = oldThreads, oldHomes, oldDecide, oldFields
	})
	threadBusiness.Threads = w.threads
	rowHomes = func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]model.RowHome, error) {
		out := map[uuid.UUID]model.RowHome{}
		for _, id := range ids {
			if h, ok := w.homes[id]; ok {
				out[id] = h
			}
		}
		return out, w.homesErr
	}
	decideRow = func(context.Context, accessBusiness.Subject, uuid.UUID) (accessBusiness.Decision, error) {
		return accessBusiness.Decision{Level: w.level}, nil
	}
	fieldsOf = func(_ context.Context, table uuid.UUID) ([]*model.Field, error) {
		w.fieldReads++
		return []*model.Field{{Id: w.field, TableId: table, Name: "Status"}}, nil
	}
	return w
}

// ctx is a call through a token of the member's, an admin, bound to the
// world's agent: the token's member acts, never with the admin's powers.
func (w *commentWorld) ctx() context.Context {
	info := userModels.UserInfo{}
	info.UserPostgresInfo.Id, info.UserPostgresInfo.IsAdmin = w.member, true
	info.UserDgraphInfo.Uuid, info.UserDgraphInfo.Uid = w.member.String(), "0x2a"
	ctx := context.WithValue(context.Background(), helpers.UserInfoContextKey, info)
	ctx = context.WithValue(ctx, helpers.ApiTokenIDContextKey, uuid.NewString())
	return context.WithValue(ctx, helpers.ApiAgentIDContextKey, w.agent)
}

func (w *commentWorld) serve(t *testing.T, name string, args map[string]any) (any, error) {
	t.Helper()
	for _, op := range APIOps() {
		if op.Name == name {
			all := map[string]any{"id": w.table.String(), "row": w.row.String()}
			for k, v := range args {
				all[k] = v
			}
			return op.Serve(w.ctx(), v1api.Call{Args: all})
		}
	}
	t.Fatalf("no op %s", name)
	return nil, nil
}

func refusedAs(t *testing.T, err error, status int, message string) {
	t.Helper()
	e := asAPIError(t, err)
	if e.Status != status || !strings.Contains(e.Message, message) {
		t.Fatalf("refused %d %q, want %d with %q", e.Status, e.Message, status, message)
	}
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A row's comments are its threads as the token's member sees them in the app:
// asked of the row (the row's own table decides), as the member without their
// admin powers and as the agent the token is bound to, open or resolved as
// asked; none is an empty list.
func TestARowsCommentsAreItsThreadsAsTheTokensMemberSeesThem(t *testing.T) {
	w := newCommentWorld(t)
	w.threads.listed = []threadBusiness.View{{Thread: threadBusiness.Thread{ID: "t1"}}}
	got, err := w.serve(t, "tables.row.comments.list", map[string]any{"status": "open"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(w.threads.calls, []string{"List"}) {
		t.Fatalf("threads asked %v, want [List]", w.threads.calls)
	}
	if want := (accessBusiness.Resource{Kind: tableAccess.KindTableRow, UUID: w.row.String()}); w.threads.res[0] != want {
		t.Fatalf("asked of %+v, want %+v", w.threads.res[0], want)
	}
	if who := w.threads.who[0]; who.Admin || who.AgentUUID != w.agent || who.UserUUID != w.member.String() || who.UserUID != "0x2a" {
		t.Fatalf("asked as %+v, want the member %s without admin powers, as agent %s", who, w.member, w.agent)
	}
	if w.threads.filters[0].Status != threadBusiness.StatusOpen {
		t.Fatalf("filter %+v, want open threads", w.threads.filters[0])
	}
	if s := jsonOf(t, got); !strings.HasPrefix(s, `{"threads":[{"id":"t1"`) {
		t.Fatalf("answered %s", s)
	}
	w.threads.listed = nil
	got, err = w.serve(t, "tables.row.comments.list", nil)
	if err != nil {
		t.Fatal(err)
	}
	if s := jsonOf(t, got); s != `{"threads":[]}` {
		t.Fatalf("no threads answered %s, want an empty list", s)
	}
	if w.threads.filters[1].Status != "" {
		t.Fatalf("no status asked %+v, want both", w.threads.filters[1])
	}
}

// A call names a table and a row, and the row's own table decides who may
// comment: so a row the named table doesn't hold is no row of it, and the
// threads are never asked. A token held to one table never reaches the
// comments of another's rows.
func TestARowTheTableDoesntHoldHasNoCommentsThere(t *testing.T) {
	for _, tc := range []struct {
		name  string
		homes func(w *commentWorld) map[uuid.UUID]model.RowHome
	}{
		{"another table's row", func(w *commentWorld) map[uuid.UUID]model.RowHome {
			return map[uuid.UUID]model.RowHome{w.row: {Table: uuid.New()}}
		}},
		{"no such row", func(*commentWorld) map[uuid.UUID]model.RowHome { return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newCommentWorld(t)
			w.homes = tc.homes(w)
			_, err := w.serve(t, "tables.row.comments.list", nil)
			refusedAs(t, err, http.StatusNotFound, "there is no such row")
			_, err = w.serve(t, "tables.row.comment.create", map[string]any{"text": "hi"})
			refusedAs(t, err, http.StatusNotFound, "there is no such row")
			_, err = w.serve(t, "tables.row.comment.create", map[string]any{"text": "hi", "thread": "t1"})
			refusedAs(t, err, http.StatusNotFound, "there is no such row")
			if len(w.threads.calls) != 0 {
				t.Fatalf("threads asked %v, want nothing", w.threads.calls)
			}
		})
	}
	w := newCommentWorld(t)
	w.homesErr = errors.New("connection refused")
	_, err := w.serve(t, "tables.row.comments.list", nil)
	refusedAs(t, err, http.StatusServiceUnavailable, "couldn't be read")
	if len(w.threads.calls) != 0 {
		t.Fatalf("threads asked %v after the row couldn't be read", w.threads.calls)
	}
}

// A comment without a thread starts one, on the row or, given a field of the
// row's table, on that cell; its text is plain text, made the comment's HTML
// as every API text is (escaped; paragraphs and lists). The answer is the
// comment made, with its thread's id.
func TestACommentStartsAThreadOnTheRowOrOneOfItsCells(t *testing.T) {
	w := newCommentWorld(t)
	text := "Ship it <now> & tell Bo\n\n- price\n- date"
	got, err := w.serve(t, "tables.row.comment.create", map[string]any{"text": text})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(w.threads.calls, []string{"Create"}) || w.threads.anchors[0].Kind != threadBusiness.AnchorNone {
		t.Fatalf("asked %v with %+v, want Create on the row as a whole", w.threads.calls, w.threads.anchors)
	}
	if html := w.threads.bodies[0].HTML; html != helpers.PlainTextToHTML(text) || !strings.Contains(html, "&lt;now&gt;") {
		t.Fatalf("body %q, want the text as escaped HTML", html)
	}
	if who := w.threads.who[0]; who.Admin || who.AgentUUID != w.agent {
		t.Fatalf("commented as %+v, want the member without admin powers, as the agent", who)
	}
	if c, ok := got.(threadBusiness.Comment); !ok || c.ID != "c-first" || c.ThreadID != "t-new" {
		t.Fatalf("answered %#v, want the thread's first comment", got)
	}

	if _, err := w.serve(t, "tables.row.comment.create", map[string]any{"text": "Is this right?", "field": w.field.String()}); err != nil {
		t.Fatal(err)
	}
	if a := w.threads.anchors[1]; a.Kind != threadBusiness.AnchorCell || string(a.Meta) != `{"field":"`+w.field.String()+`"}` {
		t.Fatalf("anchored %+v (meta %s), want the cell of field %s", a, a.Meta, w.field)
	}

	_, err = w.serve(t, "tables.row.comment.create", map[string]any{"text": "Where?", "field": uuid.NewString()})
	refusedAs(t, err, http.StatusNotFound, "there is no such field")
	if len(w.threads.calls) != 2 {
		t.Fatalf("threads asked %v for a field the table hasn't got", w.threads.calls)
	}

	for _, blank := range []string{"", "  \n\n "} {
		_, err = w.serve(t, "tables.row.comment.create", map[string]any{"text": blank})
		refusedAs(t, err, http.StatusBadRequest, "write the comment")
	}
	if len(w.threads.calls) != 2 {
		t.Fatalf("threads asked %v for a blank comment", w.threads.calls)
	}
}

// A cell's field is checked only for someone who may open the row: for anyone
// else the threads service refuses the comment in its own words, and the
// table's fields aren't read, so the answer never says which fields a table
// they can't open has.
func TestACellsFieldIsCheckedOnlyForSomeoneWhoMayOpenTheRow(t *testing.T) {
	w := newCommentWorld(t)
	w.level = accessBusiness.LevelNone
	w.threads.errs = map[string]error{"Create": threadBusiness.ErrNotFound}
	_, err := w.serve(t, "tables.row.comment.create", map[string]any{"text": "hi", "field": uuid.NewString()})
	refusedAs(t, err, http.StatusNotFound, "there is no such row")
	if w.fieldReads != 0 {
		t.Fatalf("the table's fields were read %d times for someone who can't open the row", w.fieldReads)
	}
	if !slices.Equal(w.threads.calls, []string{"Create"}) {
		t.Fatalf("threads asked %v, want Create to refuse it", w.threads.calls)
	}
}

// A comment with a thread is a reply in it, and only in a thread on this row:
// a thread elsewhere is no thread here, and a reply never moves its thread to
// a cell.
func TestAReplyJoinsItsThreadOnlyOnThisRow(t *testing.T) {
	w := newCommentWorld(t)
	w.threads.got = threadBusiness.View{Thread: threadBusiness.Thread{ID: "t1", ResourceKind: tableAccess.KindTableRow, ResourceUUID: w.row.String()}}
	got, err := w.serve(t, "tables.row.comment.create", map[string]any{"text": "Done", "thread": "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(w.threads.calls, []string{"Get", "Reply"}) || !slices.Equal(w.threads.threads, []string{"t1", "t1"}) {
		t.Fatalf("asked %v of %v, want Get then Reply on t1", w.threads.calls, w.threads.threads)
	}
	if who := w.threads.who[1]; who.Admin || who.AgentUUID != w.agent {
		t.Fatalf("replied as %+v, want the member without admin powers, as the agent", who)
	}
	if c, ok := got.(threadBusiness.Comment); !ok || c.ID != "c-reply" || c.ThreadID != "t1" || c.Text != "<p>Done</p>" {
		t.Fatalf("answered %#v, want the reply", got)
	}

	for _, elsewhere := range []threadBusiness.Thread{
		{ID: "t2", ResourceKind: tableAccess.KindTableRow, ResourceUUID: uuid.NewString()},
		{ID: "t2", ResourceKind: "doc", ResourceUUID: w.row.String()},
	} {
		w.threads.got = threadBusiness.View{Thread: elsewhere}
		w.threads.calls = nil
		_, err = w.serve(t, "tables.row.comment.create", map[string]any{"text": "Done", "thread": "t2"})
		refusedAs(t, err, http.StatusNotFound, "there is no such thread on this row")
		if !slices.Equal(w.threads.calls, []string{"Get"}) {
			t.Fatalf("asked %v for a thread on %s %s, want no reply", w.threads.calls, elsewhere.ResourceKind, elsewhere.ResourceUUID)
		}
	}

	w.threads.calls = nil
	_, err = w.serve(t, "tables.row.comment.create", map[string]any{"text": "Done", "thread": "t1", "field": w.field.String()})
	refusedAs(t, err, http.StatusBadRequest, "leave field out")
	if len(w.threads.calls) != 0 {
		t.Fatalf("threads asked %v for a reply with a field", w.threads.calls)
	}
}

// The threads service's refusals answer in the API's one error body, in its
// own words where it has them, as the app's threads controller answers them.
func TestTheThreadsRefusalsAnswerInTheAPIsErrorBody(t *testing.T) {
	for _, tc := range []struct {
		err     error
		status  int
		message string
	}{
		{&threadBusiness.Invalid{Msg: "That comment is too long."}, http.StatusBadRequest, "That comment is too long."},
		{&threadBusiness.Refusal{Msg: "You can only read this table."}, http.StatusForbidden, "You can only read this table."},
		{threadBusiness.ErrForbidden, http.StatusForbidden, "may not comment"},
		{threadBusiness.ErrNotFound, http.StatusNotFound, "there is no such row"},
		{threadBusiness.ErrGone, http.StatusGone, "in its table's trash"},
		{errors.New("pq: connection reset"), http.StatusServiceUnavailable, "couldn't be read or saved"},
	} {
		w := newCommentWorld(t)
		w.threads.errs = map[string]error{"List": tc.err, "Create": tc.err}
		_, err := w.serve(t, "tables.row.comments.list", nil)
		refusedAs(t, err, tc.status, tc.message)
		_, err = w.serve(t, "tables.row.comment.create", map[string]any{"text": "hi"})
		refusedAs(t, err, tc.status, tc.message)
		if tc.status == http.StatusServiceUnavailable && asAPIError(t, err).RetryAfter == 0 {
			t.Fatalf("%v: a 503 without Retry-After", tc.err)
		}
	}
}
