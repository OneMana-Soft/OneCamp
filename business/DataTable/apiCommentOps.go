package business

// A row's comments over the public API (tables H2b-2): its threads, and a
// comment that starts one, on the row or on one of its cells, or replies in
// one. They go through the app's own threads service (business/Thread, with
// the table_row adapter of business/TableComments) as the token's member:
// accessBusiness.TokenSubjectOf, so never with the member's admin powers, and
// a token bound to an agent comments as that agent for them (the comment is
// stored as the agent's). A row's own table decides who may read or write its
// comments (rowAccess.go), never the table a call names, so a call's row must
// be one that table holds: otherwise a token held to one table would reach the
// comments of another table's rows.
//
// A comment mentions a member as @[<their user_uuid>] (Airtable's API writes
// @[usrID]): it becomes the chip the app's composer writes, so the member is
// told as anyone mentioned in the app is, and every answer writes each chip
// back as @[uuid], with who they are in comment_mentioned
// (contentcodec.WriteMentions, MentionsAsTokens).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
	v1api "github.com/akashc777/OneCamp/business/PublicAPI"
	tableAccess "github.com/akashc777/OneCamp/business/TableAccess"
	threadBusiness "github.com/akashc777/OneCamp/business/Thread"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

const commentsPath = "/tables/{id}/rows/{row}/comments"

// errRowUnread is a row whose table couldn't be read just now: the server's.
var errRowUnread = storeFailed("failed to read the row's table")

func commentOps() []v1api.Op {
	return []v1api.Op{{
		Name:    "tables.row.comments.list",
		Summary: "A row's comment threads, oldest first, each with its comments: the threads on the row and on its cells, open, resolved or both",
		Method:  http.MethodGet, Path: commentsPath, Scope: apiTokenBusiness.ScopeTablesRead,
		Behaviour: v1api.Behaviour{ReadOnly: true, Idempotent: true},
		Input: json.RawMessage(`{"type":"object","properties":{` + tableArg + `,` + rowArg + `,
			"status":{"type":"string","enum":["open","resolved"],"description":"Only the open or only the resolved threads; both when left out."}},
			"required":["id","row"]}`),
		Reach: tableReach,
		Serve: serveRowComments,
	}, {
		Name:    "tables.row.comment.create",
		Summary: "Comment on a row as the token's member: a new thread on the row or on one of its cells, or a reply in one of the row's threads",
		Method:  http.MethodPost, Path: commentsPath, Scope: apiTokenBusiness.ScopeTablesWrite,
		// Each one sent is one more comment: an Idempotency-Key makes a retry
		// safe. It deletes nothing.
		Behaviour: v1api.Behaviour{},
		Input: json.RawMessage(`{"type":"object","properties":{` + tableArg + `,` + rowArg + `,
			"text":{"type":"string","minLength":1,"maxLength":50000,"description":"The comment, as plain text: a blank line starts a paragraph, and lines that start with - or * make a list. @[a member's user_uuid] mentions them: they are told, as anyone mentioned in the app is."},
			"thread":{"type":"string","maxLength":64,"description":"The id of one of the row's threads, to reply in it; leave it out to start a thread."},
			"field":{"type":"string","maxLength":64,"description":"A field's id, to start the thread on the row's cell in that field; leave it out for the row as a whole."}},
			"required":["id","row","text"]}`),
		Reach: tableReach,
		Serve: serveRowCommentCreate,
	}, {
		Name:    "tables.row.thread.resolve",
		Summary: "Resolve one of a row's comment threads: for whoever started it, or anyone who can edit the row",
		Method:  http.MethodPost, Path: threadPath + "/resolve", Scope: apiTokenBusiness.ScopeTablesWrite,
		Behaviour: v1api.Behaviour{Idempotent: true, Recoverable: true},
		UndoneBy:  []string{"tables.row.thread.reopen"},
		Input:     json.RawMessage(`{"type":"object","properties":{` + tableArg + `,` + rowArg + `,` + threadArg + `},"required":["id","row","thread"]}`),
		Reach:     tableReach,
		Serve:     serveThreadSettle(threadBusiness.StatusResolved),
	}, {
		Name:    "tables.row.thread.reopen",
		Summary: "Reopen one of a row's resolved comment threads: for whoever started it, or anyone who can edit the row",
		Method:  http.MethodPost, Path: threadPath + "/reopen", Scope: apiTokenBusiness.ScopeTablesWrite,
		Behaviour: v1api.Behaviour{Idempotent: true, Recoverable: true},
		UndoneBy:  []string{"tables.row.thread.resolve"},
		Input:     json.RawMessage(`{"type":"object","properties":{` + tableArg + `,` + rowArg + `,` + threadArg + `},"required":["id","row","thread"]}`),
		Reach:     tableReach,
		Serve:     serveThreadSettle(threadBusiness.StatusOpen),
	}, {
		Name:    "tables.row.comment.update",
		Summary: "Edit a comment the token's member wrote in one of a row's threads: its new text replaces the old, marked edited",
		Method:  http.MethodPatch, Path: threadPath + "/comments/{comment}", Scope: apiTokenBusiness.ScopeTablesWrite,
		Behaviour: v1api.Behaviour{Idempotent: true},
		Input: json.RawMessage(`{"type":"object","properties":{` + tableArg + `,` + rowArg + `,` + threadArg + `,` + commentArg + `,
			"text":{"type":"string","minLength":1,"maxLength":50000,"description":"The comment's new text, as plain text: a blank line starts a paragraph, and lines that start with - or * make a list. @[a member's user_uuid] mentions them."}},
			"required":["id","row","thread","comment","text"]}`),
		Reach: tableReach,
		Serve: serveRowCommentUpdate,
	}, {
		Name:    "tables.row.comment.delete",
		Summary: "Delete a comment the token's member wrote in one of a row's threads: it is gone for every reader, and kept in the store",
		Method:  http.MethodDelete, Path: threadPath + "/comments/{comment}", Scope: apiTokenBusiness.ScopeTablesWrite,
		Behaviour: v1api.Behaviour{Destructive: true, Idempotent: true},
		Input:     json.RawMessage(`{"type":"object","properties":{` + tableArg + `,` + rowArg + `,` + threadArg + `,` + commentArg + `},"required":["id","row","thread","comment"]}`),
		Reach:     tableReach,
		Serve:     serveRowCommentDelete,
	}}
}

