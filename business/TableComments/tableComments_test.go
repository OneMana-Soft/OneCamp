package business

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	threadBusiness "github.com/akashc777/OneCamp/business/Thread"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	threadModels "github.com/akashc777/OneCamp/models/postgres/Thread"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

var (
	maya  = "11111111-1111-4111-8111-111111111111"
	bo    = "22222222-2222-4222-8222-222222222222"
	sam   = "33333333-3333-4333-8333-333333333333" // can't open the table
	gone  = "44444444-4444-4444-8444-444444444444" // deactivated
	row   = "7a7a7a7a-7777-4777-8777-77777777777a"
	table = uuid.MustParse("88888888-8888-4888-8888-888888888888")
	c1    = "c1c1c1c1-0000-4000-8000-000000000001"
	c2    = "c2c2c2c2-0000-4000-8000-000000000002"
)

// A row's threads are registered once, take a row or a cell, and name no row
// by an id the server wouldn't write.
func TestARowTakesThreadsOnItselfAndItsCells(t *testing.T) {
	kinds := rowThreads{}.Anchors()
	if len(kinds) != 2 || kinds[0] != threadBusiness.AnchorNone || kinds[1] != threadBusiness.AnchorCell {
		t.Errorf("anchors %v", kinds)
	}
	for _, id := range []string{strings.ToUpper(row), "{" + row + "}", "row-1", ""} {
		if _, err := (rowThreads{}).Access(context.Background(), accessBusiness.Subject{UserUUID: maya}, accessBusiness.Resource{Kind: Kind, UUID: id}); !errors.Is(err, threadBusiness.ErrNotFound) {
			t.Errorf("%q: %v", id, err)
		}
	}
	if !canonical(row) {
		t.Error("the server's own spelling is refused")
	}
}

// A row's thread changes reach its table's open tabs as ids alone.
func TestARowsThreadsAreLiveOnItsTable(t *testing.T) {
	oldHomes, oldPublish := liveHomes, livePublish
	t.Cleanup(func() { liveHomes, livePublish = oldHomes, oldPublish })
	liveHomes = func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]model.RowHome, error) {
		return map[uuid.UUID]model.RowHome{ids[0]: {Table: table}}, nil
	}
	var topic string
	var sent map[string]interface{}
	livePublish = func(id string, data interface{}) { topic, sent = id, data.(map[string]interface{}) }
	e := resourceEventBusiness.Event{Kind: threadBusiness.ThreadReplied, ResourceKind: Kind, ResourceUUID: row,
		Data: map[string]string{"thread_id": "t1", "comment_id": "c1", "body": "never"}}
	if !isRowThreadEvent(e) || !isRowThreadEvent(resourceEventBusiness.Event{Kind: threadBusiness.ThreadCommentRemoved, ResourceKind: Kind}) {
		t.Error("a row's thread events aren't taken")
	}
	if isRowThreadEvent(resourceEventBusiness.Event{Kind: threadBusiness.ThreadReplied, ResourceKind: "doc"}) || isRowThreadEvent(resourceEventBusiness.Event{Kind: "table.rows.changed", ResourceKind: Kind}) {
		t.Error("a doc's thread, or a row's other event, is taken")
	}
	publishLive(context.Background(), e)
	if topic != table.String() || sent["action"] != liveAction || sent["row_id"] != row || sent["thread_id"] != "t1" || sent["comment_id"] != "c1" || sent["event"] != "thread.replied" {
		t.Errorf("sent %v to %s", sent, topic)
	}
	if _, ok := sent["body"]; ok {
		t.Error("a comment's words went out live")
	}
}

// notifyWorld is the people and the row a telling reads, and what it told.
type notifyWorld struct {
	levels  map[string]accessBusiness.Level
	body    string
	deleted bool
	told    []told
}

type told struct {
	n  notificationBusiness.Notice
	to []notificationBusiness.NoticeRecipient
}

