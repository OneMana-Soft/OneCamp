package business

// Threads on docs. A thread's comments are the doc's own Comment nodes,
// written through the doc comment path (business/Doc CreateDocComment, so
// search, the AI index, notifications, activity and the comment event happen
// as for any doc comment) and then tagged with the thread's id.
//
// SLOT REQUEST (for the integrator): the tag is a second, small write after
// the comment. When CreateDocComment can take a thread id and write
// comment_thread_id in the comment's own mutation, Post passes it there and
// the second write goes. Until then a tag that fails three times leaves the
// comment in the doc as a page-level comment of its own: never lost, never
// written twice.

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	docAdapter "github.com/akashc777/OneCamp/adapter/Doc"
	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	threadComments "github.com/akashc777/OneCamp/models/dgraph/Thread"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

type docResource struct{}

// Seams for tests.
var (
	docCommentsOf  = threadComments.DocComments
	docCommentByID = threadComments.DocComment
	tagDocComment  = threadComments.SetCommentThread
	tagBackoff     = []time.Duration{0, 100 * time.Millisecond, 400 * time.Millisecond}
)

func (docResource) Anchors() []AnchorKind {
	return []AnchorKind{AnchorYRange, AnchorBlock, AnchorNone}
}

func (docResource) Access(ctx context.Context, who accessBusiness.Subject, res accessBusiness.Resource) (accessBusiness.Decision, error) {
	d, err := accessBusiness.Rules.Resolve(ctx, who, res)
	if errors.Is(err, docBusiness.ErrNotFound) {
		return d, ErrNotFound
	}
	return d, err
}

func (docResource) Comments(ctx context.Context, _ accessBusiness.Subject, res accessBusiness.Resource) ([]Comment, error) {
	rows, ok, err := docCommentsOf(ctx, res.UUID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotFound
	}
	out := make([]Comment, 0, len(rows))
	for _, r := range rows {
		out = append(out, commentOf(r))
	}
	return out, nil
}

func (docResource) Post(ctx context.Context, who accessBusiness.Subject, res accessBusiness.Resource, threadID string, body Body) (Posted, error) {
	poster, ok := userModels.FromContext(ctx)
	if !ok || poster.UserDgraphInfo.Uuid != who.UserUUID || poster.UserDgraphInfo.Uid == "" {
		return Posted{}, &Refusal{Msg: "Sign in again to comment."}
	}
	doc, err := docBusiness.GetBasicDgraphDocByUUID(ctx, res.UUID, who.UserUID)
	if errors.Is(err, docBusiness.ErrNotFound) || (err == nil && doc == nil) {
		return Posted{}, ErrNotFound
	}
	if err != nil {
		return Posted{}, err
	}
	if doc.CreatedBy == nil {
		return Posted{}, errors.New("threads: the doc has no owner on record")
	}
	if doc.IsPrivate == nil { // the comment path reads it as a value
		public := false
		doc.IsPrivate = &public
	}

	files, err := attachmentsOf(body.Attachments)
	if err != nil {
		return Posted{}, err
	}
	uids, err := helpers.GetMentions(body.HTML)
	if err != nil {
		return Posted{}, &Invalid{Msg: "The people mentioned can't be read."}
	}
	mentioned, err := userBusiness.GetDgraphUserInfoByUUIDs(ctx, uids)
	if len(mentioned) != len(uids) {
		if err == nil {
			err = &Invalid{Msg: "Someone you mentioned isn't in this workspace."}
		}
		return Posted{}, err
	}

	in := &docAdapter.CreateOrUpdateDocCommentInput{CommentBody: body.HTML, DocUuid: res.UUID, Attachments: files}
	out, err := docBusiness.CreateDocComment(ctx, doc, poster, in, mentioned)
	if err != nil {
		return Posted{}, err
	}
	tagWithRetry(ctx, out.Uuid, threadID)

	p := Posted{Comment: Comment{
		ID: out.Uuid, ThreadID: threadID, Text: body.HTML,
		By:        personOf(&poster.UserDgraphInfo),
		CreatedAt: out.CommentCreatedAt.UTC(), Attachments: shownFiles(in.Attachments),
	}}
	for _, m := range mentioned {
		if m != nil && m.Uuid != "" {
			p.Mentioned = append(p.Mentioned, m.Uuid)
		}
	}
	return p, nil
}