// threadPath is one of a row's threads.
const threadPath = "/tables/{id}/rows/{row}/threads/{thread}"

var (
	threadArg  = `"thread":{"type":"string","maxLength":64,"description":"The id of one of the row's threads."}`
	commentArg = `"comment":{"type":"string","maxLength":64,"description":"The comment's id."}`
)

// errNoComment is a comment the thread hasn't got, or one deleted.
var errNoComment = v1api.Errorf(http.StatusNotFound, "not_found", "", "there is no such comment in this thread")

// threadChange applies a change to one of the row's threads with its audit
// row as action, for op, in one transaction (applyItem: the write, the row,
// the commit): a change the log can't record isn't made, and the thread's
// readers hear of it once it commits (the threads service's events wait for
// it). What the row says is ids and the thread's state, never a comment's
// words.
func threadChange(ctx context.Context, call v1api.Call, op, action string, apply func(ctx context.Context, who accessBusiness.Subject, row uuid.UUID, thread string) (any, bool, error)) (any, error) {
	actor, err := apiActor(ctx)
	if err != nil {
		return nil, err
	}
	who, table, row, err := commentCall(ctx, call)
	if err != nil {
		return nil, err
	}
	thread, _ := call.Args["thread"].(string)
	comment, _ := call.Args["comment"].(string)
	facts := map[string]any{"table_id": table.String(), "row_id": row.String(), "thread_id": thread}
	id := thread
	if comment != "" {
		facts["comment_id"], id = comment, comment
	}
	s := batchStep{item: batchItem{List: "comments", ID: id}, action: action, apply: func(ctx context.Context) (itemDone, error) {
		answer, changed, err := apply(ctx, who, row, thread)
		if err != nil || !changed {
			return itemDone{answer: answer}, err
		}
		return itemDone{answer: answer, summary: commentSummaries[action], facts: facts}, nil
	}}
	v, err := applyItem(ctx, s, func(ctx context.Context, s batchStep, done itemDone, action string) error {
		return auditItem(ctx, actor, op, "", table, s, done, action)
	})
	if err != nil {
		// The change's own refusals are said already; the store's, or the
		// log's, are the server's (apiErr: a 503 that says when to ask again).
		if errors.As(err, new(*v1api.Error)) {
			return nil, err
		}
		if e := apiErr(err); errors.As(e, new(*v1api.Error)) {
			return nil, e
		}
		return nil, threadErr(ctx, err)
	}
	return v, nil
}