func (w *notifyWorld) install(t *testing.T) {
	t.Helper()
	old := struct {
		titles   func(context.Context, []uuid.UUID) (map[uuid.UUID]dataTableBusiness.RowTitle, error)
		people   func(context.Context, []string) (map[string]*dgraphStruct.DgraphUser, error)
		account  func(context.Context, uuid.UUID) (*userModels.User, error)
		decide   func(context.Context, accessBusiness.Subject, accessBusiness.Resource) (accessBusiness.Decision, error)
		comment  func(context.Context, uuid.UUID) (*threadModels.Comment, error)
		mentions func(context.Context, []string) ([]*dgraphStruct.DgraphUser, error)
		tell     func(context.Context, notificationBusiness.Notice, []notificationBusiness.NoticeRecipient) []string
	}{notifyTitles, notifyPeople, notifyAccount, notifyDecide, notifyComment, notifyMentions, tell}
	t.Cleanup(func() {
		notifyTitles, notifyPeople, notifyAccount, notifyDecide, notifyComment, notifyMentions, tell =
			old.titles, old.people, old.account, old.decide, old.comment, old.mentions, old.tell
	})
	names := map[string]string{maya: "Maya", bo: "Bo", sam: "Sam", gone: "Gil"}
	notifyTitles = func(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]dataTableBusiness.RowTitle, error) {
		out := map[uuid.UUID]dataTableBusiness.RowTitle{}
		for _, id := range ids {
			out[id] = dataTableBusiness.RowTitle{Table: table, TableName: "Deals", Title: "Acme", Deleted: w.deleted}
		}
		return out, nil
	}
	notifyPeople = func(_ context.Context, ids []string) (map[string]*dgraphStruct.DgraphUser, error) {
		out := map[string]*dgraphStruct.DgraphUser{}
		for _, id := range ids {
			if n, ok := names[id]; ok {
				out[id] = &dgraphStruct.DgraphUser{Uuid: id, UserName: n}
			}
		}
		return out, nil
	}
	notifyAccount = func(_ context.Context, id uuid.UUID) (*userModels.User, error) {
		if id.String() == gone {
			return nil, nil
		}
		return &userModels.User{Id: id}, nil
	}
	notifyDecide = func(_ context.Context, who accessBusiness.Subject, _ accessBusiness.Resource) (accessBusiness.Decision, error) {
		return accessBusiness.Decision{Level: w.levels[who.UserUUID]}, nil
	}
	notifyComment = func(context.Context, uuid.UUID) (*threadModels.Comment, error) {
		return &threadModels.Comment{Body: w.body}, nil
	}
	notifyMentions = func(_ context.Context, uids []string) ([]*dgraphStruct.DgraphUser, error) {
		var out []*dgraphStruct.DgraphUser
		for _, uid := range uids {
			for id := range names {
				if "0x"+id[:8] == uid {
					out = append(out, &dgraphStruct.DgraphUser{Uuid: id})
				}
			}
		}
		return out, nil
	}
	tell = func(_ context.Context, n notificationBusiness.Notice, to []notificationBusiness.NoticeRecipient) []string {
		w.told = append(w.told, told{n, to})
		return nil
	}
}

func mention(id string) string {
	return `<span data-type="mention" data-id="` + id + `@0x` + id[:8] + `">@x</span>`
}

func (w *notifyWorld) of(kind string) []told {
	var out []told
	for _, x := range w.told {
		if x.n.Kind == kind {
			out = append(out, x)
		}
	}
	return out
}

func recipients(x told) []string {
	var out []string
	for _, r := range x.to {
		out = append(out, r.UserUUID+":"+r.Reason)
	}
	sort.Strings(out)
	return out
}