// maxAttachments is the most files one comment carries.
const maxAttachments = 20

// attachmentsOf reads a comment's files as the client describes them (the
// upload already stored them) and keeps only what the client may say: names,
// keys, kinds and sizes. A node id, an author, a deletion or a link to a task
// or project from the client would be written into the graph as given, so
// they are never passed on.
func attachmentsOf(raw json.RawMessage) ([]*dgraphStruct.DgraphAttachment, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var in []*dgraphStruct.DgraphAttachment
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, &Invalid{Msg: "Those attachments can't be read."}
	}
	if len(in) > maxAttachments {
		return nil, &Invalid{Msg: "Attach at most 20 files to one comment."}
	}
	var out []*dgraphStruct.DgraphAttachment
	for _, f := range in {
		if f == nil || f.ObjectKey == "" {
			continue
		}
		out = append(out, clientFields(f))
	}
	return out, nil
}

// clientFields is the fields of a file the client may say: names, keys,
// kinds and sizes.
func clientFields(f *dgraphStruct.DgraphAttachment) *dgraphStruct.DgraphAttachment {
	return &dgraphStruct.DgraphAttachment{
		Uuid: f.Uuid, FileName: f.FileName, ObjectKey: f.ObjectKey, Type: f.Type, RawType: f.RawType,
		Width: f.Width, Hight: f.Hight, Size: f.Size, Duration: f.Duration,
	}
}

// shownFiles is what a comment shows of the files it kept (CreateDocComment
// leaves them in its input: its writer's own uploads to the doc, each with the
// key its upload stored), with the fields the client may say.
func shownFiles(kept []*dgraphStruct.DgraphAttachment) json.RawMessage {
	if len(kept) == 0 {
		return nil
	}
	out := make([]*dgraphStruct.DgraphAttachment, 0, len(kept))
	for _, f := range kept {
		out = append(out, clientFields(f))
	}
	shown, _ := json.Marshal(out)
	return shown
}

// tagWithRetry puts a just-written comment in its thread, trying three times.
func tagWithRetry(ctx context.Context, commentID, threadID string) {
	var err error
	for _, wait := range tagBackoff {
		if wait > 0 && !helpers.SleepCtx(ctx, wait) {
			err = ctx.Err()
			break
		}
		if err = tagDocComment(ctx, commentID, threadID); err == nil {
			return
		}
	}
	helpers.LogErrorWithContext(ctx, "business/Thread: comment %s could not be put in thread %s; it reads as a page-level comment: %+v", commentID, threadID, err)
}

func (docResource) Legacy(ctx context.Context, commentID string) (Thread, Comment, bool, error) {
	row, err := docCommentByID(ctx, commentID)
	if errors.Is(err, threadComments.ErrNoComment) {
		return Thread{}, Comment{}, false, nil
	}
	if err != nil {
		return Thread{}, Comment{}, false, err
	}
	if row.ThreadID != "" && row.ThreadID != row.Uuid {
		return Thread{}, Comment{}, false, nil // in another thread
	}
	c := commentOf(*row)
	c.ThreadID = row.Uuid
	return legacyThread(accessBusiness.KindDoc, row.Doc.Uuid, c), c, true, nil
}

func (docResource) Claim(ctx context.Context, threadID string, root Comment) error {
	return tagDocComment(ctx, root.ID, threadID)
}

func commentOf(r threadComments.CommentRow) Comment {
	c := Comment{
		ID: r.Uuid, ThreadID: r.ThreadID, Text: r.Text, By: personOf(r.By),
		UpdatedAt: r.UpdatedAt, Attachments: nullless(r.Attachments), Reactions: nullless(r.Reactions),
	}
	if r.CreatedAt != nil {
		c.CreatedAt = r.CreatedAt.UTC()
	}
	return c
}

func personOf(u *dgraphStruct.DgraphUser) Person {
	if u == nil {
		return Person{}
	}
	p := Person{UUID: u.Uuid, Name: u.DisplayName()}
	if u.ProfileKey != nil {
		p.ProfileKey = *u.ProfileKey
	}
	return p
}

func nullless(b json.RawMessage) json.RawMessage {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	return b
}