// commentSummaries are the audit rows' words for each change.
var commentSummaries = map[string]string{
	"table.row.thread.resolved": "Resolved a comment thread on a table's row through the API",
	"table.row.thread.reopened": "Reopened a comment thread on a table's row through the API",
	"table.row.comment.edited":  "Edited a comment on a table's row through the API",
	"table.row.comment.deleted": "Deleted a comment on a table's row through the API",
}

// serveThreadSettle resolves or reopens a thread of the row's, as the app's
// threads service does (its starter, or anyone who can edit the row). A thread
// already so is answered as it is, with no row.
func serveThreadSettle(to threadBusiness.Status) func(context.Context, v1api.Call) (any, error) {
	op, action, settle := "tables.row.thread.resolve", "table.row.thread.resolved", threadBusiness.Service.Resolve
	if to == threadBusiness.StatusOpen {
		op, action, settle = "tables.row.thread.reopen", "table.row.thread.reopened", threadBusiness.Service.Reopen
	}
	return func(ctx context.Context, call v1api.Call) (any, error) {
		return threadChange(ctx, call, op, action, func(ctx context.Context, who accessBusiness.Subject, row uuid.UUID, thread string) (any, bool, error) {
			v, err := rowThread(ctx, who, row, thread)
			if err != nil {
				return nil, false, err
			}
			t, err := settle(threadBusiness.Threads, ctx, who, thread)
			if err != nil {
				return nil, false, threadErr(ctx, err)
			}
			return t, v.Status != to, nil
		})
	}
}

func serveRowCommentUpdate(ctx context.Context, call v1api.Call) (any, error) {
	text, _ := call.Args["text"].(string)
	body, err := commentBody(ctx, text)
	if err != nil {
		return nil, err
	}
	return threadChange(ctx, call, "tables.row.comment.update", "table.row.comment.edited", func(ctx context.Context, who accessBusiness.Subject, row uuid.UUID, thread string) (any, bool, error) {
		if _, err := rowThread(ctx, who, row, thread); err != nil {
			return nil, false, err
		}
		comment, _ := call.Args["comment"].(string)
		c, err := threadBusiness.Comments.Edit(ctx, who, thread, comment, body)
		if errors.Is(err, threadBusiness.ErrNotFound) {
			return nil, false, errNoComment
		}
		if err != nil {
			return nil, false, threadErr(ctx, err)
		}
		return apiComments(ctx, c)[0], true, nil
	})
}

// serveRowCommentDelete deletes the member's own comment, and only theirs:
// the app lets a thread's moderators remove anyone's, and a token never acts
// with those powers. The comment is kept in the store, marked deleted.
func serveRowCommentDelete(ctx context.Context, call v1api.Call) (any, error) {
	return threadChange(ctx, call, "tables.row.comment.delete", "table.row.comment.deleted", func(ctx context.Context, who accessBusiness.Subject, row uuid.UUID, thread string) (any, bool, error) {
		v, err := rowThread(ctx, who, row, thread)
		if err != nil {
			return nil, false, err
		}
		comment, _ := call.Args["comment"].(string)
		i := slices.IndexFunc(v.Comments, func(c threadBusiness.Comment) bool { return strings.EqualFold(c.ID, comment) })
		id, perr := uuid.Parse(comment)
		if i < 0 || perr != nil {
			return nil, false, errNoComment
		}
		if !strings.EqualFold(v.Comments[i].By.UUID, who.UserUUID) {
			return nil, false, v1api.Errorf(http.StatusForbidden, "forbidden", "", "a token deletes only its member's own comments")
		}
		if _, err := threadBusiness.Comments.Remove(ctx, who, thread, comment); errors.Is(err, threadBusiness.ErrNotFound) {
			return nil, false, errNoComment
		} else if err != nil {
			return nil, false, threadErr(ctx, err)
		}
		return apiDeleted{ID: id, Deleted: true}, true, nil
	})
}