// A new comment tells the people it mentions who may open the row, with the
// row and its table named and the row's place to open; someone mentioned who
// can't open it isn't told, and the comment's writer hears that instead.
func TestAMentionOnARowTellsOnlyWhoMayOpenIt(t *testing.T) {
	w := &notifyWorld{levels: map[string]accessBusiness.Level{maya: accessBusiness.LevelEdit, bo: accessBusiness.LevelEdit}}
	w.install(t)
	w.body = "<p>" + mention(bo) + " and " + mention(sam) + " and " + mention(gone) + " and me " + mention(maya) + ", look</p>"
	notify(context.Background(), resourceEventBusiness.Event{
		Kind: threadBusiness.ThreadCreated, ResourceKind: Kind, ResourceUUID: row, At: time.Now(),
		Actor: resourceEventBusiness.Actor{UserUUID: maya}, Subjects: []string{bo, sam, gone},
		Data: map[string]string{"thread_id": "t1", "comment_id": c1},
	})
	got := w.of(KindMentioned)
	if len(got) != 1 {
		t.Fatalf("mention notices: %+v", w.told)
	}
	n := got[0].n
	if strings.Join(recipients(got[0]), ",") != bo+":mention" {
		t.Errorf("mentioned and told: %v", recipients(got[0]))
	}
	if n.Title != "Maya mentioned you on Acme in Deals" || n.ResourceKind != Kind || n.ResourceUUID != row ||
		n.Path != "/app/tables/"+table.String()+"?row="+row+"&thread=t1" || n.Email != notificationBusiness.EventTableMention ||
		!strings.Contains(n.Body, "look") || n.DedupKey != KindMentioned+"|"+c1 {
		t.Errorf("the mention reads %+v", n)
	}
	if n.Push == nil || n.Push.Type != firebaseInit.FIREBASE_PUSH_DATA_TYPE_TABLE_ROW || n.Push.ID != table.String() || n.Push.Thread != row {
		t.Errorf("its push opens %+v", n.Push)
	}
	if n.Data["table_id"] != table.String() || n.Data["thread_id"] != "t1" || n.Data["comment_id"] != c1 {
		t.Errorf("its ids %v", n.Data)
	}
	asks := w.of(KindMentionUnshared)
	var asked []string
	for _, x := range asks {
		if len(x.to) != 1 || x.to[0].UserUUID != maya || !x.to[0].InboxOnly || x.n.Actor != nil || x.n.Push != nil {
			t.Errorf("an ask went %+v to %+v", x.n, x.to)
		}
		asked = append(asked, x.n.Data["person"])
	}
	sort.Strings(asked)
	if strings.Join(asked, ",") != strings.Join([]string{sam, gone}, ",") && strings.Join(asked, ",") != strings.Join([]string{gone, sam}, ",") {
		t.Errorf("the writer heard about %v, want Sam and Gil", asked)
	}
	for _, x := range w.told {
		for _, r := range x.to {
			if r.UserUUID == sam && x.n.Kind != KindMentionUnshared {
				t.Error("Sam, who can't open the table, was told")
			}
		}
	}
}

// A reply tells the thread's people who may open the row, the people it
// mentions as mentioned, and never its writer.
func TestAReplyOnARowTellsTheThreadsPeople(t *testing.T) {
	w := &notifyWorld{levels: map[string]accessBusiness.Level{maya: accessBusiness.LevelEdit, bo: accessBusiness.LevelEdit, gone: accessBusiness.LevelEdit}}
	w.install(t)
	w.body = "<p>" + mention(bo) + " agreed</p>"
	notify(context.Background(), resourceEventBusiness.Event{
		Kind: threadBusiness.ThreadReplied, ResourceKind: Kind, ResourceUUID: row, At: time.Now(),
		Actor: resourceEventBusiness.Actor{UserUUID: sam}, Subjects: []string{maya, bo, gone, sam},
		Data: map[string]string{"thread_id": "t1", "comment_id": c2},
	})
	replies := w.of(string(threadBusiness.ThreadReplied))
	if len(replies) != 1 || strings.Join(recipients(replies[0]), ",") != maya+":participant" {
		t.Fatalf("the reply told %+v", w.told)
	}
	if replies[0].n.Title != "Sam replied in a thread on Acme in Deals" || replies[0].n.DedupKey != "comment|"+c2 || replies[0].n.Email != notificationBusiness.EventTableComment {
		t.Errorf("the reply reads %+v", replies[0].n)
	}
	if m := w.of(KindMentioned); len(m) != 1 || strings.Join(recipients(m[0]), ",") != bo+":mention" {
		t.Errorf("the mention in the reply told %+v", m)
	}
}

