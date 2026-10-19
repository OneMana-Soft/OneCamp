package business

// Telling people about a row's threads, as docs' threads tell (business/Follow
// notify.go), through the one notifier (notificationBusiness.Tell: the inbox,
// a push through the push gate, and an email by each person's settings):
//
//   - a new comment tells the people it mentions ("Maya mentioned you on Acme
//     in Deals");
//   - a reply tells the thread's people too: its author, its repliers and
//     whoever it mentioned before ("replied in a thread on…");
//   - resolving and reopening tell the thread's people, in the app only;
//   - nobody is told what they can't open now: someone mentioned who can't open
//     the row isn't told, and its writer hears that instead ("Bo can't open
//     Deals"), as a doc's mention says so.
//
// The actor is never told of their own comment, and an agent's comment is said
// as the agent's. A notice names the row and its table; a push and an email
// carry the comment's first words, as a doc thread's reply does.

import (
	"context"
	"strings"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	threadBusiness "github.com/akashc777/OneCamp/business/Thread"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	threadModels "github.com/akashc777/OneCamp/models/postgres/Thread"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// The notices a row's threads send. Never renamed once shipped: the inbox
// words each (the web app's lib/activity/notice.ts).
const (
	KindMentioned       = "table_row.mentioned"
	KindMentionUnshared = "table_row.mention.unshared"
)

func init() {
	resourceEventBusiness.Sinks.Register(resourceEventBusiness.Sink{Name: "table_row.notify", Wants: notifies, Run: notify})
}

// Seams for tests: who people are, what may they open, where a comment is
// read, and where notices go.
var (
	notifyTitles  = dataTableBusiness.RowTitles
	notifyPeople  = userDomain.GetUserDisplayMapByUUIDs
	notifyAccount = userDomain.GetActiveUserWithAdminFlagByUserUUID
	notifyDecide  = func(ctx context.Context, who accessBusiness.Subject, res accessBusiness.Resource) (accessBusiness.Decision, error) {
		return accessBusiness.Rules.Resolve(ctx, who, res)
	}
	notifyComment  = threadModels.GetComment
	notifyMentions = userBusiness.GetDgraphUserInfoByUUIDs
	tell           = notificationBusiness.Tell
)

// notifies is the thread events a row's people hear of. Pure.
func notifies(e resourceEventBusiness.Event) bool {
	if e.ResourceKind != Kind {
		return false
	}
	switch e.Kind {
	case threadBusiness.ThreadCreated, threadBusiness.ThreadReplied, threadBusiness.ThreadResolved, threadBusiness.ThreadReopened:
		return true
	}
	return false
}

func notify(ctx context.Context, e resourceEventBusiness.Event) {
	row, err := uuid.Parse(e.ResourceUUID)
	if err != nil {
		return
	}
	titles, err := notifyTitles(ctx, []uuid.UUID{row})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/TableComments notify %s: row %s couldn't be named, so nobody was told: %+v", e.Kind, row, err)
		return
	}
	rt, ok := titles[row]
	if !ok || rt.Deleted {
		return
	}
	c := castOf(ctx, e)
	switch e.Kind {
	case threadBusiness.ThreadCreated, threadBusiness.ThreadReplied:
		tellComment(ctx, e, rt, c)
	case threadBusiness.ThreadResolved, threadBusiness.ThreadReopened:
		tellStatus(ctx, e, rt, c)
	}
}

// cast is the people an event names, by uuid.
type cast struct {
	byID         map[string]*dgraphStruct.DgraphUser
	actor, agent *dgraphStruct.DgraphUser
}

func castOf(ctx context.Context, e resourceEventBusiness.Event) cast {
	ids := append([]string{e.Actor.UserUUID, e.Actor.AgentUUID}, e.Subjects...)
	var want []string
	for _, id := range ids {
		if id != "" {
			want = append(want, id)
		}
	}
	found, err := notifyPeople(ctx, want)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/TableComments notify %s: the people in it couldn't be read: %+v", e.Kind, err)
		found = map[string]*dgraphStruct.DgraphUser{}
	}
	return cast{byID: found, actor: found[e.Actor.UserUUID], agent: found[e.Actor.AgentUUID]}
}