// rowComments is a row's threads as the API answers them.
type rowComments struct {
	Threads []apiThread `json:"threads"`
}

func serveRowComments(ctx context.Context, call v1api.Call) (any, error) {
	who, _, row, err := commentCall(ctx, call)
	if err != nil {
		return nil, err
	}
	status, _ := call.Args["status"].(string)
	list, err := threadBusiness.Threads.List(ctx, who, rowResource(row), threadBusiness.Filter{Status: threadBusiness.Status(status)})
	if err != nil {
		return nil, threadErr(ctx, err)
	}
	return rowComments{Threads: apiThreads(ctx, list...)}, nil
}

// serveRowCommentCreate answers the comment it made, with its thread's id.
func serveRowCommentCreate(ctx context.Context, call v1api.Call) (any, error) {
	who, table, row, err := commentCall(ctx, call)
	if err != nil {
		return nil, err
	}
	text, _ := call.Args["text"].(string)
	body, err := commentBody(ctx, text)
	if err != nil {
		return nil, err
	}
	field, err := optionalID(call.Args, "field")
	if err != nil {
		return nil, err
	}
	if thread, _ := call.Args["thread"].(string); thread != "" {
		if field != nil {
			return nil, v1api.Errorf(http.StatusBadRequest, "invalid_argument", "", "a reply stays where its thread is: leave field out")
		}
		return replyOnRow(ctx, who, row, thread, body)
	}
	anchor := threadBusiness.Anchor{Kind: threadBusiness.AnchorNone}
	if field != nil {
		if anchor, err = cellAnchor(ctx, who, table, row, *field); err != nil {
			return nil, err
		}
	}
	v, err := threadBusiness.Threads.Create(ctx, who, rowResource(row), anchor, body)
	if err != nil {
		return nil, threadErr(ctx, err)
	}
	if len(v.Comments) == 0 {
		return nil, errors.New("tables: a new thread came back without its first comment")
	}
	return apiComments(ctx, v.Comments[0])[0], nil
}

// commentCall is who a comments call acts for, as the threads service
// decides for them (the token's member, as its agent if it has one), and the
// table and row it names. The row must be one the table holds, deleted ones
// too (the threads service says a row in the trash is there).
func commentCall(ctx context.Context, call v1api.Call) (accessBusiness.Subject, uuid.UUID, uuid.UUID, error) {
	_, table, row, err := rowCall(ctx, call)
	if err != nil {
		return accessBusiness.Subject{}, uuid.Nil, uuid.Nil, err
	}
	homes, err := rowHomes(ctx, []uuid.UUID{row})
	if err != nil {
		return accessBusiness.Subject{}, uuid.Nil, uuid.Nil, apiErr(errRowUnread)
	}
	if home, ok := homes[row]; !ok || home.Table != table {
		return accessBusiness.Subject{}, uuid.Nil, uuid.Nil, apiErr(errRowNotFound)
	}
	// rowCall's apiActor has checked the call acts for a member.
	info, _ := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	return accessBusiness.TokenSubjectOf(&info, helpers.GetApiAgentID(ctx)), table, row, nil
}

func rowResource(row uuid.UUID) accessBusiness.Resource {
	return accessBusiness.Resource{Kind: tableAccess.KindTableRow, UUID: row.String()}
}

// replyOnRow replies in a thread of the row's, and only of the row's: a
// thread anywhere else is no thread here.
func replyOnRow(ctx context.Context, who accessBusiness.Subject, row uuid.UUID, thread string, body threadBusiness.Body) (any, error) {
	if _, err := rowThread(ctx, who, row, thread); err != nil {
		return nil, err
	}
	c, err := threadBusiness.Threads.Reply(ctx, who, thread, body)
	if err != nil {
		return nil, threadErr(ctx, err)
	}
	return apiComments(ctx, c)[0], nil
}

