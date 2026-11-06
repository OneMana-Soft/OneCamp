package business

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	tableAccess "github.com/akashc777/OneCamp/business/TableAccess"
	threadBusiness "github.com/akashc777/OneCamp/business/Thread"
)

// settleThreads is the threads service as a thread's resolve and reopen call
// it: the thread got, with the status it is set to.
type settleThreads struct{ *fakeThreads }

func (f settleThreads) settle(call string, who accessBusiness.Subject, id string, to threadBusiness.Status) (threadBusiness.Thread, error) {
	f.threads = append(f.threads, id)
	if err := f.asked(call, who); err != nil {
		return threadBusiness.Thread{}, err
	}
	t := f.got.Thread
	t.Status = to
	return t, nil
}

func (f settleThreads) Resolve(_ context.Context, who accessBusiness.Subject, id string) (threadBusiness.Thread, error) {
	return f.settle("Resolve", who, id, threadBusiness.StatusResolved)
}

func (f settleThreads) Reopen(_ context.Context, who accessBusiness.Subject, id string) (threadBusiness.Thread, error) {
	return f.settle("Reopen", who, id, threadBusiness.StatusOpen)
}

// fakeComments are a stored comment's changes, as the API's edit and delete
// call them: each call (call thread comment), as whom, with which words.
type fakeComments struct {
	threadBusiness.CommentChanges // a change the ops shouldn't make panics
	calls                         []string
	who                           []accessBusiness.Subject
	bodies                        []threadBusiness.Body
	errs                          map[string]error
}

func (f *fakeComments) Edit(_ context.Context, who accessBusiness.Subject, thread, comment string, body threadBusiness.Body) (threadBusiness.Comment, error) {
	f.calls, f.who, f.bodies = append(f.calls, "Edit "+thread+" "+comment), append(f.who, who), append(f.bodies, body)
	if err := f.errs["Edit"]; err != nil {
		return threadBusiness.Comment{}, err
	}
	return threadBusiness.Comment{ID: comment, ThreadID: thread, Text: body.HTML}, nil
}

func (f *fakeComments) Remove(_ context.Context, who accessBusiness.Subject, thread, comment string) (threadBusiness.Removed, error) {
	f.calls, f.who = append(f.calls, "Remove "+thread+" "+comment), append(f.who, who)
	return threadBusiness.Removed{}, f.errs["Remove"]
}

// loggedRow is an audit row as the API wrote it.
type loggedRow struct {
	action, category, summary string
	meta                      map[string]any
}

// changeWorld is a row's comments ops with a thread on the row, "t-1": the
// member's own comment and someone else's, each change made in its own
// transaction (counted) with its audit row (kept, or refused).
type changeWorld struct {
	*commentWorld
	comments      *fakeComments
	mine, theirs  string
	txs           int
	rows          []loggedRow
	refuseLogging error
}

// secretWords are a comment's words, which no audit row may quote.
const secretWords = "the board's salary bands"

func newChangeWorld(t *testing.T) *changeWorld {
	t.Helper()
	w := &changeWorld{commentWorld: newCommentWorld(t), comments: &fakeComments{}, mine: uuid.NewString(), theirs: uuid.NewString()}
	w.threads.got = threadBusiness.View{
		Thread: threadBusiness.Thread{ID: "t-1", ResourceKind: tableAccess.KindTableRow, ResourceUUID: w.row.String(), Status: threadBusiness.StatusOpen},
		Comments: []threadBusiness.Comment{
			{ID: w.mine, ThreadID: "t-1", Text: "<p>" + secretWords + "</p>", By: threadBusiness.Person{UUID: w.member.String()}},
			{ID: w.theirs, ThreadID: "t-1", Text: "<p>" + secretWords + "?</p>", By: threadBusiness.Person{UUID: uuid.NewString()}},
		},
	}
	oldComments, oldTx, oldRecord := threadBusiness.Comments, itemTx, recordItem
	t.Cleanup(func() { threadBusiness.Comments, itemTx, recordItem = oldComments, oldTx, oldRecord })
	threadBusiness.Threads = settleThreads{w.threads} // newCommentWorld puts the service back
	threadBusiness.Comments = w.comments
	itemTx = func(ctx context.Context, fn func(ctx context.Context) error) error {
		w.txs++
		return fn(ctx)
	}
	recordItem = func(_ context.Context, _ *uuid.UUID, _, _, action, category, summary string, meta map[string]any) error {
		if w.refuseLogging != nil {
			return w.refuseLogging
		}
		w.rows = append(w.rows, loggedRow{action: action, category: category, summary: summary, meta: meta})
		return nil
	}
	return w
}