// who is the actor as a sentence opens: the agent's name for an agent's
// comment. Pure.
func (c cast) who() string {
	switch {
	case c.agent != nil:
		return c.agent.DisplayName()
	case c.actor != nil:
		return c.actor.DisplayName()
	}
	return "Someone"
}

func (c cast) name(id string) string {
	if u := c.byID[id]; u != nil {
		return u.DisplayName()
	}
	return "Someone"
}

// place is a row as a notice says where something happened: "Acme in Deals".
// Pure.
func place(rt dataTableBusiness.RowTitle) string {
	return rt.Title + " in " + rt.TableName
}

// path is where a notice opens: the row in its table, at the thread.
func path(rt dataTableBusiness.RowTitle, row, thread string) string {
	p := "/app/tables/" + rt.Table.String() + "?row=" + row
	if thread != "" {
		p += "&thread=" + thread
	}
	return p
}

// base is the notice every telling of a row's threads starts from.
func base(e resourceEventBusiness.Event, rt dataTableBusiness.RowTitle, c cast, kind string) notificationBusiness.Notice {
	thread := e.Data["thread_id"]
	return notificationBusiness.Notice{
		Kind:          kind,
		ResourceKind:  Kind,
		ResourceUUID:  e.ResourceUUID,
		ResourceTitle: place(rt),
		Actor:         c.actor,
		Agent:         c.agent,
		Data:          map[string]string{"thread_id": thread, "comment_id": e.Data["comment_id"], "table_id": rt.Table.String()},
		Path:          path(rt, e.ResourceUUID, thread),
		Push:          &notificationBusiness.Push{Type: firebaseInit.FIREBASE_PUSH_DATA_TYPE_TABLE_ROW, ID: rt.Table.String(), Thread: e.ResourceUUID},
	}
}

// readers is, of ids, who may open the row now: each decided as themselves,
// an admin as an admin (who opens private tables), and nobody deactivated.
func readers(ctx context.Context, row string, ids []string) map[string]bool {
	out := map[string]bool{}
	for _, id := range ids {
		parsed, err := uuid.Parse(id)
		if err != nil || out[id] {
			continue
		}
		account, err := notifyAccount(ctx, parsed)
		if err != nil || account == nil {
			continue
		}
		d, err := notifyDecide(ctx, subjectOf(id, account), accessBusiness.Resource{Kind: Kind, UUID: row})
		if err == nil && !d.Deleted && d.Level >= accessBusiness.LevelView {
			out[id] = true
		}
	}
	return out
}

// subjectOf is a person told of something as the Access Resolver decides for
// them: as themselves, never through a token or an agent. Pure.
func subjectOf(id string, account *userModels.User) accessBusiness.Subject {
	return accessBusiness.Subject{UserUUID: id, Admin: account.IsAdmin}
}

