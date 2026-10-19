package business

// Stored comments: a thread's comments kept in Postgres (thread_comments,
// migration 348), for a kind of resource that keeps no comments of its own (a
// table's rows today; a call's timeline or an event next). A doc keeps its
// comments in the graph and never uses this.
//
// A kind opts in through its Resource: it embeds StoredComments, which answers
// Comments, Legacy and Claim, and its Post does the resource's own checks and
// then Write. Edits, removals and reactions reach a stored comment through
// Comments (commentChanges.go), which decides who may make them.
//
// What is stored is safe to show: a body is the comment editor's HTML read
// through the content codec (only what the editor's schema renders survives:
// no scripts, styles, frames, foreign classes or unsafe links), as a calendar
// event's notes are. The people it mentions are read from what is stored, so
// a mention the codec dropped tells nobody.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	models "github.com/akashc777/OneCamp/models/postgres/Thread"
	"github.com/google/uuid"
)

// StoredComments keeps one kind's comments in Postgres.
type StoredComments struct {
	// Kind is the resource kind, as threads name it ("table_row").
	Kind string
}

// Seams for tests: where stored comments, their reactions and the people
// named in them are read and written.
var (
	storedWrite      = models.InsertComment
	storedRead       = models.CommentsOf
	storedReactions  = models.ReactionsOf
	storedPeople     = userDomain.GetUserDisplayMapByUUIDs
	storedMentioned  = userBusiness.GetDgraphUserInfoByUUIDs
	storedNewID      = uuid.NewString
	storedClock      = func() time.Time { return time.Now().UTC() }
	errStoredNoOwner = errors.New("threads: a stored comment needs someone to have written it")
)

// formerMember is how a comment by someone no longer in the workspace names
// them: the comment stays, its writer's name with it is gone.
const formerMember = "A former member"

// Comments is res's live comments, oldest first, each with its thread, its
// writer and its reactions.
func (s StoredComments) Comments(ctx context.Context, _ accessBusiness.Subject, res accessBusiness.Resource) ([]Comment, error) {
	rows, err := storedRead(ctx, s.Kind, res.UUID)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0, len(rows))
	people := map[string]bool{}
	for _, r := range rows {
		ids = append(ids, r.ID)
		people[r.AuthorUUID.String()] = true
	}
	reactions, err := storedReactions(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, rs := range reactions {
		for _, r := range rs {
			people[r.UserUUID.String()] = true
		}
	}
	names, err := storedPeople(ctx, keys(people))
	if err != nil {
		// Who wrote them couldn't be read: the comments are shown without
		// names rather than not at all.
		helpers.LogErrorWithContext(ctx, "business/Thread StoredComments: reading the people of %s %s failed: %+v", s.Kind, res.UUID, err)
		names = nil
	}
	out := make([]Comment, 0, len(rows))
	for _, r := range rows {
		out = append(out, storedComment(r, reactions[r.ID], names))
	}
	return out, nil
}

// Legacy: stored comments were never written outside a thread.
func (StoredComments) Legacy(context.Context, string) (Thread, Comment, bool, error) {
	return Thread{}, Comment{}, false, nil
}

// Claim: there is no legacy comment to claim.
func (StoredComments) Claim(context.Context, string, Comment) error { return nil }

// Write stores a comment by who in thread threadID on res, after the
// resource's own checks. It answers the comment as readers will see it and the
// people it mentions (by uuid), for the thread's events to tell.
func (s StoredComments) Write(ctx context.Context, who accessBusiness.Subject, res accessBusiness.Resource, threadID string, body Body) (Posted, error) {
	if len(body.Attachments) > 0 {
		return Posted{}, &Invalid{Msg: "Files can't be attached to a comment here yet."}
	}
	author, err := uuid.Parse(who.UserUUID)
	if err != nil {
		return Posted{}, errStoredNoOwner
	}
	thread, err := uuid.Parse(threadID)
	if err != nil {
		return Posted{}, ErrNotFound
	}
	html, err := safeBody(body.HTML)
	if err != nil {
		return Posted{}, err
	}
	mentioned, err := mentionsIn(ctx, html)
	if err != nil {
		return Posted{}, err
	}
	row := &models.Comment{
		ID: uuid.MustParse(storedNewID()), ThreadID: thread, ResourceKind: s.Kind, ResourceUUID: res.UUID,
		AuthorUUID: author, AuthorKind: "person", Body: html, CreatedAt: storedClock(),
	}
	if who.AgentUUID != "" {
		agent, err := uuid.Parse(who.AgentUUID)
		if err != nil {
			return Posted{}, errStoredNoOwner
		}
		row.AuthorKind, row.AgentUUID = "agent", &agent
	}
	if err := storedWrite(ctx, row); err != nil {
		return Posted{}, err
	}
	names, err := storedPeople(ctx, []string{who.UserUUID})
	if err != nil {
		names = nil
	}
	return Posted{Comment: storedComment(row, nil, names), Mentioned: mentioned}, nil
}