func (w *changeWorld) actions() []string {
	out := []string{}
	for _, r := range w.rows {
		out = append(out, r.action)
	}
	return out
}

// noWordsLogged fails when an audit row quotes a comment's words.
func (w *changeWorld) noWordsLogged(t *testing.T) {
	t.Helper()
	logged := []any{}
	for _, r := range w.rows {
		logged = append(logged, r.summary, r.meta)
	}
	if s := jsonOf(t, logged); strings.Contains(s, "salary") || strings.Contains(s, "New words") || !strings.Contains(s, w.table.String()) {
		t.Fatalf("an audit row quotes a comment's words, or names no table: %s", s)
	}
}

// A row's thread is resolved and reopened as the app's threads service does it,
// asked as the token's member without their admin powers, in a transaction
// with an audit row each: ids and the thread, never its words. A thread
// already so is answered as it is, with no row.
func TestAThreadIsResolvedAndReopenedWithARowEach(t *testing.T) {
	w := newChangeWorld(t)
	got, err := w.serve(t, "tables.row.thread.resolve", map[string]any{"thread": "t-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(w.threads.calls, []string{"Get", "Resolve"}) || !slices.Equal(w.threads.threads, []string{"t-1", "t-1"}) {
		t.Fatalf("threads asked %v of %v, want Get then Resolve of t-1", w.threads.calls, w.threads.threads)
	}
	if who := w.threads.who[1]; who.Admin || who.UserUUID != w.member.String() || who.AgentUUID != w.agent {
		t.Fatalf("resolved as %+v, want the member %s without admin powers, as agent %s", who, w.member, w.agent)
	}
	if s := jsonOf(t, got); !strings.Contains(s, `"id":"t-1"`) || !strings.Contains(s, `"status":"resolved"`) {
		t.Fatalf("answered %s, want the thread resolved", s)
	}
	if w.txs != 1 || !slices.Equal(w.actions(), []string{"table.row.thread.resolved"}) {
		t.Fatalf("%d transactions wrote rows %v, want one resolved row in one", w.txs, w.actions())
	}
	row := w.rows[0]
	for k, want := range map[string]any{"op": "tables.row.thread.resolve", "table_id": w.table.String(), "row_id": w.row.String(),
		"thread_id": "t-1", "id": "t-1", "list": "comments", "agent_id": w.agent} {
		if row.meta[k] != want {
			t.Errorf("the row's %s is %v, want %v (%v)", k, row.meta[k], want, row.meta)
		}
	}
	if row.category != "tables" || row.summary == "" {
		t.Errorf("the row is %q in %q, want a summary in tables", row.summary, row.category)
	}

	// Resolved already: answered as it is, and no row.
	w.threads.got.Status = threadBusiness.StatusResolved
	if _, err := w.serve(t, "tables.row.thread.resolve", map[string]any{"thread": "t-1"}); err != nil {
		t.Fatal(err)
	}
	if len(w.rows) != 1 {
		t.Fatalf("a thread resolved already wrote rows %v", w.actions())
	}
	// Reopened: a row; reopened again: none.
	if _, err := w.serve(t, "tables.row.thread.reopen", map[string]any{"thread": "t-1"}); err != nil {
		t.Fatal(err)
	}
	w.threads.got.Status = threadBusiness.StatusOpen
	if _, err := w.serve(t, "tables.row.thread.reopen", map[string]any{"thread": "t-1"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(w.actions(), []string{"table.row.thread.resolved", "table.row.thread.reopened"}) {
		t.Fatalf("rows %v, want resolved then reopened", w.actions())
	}
	if n := len(w.threads.calls); w.threads.calls[n-1] != "Reopen" {
		t.Fatalf("threads asked %v, want a Reopen last", w.threads.calls)
	}
	w.noWordsLogged(t)
}

// A thread anywhere but this row is no thread here, for every change: the
// threads service is asked nothing more, and nothing is logged.
func TestAThreadOnAnotherRowIsNoThreadForAnyChange(t *testing.T) {
	w := newChangeWorld(t)
	w.threads.got.ResourceUUID = uuid.NewString()
	changes := map[string]map[string]any{
		"tables.row.thread.resolve": {"thread": "t-1"},
		"tables.row.thread.reopen":  {"thread": "t-1"},
		"tables.row.comment.update": {"thread": "t-1", "comment": w.mine, "text": "New words"},
		"tables.row.comment.delete": {"thread": "t-1", "comment": w.mine},
	}
	for name, args := range changes {
		_, err := w.serve(t, name, args)
		refusedAs(t, err, http.StatusNotFound, "no such thread on this row")
	}
	w.threads.got.ResourceUUID, w.threads.errs = w.row.String(), map[string]error{"Get": threadBusiness.ErrNotFound}
	for name, args := range changes {
		_, err := w.serve(t, name, args)
		refusedAs(t, err, http.StatusNotFound, "no such thread on this row")
	}
	for _, c := range w.threads.calls {
		if c != "Get" {
			t.Fatalf("threads asked %v; a thread not on the row is changed by nobody", w.threads.calls)
		}
	}
	if len(w.comments.calls) != 0 || len(w.rows) != 0 {
		t.Fatalf("comments changed %v, rows %v; want nothing", w.comments.calls, w.actions())
	}
}

// A comment's writer gives it new words through the threads service (which
// lets only its writer edit), the text made escaped HTML, with a row that names
// the comment and never quotes it. A blank text is nothing to save; a comment
// the thread hasn't got is 404; the service's refusals answer in the API's
// error body, with no row.
func TestACommentIsEditedWithARowThatNeverQuotesIt(t *testing.T) {
	w := newChangeWorld(t)
	got, err := w.serve(t, "tables.row.comment.update", map[string]any{"thread": "t-1", "comment": w.mine, "text": "New words <b>"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(w.comments.calls, []string{"Edit t-1 " + w.mine}) || w.comments.bodies[0].HTML != "<p>New words &lt;b&gt;</p>" {
		t.Fatalf("comments asked %v with %+v, want one Edit of %s as escaped HTML", w.comments.calls, w.comments.bodies, w.mine)
	}
	if who := w.comments.who[0]; who.Admin || who.UserUUID != w.member.String() || who.AgentUUID != w.agent {
		t.Fatalf("edited as %+v, want the member without admin powers, as the agent", who)
	}
	if s := jsonOf(t, got); !strings.Contains(s, `"comment_uuid":"`+w.mine+`"`) {
		t.Fatalf("answered %s, want the edited comment", s)
	}
	if !slices.Equal(w.actions(), []string{"table.row.comment.edited"}) || w.rows[0].meta["comment_id"] != w.mine || w.rows[0].meta["id"] != w.mine ||
		w.rows[0].meta["thread_id"] != "t-1" || w.rows[0].meta["row_id"] != w.row.String() {
		t.Fatalf("rows %v (%v), want one edited row naming comment %s in t-1 on the row", w.actions(), w.rows, w.mine)
	}
	w.noWordsLogged(t)

	before := w.txs
	_, err = w.serve(t, "tables.row.comment.update", map[string]any{"thread": "t-1", "comment": w.mine, "text": " \n "})
	refusedAs(t, err, http.StatusBadRequest, "blank")
	if w.txs != before || len(w.comments.calls) != 1 {
		t.Fatalf("a blank edit reached the store (%d transactions, calls %v)", w.txs-before, w.comments.calls)
	}

	for err, want := range map[error]struct {
		status int
		words  string
	}{
		threadBusiness.ErrNotFound:                                      {http.StatusNotFound, "no such comment"},
		&threadBusiness.Refusal{Msg: "only its writer edits a comment"}: {http.StatusForbidden, "only its writer"},
		errors.New("connection reset"):                                  {http.StatusServiceUnavailable, "couldn't be read or saved"},
	} {
		w.comments.errs = map[string]error{"Edit": err}
		_, got := w.serve(t, "tables.row.comment.update", map[string]any{"thread": "t-1", "comment": w.theirs, "text": "New words"})
		refusedAs(t, got, want.status, want.words)
	}
	if len(w.rows) != 1 {
		t.Fatalf("refused edits wrote rows %v", w.actions())
	}
}

// A token deletes only its member's own comment, never with a moderator's
// powers: someone else's is refused before the store is asked, and a comment
// the thread hasn't got is 404. Deleted, the comment answers its id, with a
// row that never quotes it.
func TestATokenDeletesOnlyItsMembersOwnComment(t *testing.T) {
	w := newChangeWorld(t)
	_, err := w.serve(t, "tables.row.comment.delete", map[string]any{"thread": "t-1", "comment": w.theirs})
	refusedAs(t, err, http.StatusForbidden, "own comments")
	_, err = w.serve(t, "tables.row.comment.delete", map[string]any{"thread": "t-1", "comment": uuid.NewString()})
	refusedAs(t, err, http.StatusNotFound, "no such comment")
	if len(w.comments.calls) != 0 || len(w.rows) != 0 {
		t.Fatalf("refused deletes asked %v and wrote %v", w.comments.calls, w.actions())
	}

	got, err := w.serve(t, "tables.row.comment.delete", map[string]any{"thread": "t-1", "comment": strings.ToUpper(w.mine)})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(w.comments.calls, []string{"Remove t-1 " + strings.ToUpper(w.mine)}) || w.comments.who[0].Admin {
		t.Fatalf("comments asked %v as %+v, want one Remove without admin powers", w.comments.calls, w.comments.who)
	}
	if s := jsonOf(t, got); s != `{"id":"`+w.mine+`","deleted":true}` {
		t.Fatalf("answered %s, want the comment's id, deleted", s)
	}
	if !slices.Equal(w.actions(), []string{"table.row.comment.deleted"}) || w.rows[0].meta["thread_id"] != "t-1" {
		t.Fatalf("rows %v (%v), want one deleted row", w.actions(), w.rows)
	}
	w.noWordsLogged(t)

	w.comments.errs = map[string]error{"Remove": threadBusiness.ErrNotFound}
	_, err = w.serve(t, "tables.row.comment.delete", map[string]any{"thread": "t-1", "comment": w.mine})
	refusedAs(t, err, http.StatusNotFound, "no such comment")
	if len(w.rows) != 1 {
		t.Fatalf("a delete the store didn't find wrote rows %v", w.actions())
	}
}

// A change whose audit row the log refuses answers as the server's failure,
// with a time to ask again.
func TestAThreadChangeTheLogRefusesIsTheServers(t *testing.T) {
	w := newChangeWorld(t)
	w.refuseLogging = errors.New("the audit log refused the row")
	_, err := w.serve(t, "tables.row.thread.resolve", map[string]any{"thread": "t-1"})
	if e := asAPIError(t, err); e.Status != http.StatusServiceUnavailable || e.Code != "unavailable" || e.RetryAfter <= 0 {
		t.Fatalf("a refused row answered %d %s (retry after %v), want 503 unavailable with a retry", e.Status, e.Code, e.RetryAfter)
	}
}

// A table's own life over the API: made and listed from the trash by anyone
// but an app (an app has no tables of its own), changed, deleted and brought
// back by the table named. An update that names nothing to change is refused
// before the store is asked.
func TestATablesLifeIsItsOwnOpsAndAnUpdateNamesAChange(t *testing.T) {
	want := map[string]struct {
		method, path, scope string
		placed              bool
	}{
		"tables.table.create":  {http.MethodPost, "/tables", "tables:write", false},
		"tables.table.update":  {http.MethodPatch, "/tables/{id}", "tables:write", true},
		"tables.table.delete":  {http.MethodDelete, "/tables/{id}", "tables:write", true},
		"tables.table.restore": {http.MethodPost, "/tables/{id}/restore", "tables:write", true},
		"tables.deleted.list":  {http.MethodGet, "/tables/trash", "tables:read", false},
	}
	for _, op := range APIOps() {
		w, ok := want[op.Name]
		if !ok {
			continue
		}
		delete(want, op.Name)
		if op.Method != w.method || op.Path != w.path || op.Scope != w.scope {
			t.Errorf("%s is %s %s (%s), want %s %s (%s)", op.Name, op.Method, op.Path, op.Scope, w.method, w.path, w.scope)
		}
		if placed := op.Reach != nil; placed != w.placed || (!placed && op.AppsRefused == "") {
			t.Errorf("%s: reached through a table %v, apps refused %q; want reached %v, or apps refused", op.Name, placed, op.AppsRefused, w.placed)
		}
	}
	if len(want) != 0 {
		t.Fatalf("no ops %v", want)
	}

	w := newCommentWorld(t)
	_, err := w.serve(t, "tables.table.update", map[string]any{})
	refusedAs(t, err, http.StatusBadRequest, "name what to change")
}