// tellComment tells of a new thread or a reply.
func tellComment(ctx context.Context, e resourceEventBusiness.Event, rt dataTableBusiness.RowTitle, c cast) {
	commentID := e.Data["comment_id"]
	text, mentioned := commentTold(ctx, commentID)
	actor := e.Actor.UserUUID
	everyone := append(append([]string{}, e.Subjects...), mentioned...)
	can := readers(ctx, e.ResourceUUID, everyone)

	var mentions, others []notificationBusiness.NoticeRecipient
	var outside []string
	isMention := map[string]bool{}
	for _, id := range mentioned {
		if id == "" || id == actor || isMention[id] {
			continue
		}
		isMention[id] = true
		if can[id] {
			mentions = append(mentions, notificationBusiness.NoticeRecipient{UserUUID: id, Reason: "mention"})
		} else {
			outside = append(outside, id)
		}
	}
	if e.Kind == threadBusiness.ThreadReplied {
		seen := map[string]bool{}
		for _, id := range e.Subjects {
			if id == "" || id == actor || isMention[id] || seen[id] || !can[id] {
				continue
			}
			seen[id] = true
			others = append(others, notificationBusiness.NoticeRecipient{UserUUID: id, Reason: "participant"})
		}
	}
	words := helpers.TruncateRunes(strings.TrimSpace(helpers.HTMLToPlainText(text)), 300)

	if len(mentions) > 0 {
		n := base(e, rt, c, KindMentioned)
		n.DedupKey = KindMentioned + "|" + commentID
		n.Email = notificationBusiness.EventTableMention
		n.Title = c.who() + " mentioned you on " + place(rt)
		n.Body = words
		tell(ctx, n, mentions)
	}
	if len(others) > 0 {
		n := base(e, rt, c, string(threadBusiness.ThreadReplied))
		// The key a doc thread's reply has: one inbox item per comment.
		n.DedupKey = "comment|" + commentID
		n.Email = notificationBusiness.EventTableComment
		n.Title = c.who() + " replied in a thread on " + place(rt)
		n.Body = words
		tell(ctx, n, others)
	}
	if len(outside) > 0 && actor != "" {
		to := []notificationBusiness.NoticeRecipient{{UserUUID: actor, Reason: "author", InboxOnly: true}}
		for _, id := range outside {
			ask := base(e, rt, c, KindMentionUnshared)
			// The writer is told about their own mention: nobody is "the actor".
			ask.Actor, ask.Agent, ask.Push = nil, nil, nil
			ask.Data["person"], ask.Data["person_name"] = id, c.name(id)
			ask.DedupKey = KindMentionUnshared + "|" + commentID + "|" + id
			ask.Title = c.name(id) + " can't open " + rt.TableName
			ask.Body = "You mentioned them, but the table isn't shared with them, so they weren't told."
			tell(ctx, ask, to)
		}
	}
}

// tellStatus tells a thread's people, in the app, that it was resolved or
// reopened.
func tellStatus(ctx context.Context, e resourceEventBusiness.Event, rt dataTableBusiness.RowTitle, c cast) {
	can := readers(ctx, e.ResourceUUID, e.Subjects)
	var to []notificationBusiness.NoticeRecipient
	for _, id := range e.Subjects {
		if can[id] && id != e.Actor.UserUUID {
			to = append(to, notificationBusiness.NoticeRecipient{UserUUID: id, Reason: "participant", InboxOnly: true})
		}
	}
	if len(to) == 0 {
		return
	}
	verb := "resolved"
	if e.Kind == threadBusiness.ThreadReopened {
		verb = "reopened"
	}
	n := base(e, rt, c, string(e.Kind))
	n.Push = nil
	n.Data["status"] = e.Data["status"]
	n.DedupKey = string(e.Kind) + "|" + e.Data["thread_id"] + "|" + e.At.UTC().Format(time.RFC3339Nano)
	n.Title = c.who() + " " + verb + " a thread on " + place(rt)
	tell(ctx, n, to)
}

// commentTold is a stored comment's words and whom it mentions (by uuid), read
// as stored; nothing when it can't be read, so it is told without its words
// and its mentions aren't.
func commentTold(ctx context.Context, commentID string) (string, []string) {
	id, err := uuid.Parse(commentID)
	if err != nil {
		return "", nil
	}
	row, err := notifyComment(ctx, id)
	if err != nil || row == nil || row.DeletedAt != nil {
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/TableComments: comment %s couldn't be read, so it is told without its words: %+v", commentID, err)
		}
		return "", nil
	}
	uids, err := helpers.GetMentions(row.Body)
	if err != nil || len(uids) == 0 {
		return row.Body, nil
	}
	found, err := notifyMentions(ctx, uids)
	if err != nil {
		return row.Body, nil
	}
	var out []string
	for _, u := range found {
		if u != nil && u.Uuid != "" {
			out = append(out, u.Uuid)
		}
	}
	return row.Body, out
}