// safeBody is a comment's HTML as it is stored: read through the content
// codec as foreign HTML and written out again. A body with nothing left to
// read is refused, as an empty one is (validBody).
func safeBody(html string) (string, error) {
	doc, err := contentcodec.FromHTML(html, contentcodec.HTMLOptions{Foreign: true})
	if err != nil {
		return "", &Invalid{Msg: "That comment couldn't be read. Try writing it again."}
	}
	out := contentcodec.ToHTML(doc)
	if strings.TrimSpace(helpers.HTMLToPlainText(out)) == "" {
		return "", &Invalid{Msg: "Write something first."}
	}
	return out, nil
}

// mentionsIn is the people a stored body mentions, by uuid: only members of
// the workspace, each once.
func mentionsIn(ctx context.Context, html string) ([]string, error) {
	uids, err := helpers.GetMentions(html)
	if err != nil {
		return nil, &Invalid{Msg: "The people mentioned can't be read."}
	}
	if len(uids) == 0 {
		return nil, nil
	}
	found, err := storedMentioned(ctx, uids)
	if err != nil {
		return nil, err
	}
	if len(found) != len(uids) {
		return nil, &Invalid{Msg: "Someone you mentioned isn't in this workspace."}
	}
	var out []string
	seen := map[string]bool{}
	for _, u := range found {
		if u != nil && u.Uuid != "" && !seen[u.Uuid] {
			seen[u.Uuid] = true
			out = append(out, u.Uuid)
		}
	}
	return out, nil
}

// storedReaction is a reaction as a comment's comment_reactions carry it: the
// doc comment's shape, so the web app's reaction pills read it unchanged.
type storedReaction struct {
	ID      string    `json:"uid"`
	EmojiID string    `json:"reaction_emoji_id"`
	At      time.Time `json:"reaction_added_at"`
	By      Person    `json:"reaction_added_by"`
}

// storedComment is a stored row as a thread's comment, its writer and
// reactors named from names (by uuid; nil when they couldn't be read). Pure.
func storedComment(r *models.Comment, reactions []models.Reaction, names map[string]*dgraphStruct.DgraphUser) Comment {
	c := Comment{
		ID: r.ID.String(), ThreadID: r.ThreadID.String(), Text: r.Body,
		By: personNamed(r.AuthorUUID.String(), names), CreatedAt: r.CreatedAt.UTC(),
	}
	if r.EditedAt != nil {
		at := r.EditedAt.UTC()
		c.UpdatedAt = &at
	}
	if len(reactions) > 0 {
		out := make([]storedReaction, 0, len(reactions))
		for _, x := range reactions {
			out = append(out, storedReaction{ID: x.ID.String(), EmojiID: x.EmojiID, At: x.CreatedAt.UTC(), By: personNamed(x.UserUUID.String(), names)})
		}
		c.Reactions, _ = json.Marshal(out)
	}
	return c
}

// personNamed is someone by uuid as a comment shows them; one no longer in
// the workspace is a former member. Pure.
func personNamed(id string, names map[string]*dgraphStruct.DgraphUser) Person {
	if u, ok := names[id]; ok && u != nil {
		return personOf(u)
	}
	if names == nil {
		return Person{UUID: id}
	}
	return Person{UUID: id, Name: formerMember}
}

// keys is a set's members, in no set order.
func keys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}