// Resolving tells the thread's people in the app alone; a row in the trash
// tells nobody anything.
func TestAThreadSettledOnARowIsToldInTheApp(t *testing.T) {
	w := &notifyWorld{levels: map[string]accessBusiness.Level{maya: accessBusiness.LevelEdit, bo: accessBusiness.LevelView}}
	w.install(t)
	e := resourceEventBusiness.Event{Kind: threadBusiness.ThreadResolved, ResourceKind: Kind, ResourceUUID: row, At: time.Now(),
		Actor: resourceEventBusiness.Actor{UserUUID: bo}, Subjects: []string{maya, sam}, Data: map[string]string{"thread_id": "t1", "status": "resolved"}}
	notify(context.Background(), e)
	got := w.of(string(threadBusiness.ThreadResolved))
	if len(got) != 1 || len(got[0].to) != 1 || got[0].to[0].UserUUID != maya || !got[0].to[0].InboxOnly || got[0].n.Push != nil {
		t.Fatalf("resolving told %+v", w.told)
	}
	if got[0].n.Title != "Bo resolved a thread on Acme in Deals" {
		t.Errorf("it reads %q", got[0].n.Title)
	}
	w.told, w.deleted = nil, true
	notify(context.Background(), e)
	if len(w.told) != 0 {
		t.Errorf("a row in the trash told %+v", w.told)
	}
	if notifies(resourceEventBusiness.Event{Kind: threadBusiness.ThreadCommentEdited, ResourceKind: Kind}) || notifies(resourceEventBusiness.Event{Kind: threadBusiness.ThreadReplied, ResourceKind: "doc"}) {
		t.Error("an edit, or a doc's reply, is told by tables")
	}
}

// The inbox names a row's notice, and opens it, only for a reader who may open
// the row now.
func TestTheInboxNamesARowOnlyForItsReaders(t *testing.T) {
	w := &notifyWorld{}
	w.install(t)
	oldAll := noticesDecideAll
	t.Cleanup(func() { noticesDecideAll = oldAll })
	other := "99999999-9999-4999-8999-999999999999"
	noticesDecideAll = func(_ context.Context, who accessBusiness.Subject, res []accessBusiness.Resource) (map[accessBusiness.Resource]accessBusiness.Decision, error) {
		out := map[accessBusiness.Resource]accessBusiness.Decision{}
		for _, r := range res {
			switch {
			case who.UserUUID == maya && r.UUID == row:
				out[r] = accessBusiness.Decision{Level: accessBusiness.LevelEdit}
			case r.UUID == other:
				out[r] = accessBusiness.Decision{Deleted: true, Was: accessBusiness.LevelEdit}
			}
		}
		return out, nil
	}
	readable, titles, err := rowNotices(context.Background(), maya, []string{strings.ToUpper(row), other, "x"})
	if err != nil || !readable[row] || readable[other] || titles[row] != "Acme in Deals" || titles[other] != "" {
		t.Errorf("Maya's inbox: %v %v %v", readable, titles, err)
	}
	readable, titles, err = rowNotices(context.Background(), bo, []string{row})
	if err != nil || readable[row] || titles[row] != "" {
		t.Errorf("Bo's inbox: %v %v %v", readable, titles, err)
	}
	readable, _, _ = rowNotices(context.Background(), gone, []string{row})
	if len(readable) != 0 {
		t.Error("a deactivated reader opens something")
	}
}