// rowThread is one of the row's threads, with its comments, as who sees it:
// a thread anywhere else is no thread here.
func rowThread(ctx context.Context, who accessBusiness.Subject, row uuid.UUID, thread string) (threadBusiness.View, error) {
	v, err := threadBusiness.Threads.Get(ctx, who, thread)
	if err == nil && (v.ResourceKind != tableAccess.KindTableRow || v.ResourceUUID != row.String()) {
		err = threadBusiness.ErrNotFound
	}
	if errors.Is(err, threadBusiness.ErrNotFound) {
		return threadBusiness.View{}, v1api.Errorf(http.StatusNotFound, "not_found", "", "there is no such thread on this row")
	}
	if err != nil {
		return threadBusiness.View{}, threadErr(ctx, err)
	}
	return v, nil
}

// cellAnchor pins a new thread to the row's cell in field. The field is
// checked only for someone the row's own decision lets open it: for anyone
// else the threads service refuses the comment in its own words, so an answer
// never tells them which fields a table they can't open has.
func cellAnchor(ctx context.Context, who accessBusiness.Subject, table, row, field uuid.UUID) (threadBusiness.Anchor, error) {
	meta, err := json.Marshal(map[string]string{"field": field.String()})
	if err != nil {
		return threadBusiness.Anchor{}, err
	}
	anchor := threadBusiness.Anchor{Kind: threadBusiness.AnchorCell, Meta: meta}
	if d, err := decideRow(ctx, who, row); err != nil || d.Level == accessBusiness.LevelNone {
		return anchor, nil
	}
	fields, err := fieldsOf(ctx, table)
	if err != nil {
		return threadBusiness.Anchor{}, apiErr(errRowUnread)
	}
	if !slices.ContainsFunc(fields, func(f *model.Field) bool { return f != nil && f.Id == field }) {
		return threadBusiness.Anchor{}, apiErr(errFieldNotFound)
	}
	return anchor, nil
}

// threadErr is a refusal of the threads service in the API's one error body,
// in its own words where it has them, as the app's threads controller answers
// it (controllers/Thread write). Anything else is the server's: a 503 that
// says when to ask again.
func threadErr(ctx context.Context, err error) error {
	var invalid *threadBusiness.Invalid
	var refusal *threadBusiness.Refusal
	switch {
	case errors.As(err, &invalid):
		return v1api.Errorf(http.StatusBadRequest, "invalid_argument", "", "%s", invalid.Msg)
	case errors.As(err, &refusal):
		return v1api.Errorf(http.StatusForbidden, "forbidden", "", "%s", refusal.Msg)
	case errors.Is(err, threadBusiness.ErrForbidden):
		return v1api.Errorf(http.StatusForbidden, "forbidden", "", "the token's member may not comment on this row")
	case errors.Is(err, threadBusiness.ErrNotFound):
		return v1api.Errorf(http.StatusNotFound, "not_found", "", "there is no such row")
	case errors.Is(err, threadBusiness.ErrGone):
		return v1api.Errorf(http.StatusGone, "gone", "tables.trash.restore brings it back", "the row is in its table's trash")
	}
	helpers.LogErrorWithContext(ctx, "business/DataTable threadErr: %+v", err)
	e := v1api.Errorf(http.StatusServiceUnavailable, "unavailable", "send it again in a few seconds", "the comments couldn't be read or saved just now")
	e.RetryAfter = retryRows
	return e
}

// mentionable is each live member among ids, by uuid, as their own node (a
// seam for tests).
var mentionable = userDomain.LiveOwnNodesByUUIDs

// commentBody is a comment's text as the HTML the threads service keeps:
// plain text (helpers.PlainTextToHTML), each @[member uuid] in it made that
// member's mention. Someone who isn't a live member of the workspace is
// refused by name, and nothing is posted.
func commentBody(ctx context.Context, text string) (threadBusiness.Body, error) {
	h := helpers.PlainTextToHTML(text)
	if h == "" {
		return threadBusiness.Body{}, v1api.Errorf(http.StatusBadRequest, "invalid_argument", "", "write the comment's text: it is blank")
	}
	ids := contentcodec.MentionTokens(text)
	if len(ids) == 0 {
		return threadBusiness.Body{HTML: h}, nil
	}
	found, err := mentionable(ctx, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/DataTable commentBody: the people mentioned couldn't be read: %+v", err)
		e := v1api.Errorf(http.StatusServiceUnavailable, "unavailable", "send it again in a few seconds", "the people mentioned couldn't be read just now")
		e.RetryAfter = retryRows
		return threadBusiness.Body{}, e
	}
	people := make(map[string]contentcodec.Mention, len(ids))
	for _, id := range ids {
		n := found[id]
		if n == nil {
			return threadBusiness.Body{}, v1api.Errorf(http.StatusBadRequest, "invalid_argument", "mention a member by their user_uuid, as @[user_uuid]", "%s is no member of this workspace", contentcodec.MentionToken(id))
		}
		people[id] = contentcodec.Mention{UUID: id, UID: n.Uid, Label: helpers.FirstNonEmpty(n.UserName, n.UserFullName)}
	}
	doc, err := contentcodec.FromHTML(h, contentcodec.HTMLOptions{})
	if err != nil {
		return threadBusiness.Body{}, err
	}
	return threadBusiness.Body{HTML: contentcodec.ToHTML(contentcodec.WriteMentions(doc, people))}, nil
}

// apiComment is a comment as the API answers it: each mention written back
// as @[uuid], and who those are in comment_mentioned (their name now; the
// name the chip kept for someone who has since left).
type apiComment struct {
	threadBusiness.Comment
	Mentioned map[string]threadBusiness.Person `json:"comment_mentioned,omitempty"`
}

// apiThread is a thread as the API answers it: its comments as apiComments.
type apiThread struct {
	threadBusiness.View
	Comments []apiComment `json:"comments"`
}

// apiThreads is threads as the API answers them, every mention in them named
// in one read.
func apiThreads(ctx context.Context, threads ...threadBusiness.View) []apiThread {
	var all []threadBusiness.Comment
	for _, t := range threads {
		all = append(all, t.Comments...)
	}
	said := apiComments(ctx, all...)
	out := make([]apiThread, len(threads))
	for i, t := range threads {
		out[i] = apiThread{View: t, Comments: said[:len(t.Comments):len(t.Comments)]}
		said = said[len(t.Comments):]
	}
	return out
}

// apiComments is comments as the API answers them, the people they mention
// named in one read. A name that can't be read just now is the one the chip
// kept: a read never fails for a name.
func apiComments(ctx context.Context, comments ...threadBusiness.Comment) []apiComment {
	out := make([]apiComment, len(comments))
	kept := make([][]contentcodec.Mention, len(comments))
	var ids []string
	for i, c := range comments {
		out[i] = apiComment{Comment: c}
		if !strings.Contains(c.Text, `data-type="mention"`) {
			continue
		}
		doc, err := contentcodec.FromHTML(c.Text, contentcodec.HTMLOptions{})
		if err != nil {
			continue
		}
		if kept[i] = contentcodec.MentionsAsTokens(doc); len(kept[i]) > 0 {
			out[i].Text = contentcodec.ToHTML(doc)
			for _, m := range kept[i] {
				ids = append(ids, m.UUID)
			}
		}
	}
	if len(ids) == 0 {
		return out
	}
	now, err := mentionable(ctx, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/DataTable apiComments: the people mentioned couldn't be named just now: %+v", err)
		now = nil
	}
	for i, ms := range kept {
		for _, m := range ms {
			p := threadBusiness.Person{UUID: m.UUID, Name: m.Label}
			if n := now[m.UUID]; n != nil {
				p.Name = helpers.FirstNonEmpty(n.UserName, n.UserFullName, m.Label)
				if n.ProfileKey != nil {
					p.ProfileKey = *n.ProfileKey
				}
			}
			if out[i].Mentioned == nil {
				out[i].Mentioned = map[string]threadBusiness.Person{}
			}
			out[i].Mentioned[m.UUID] = p
		}
	}
	return out
}
