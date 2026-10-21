package business

// Sharing earlier messages with a new set of people (D-S1): a scheduler job,
// conversation_share, copies a DM's or group's messages from a time on into
// the group the new set has (or the private channel a group became), a page
// at a time, oldest first, each with its replies and files.
//
// A copy is its original's words, files, author and time. It notifies nobody
// (no mention node, no reply-to-you edge, no push, no Activity), is read for
// everyone (it keeps its original's time, before everyone's read mark), and is
// searched, and in the AI edition embedded, as content of the new
// conversation. Its files are new attachment rows on the same stored objects,
// so they are served to the new conversation's people, and the purge keeps an
// object while any row still shows it.
//
// Left out, so nothing reaches people it wasn't shown to: forwards (what they
// forwarded belongs to another conversation), system lines, deleted messages
// and replies, a quote of a message the share doesn't copy, and a poll's live
// block (its votes are the first conversation's; the copy keeps its question).
//
// Each copy is claimed in the messages ledger before it is written, so a run
// that is tried again, or a second worker, copies each message once. A run
// works for at most shareRunFor and then queues the rest as a job of its own.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	refsBusiness "github.com/akashc777/OneCamp/business/Refs"
	schedulerBusiness "github.com/akashc777/OneCamp/business/Scheduler"
	attachmentDomain "github.com/akashc777/OneCamp/domain/Attachment"
	chatDomain "github.com/akashc777/OneCamp/domain/Chat"
	commentDomain "github.com/akashc777/OneCamp/domain/Comment"
	conversationDomain "github.com/akashc777/OneCamp/domain/Conversation"
	postDomain "github.com/akashc777/OneCamp/domain/Post"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	postgressStruct "github.com/akashc777/OneCamp/models/postgres"
	models "github.com/akashc777/OneCamp/models/postgres/Conversation"
	jobModel "github.com/akashc777/OneCamp/models/postgres/ScheduledJob"
	"github.com/google/uuid"
)

// shareJob is the scheduler's job type.
const shareJob = "conversation_share"

const (
	// shareCap is the most messages and replies one share copies: a copy
	// writes the graph, Postgres, search and, in the AI edition, an
	// embedding, so an unbounded share is a load any member could start.
	shareCap = 5000
	// sharePageSize is how many messages one read takes.
	sharePageSize = 50
)

// shareRunFor is how long one run copies before it queues the rest, well
// inside the scheduler's limit for one job.
var shareRunFor = 40 * time.Second

func init() { schedulerBusiness.RegisterJobHandler(shareJob, runShare) }

// sharePayload is a share's job: what is copied where, and how far it got.
type sharePayload struct {
	From   string    `json:"from"`    // the DM's or group's grouping id
	ToKind string    `json:"to_kind"` // KindGroup or KindChannel
	To     string    `json:"to"`      // the group's grouping id, or the channel's uuid
	Since  time.Time `json:"since"`   // the zero time is everything
	Until  time.Time `json:"until"`
	By     string    `json:"by"`
	Cap    int       `json:"cap"`
	// The cursor: the last message passed; how much the target holds of what
	// was passed (Copied, which the cap counts) and how much of it runs of this
	// share made (Made, which its line counts: a share asked for again makes
	// nothing and says nothing).
	At     time.Time `json:"at,omitempty"`
	After  string    `json:"after,omitempty"`
	Copied int       `json:"copied"`
	Made   int       `json:"made"`
}

// Seams for tests.
var (
	sharePage    = conversationDomain.SharePage
	claimCopy    = models.ClaimCopy
	releaseCopy  = models.ReleaseCopy
	copiesOf     = models.CopiesOf
	enqueueJob   = schedulerBusiness.Enqueue
	targetOf     = loadTarget
	shareFinish  = finishShare
	shareRunTime = time.Now
)

// queueShare starts a share.
func queueShare(ctx context.Context, p sharePayload) error {
	if p.Cap <= 0 || p.Cap > shareCap {
		p.Cap = shareCap
	}
	return enqueueShare(ctx, p, time.Now())
}

func enqueueShare(ctx context.Context, p sharePayload, at time.Time) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	by, _ := uuid.Parse(p.By)
	_, err = enqueueJob(ctx, schedulerBusiness.EnqueueInput{JobType: shareJob, UserUUID: by, PayloadJSON: string(raw), RunAt: at})
	return err
}

// runShare is one run of a share.
func runShare(ctx context.Context, job *jobModel.ScheduledJob) error {
	var p sharePayload
	if err := json.Unmarshal([]byte(job.Payload), &p); err != nil || p.From == "" || p.To == "" {
		helpers.LogErrorWithContext(ctx, "business/Conversation a share job (%s) can't be read, dropped: %v", job.Id, err)
		return nil
	}
	return shareRun(ctx, p)
}

// shareRun copies until the share is done, its cap is reached or its time is
// up, and then finishes it or queues the rest.
func shareRun(ctx context.Context, p sharePayload) error {
	t, err := targetOf(ctx, p)
	if err != nil {
		return err
	}
	if t == nil {
		helpers.LogErrorWithContext(ctx, "business/Conversation a share into %s %s found nowhere to copy to, dropped", p.ToKind, p.To)
		return nil
	}
	stop := shareRunTime().Add(shareRunFor)
	for p.Copied < p.Cap {
		if shareRunTime().After(stop) {
			// The rest in a job of its own: one run never outlives its lock.
			return enqueueShare(ctx, p, shareRunTime())
		}
		page, err := sharePage(ctx, p.From, p.Since, p.Until, p.At, p.After, sharePageSize)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			break
		}
		for _, m := range page {
			if p.Copied >= p.Cap {
				break
			}
			if m.By != nil {
				held, made, err := copyMessage(ctx, t, m, p.Cap-p.Copied)
				if err != nil {
					return err
				}
				p.Copied += held
				p.Made += made
			}
			p.At, p.After = m.At, m.UUID
		}
		if len(page) < sharePageSize {
			break
		}
	}
	return shareFinish(ctx, t, p)
}

// copyTarget is where a share writes: a group or a channel.
type copyTarget interface {
	// conversation is the target's row in the index (the ledger's key).
	conversation() uuid.UUID
	// writeMessage writes the copy id of m.
	writeMessage(ctx context.Context, id uuid.UUID, m conversationDomain.SharedMessage, replyTo string) error
	// writeReply writes the copy id of r, in the thread of the copy root.
	writeReply(ctx context.Context, root, id uuid.UUID, r conversationDomain.SharedReply) error
	// shared tells the target's open apps the share is done, with a line.
	shared(ctx context.Context, by *dgraphStruct.DgraphUser, from string, count int) error
}

// copyMessage copies m and up to room-1 of its replies (room counts the
// message too), and says how many of them the target holds now (held: made by
// this run or an earlier one) and how many this run made.
func copyMessage(ctx context.Context, t copyTarget, m conversationDomain.SharedMessage, room int) (held, made int, err error) {
	author, err := uuid.Parse(m.By.Uuid)
	if err != nil {
		return 0, 0, nil
	}
	original, err := uuid.Parse(m.UUID)
	if err != nil {
		return 0, 0, nil
	}
	conv := t.conversation()
	root := uuid.New()
	claimed, err := claimCopy(ctx, models.Copy{ID: root, Conversation: conv, Author: author, CopiedFrom: original, At: m.At})
	if err != nil {
		return 0, 0, err
	}
	if claimed {
		replyTo := ""
		if parent, err := uuid.Parse(m.ReplyTo); err == nil {
			// A quote of a message the share copied points at its copy; one
			// of a message it didn't is left out.
			if got, err := copiesOf(ctx, conv, []uuid.UUID{parent}); err == nil {
				if c, ok := got[parent]; ok {
					replyTo = c.String()
				}
			}
		}
		if err := t.writeMessage(ctx, root, m, replyTo); err != nil {
			if rerr := releaseCopy(ctx, root); rerr != nil {
				helpers.LogErrorWithContext(ctx, "business/Conversation a copy that failed couldn't be released: %+v", rerr)
			}
			return 0, 0, err
		}
		made++
	} else {
		got, err := copiesOf(ctx, conv, []uuid.UUID{original})
		if err != nil {
			return 0, 0, err
		}
		c, ok := got[original]
		if !ok {
			return 0, 0, fmt.Errorf("the copy of %s was claimed and isn't there", m.UUID)
		}
		root = c
	}
	held = 1
	for _, r := range m.Replies {
		if held >= room {
			break
		}
		by, err1 := uuid.Parse(r.By.Uuid)
		from, err2 := uuid.Parse(r.UUID)
		if err1 != nil || err2 != nil {
			continue
		}
		id := uuid.New()
		claimed, err := claimCopy(ctx, models.Copy{ID: id, Conversation: conv, Author: by, CopiedFrom: from, ThreadRoot: &root, At: r.At})
		if err != nil {
			return held, made, err
		}
		if claimed {
			if err := t.writeReply(ctx, root, id, r); err != nil {
				if rerr := releaseCopy(ctx, id); rerr != nil {
					helpers.LogErrorWithContext(ctx, "business/Conversation a copy that failed couldn't be released: %+v", rerr)
				}
				return held, made, err
			}
			made++
		}
		held++
	}
	return held, made, nil
}

// pollBlock is a poll's live block in a message (business/Poll PollHTML).
var pollBlock = regexp.MustCompile(`(?is)<div[^>]*data-type="poll"[^>]*>\s*</div>`)

// copiedHTML is a message's words as its copy carries them: without a poll's
// live block, whose votes are the first conversation's. Pure.
func copiedHTML(s string) string {
	return pollBlock.ReplaceAllString(s, "")
}

// copiedFiles is new attachment nodes for files, on the same stored objects.
func copiedFiles(files []*dgraphStruct.DgraphAttachment, by *dgraphStruct.DgraphUser, at time.Time) []*dgraphStruct.DgraphAttachment {
	out := make([]*dgraphStruct.DgraphAttachment, 0, len(files))
	for _, f := range files {
		if f == nil || f.ObjectKey == "" {
			continue
		}
		c := *f
		c.Uid, c.Uuid = "", uuid.NewString()
		c.DType = []string{"Attachment"}
		c.CreatedBy = &dgraphStruct.DgraphUser{Uid: by.Uid}
		c.CreatedAt = &at
		c.DeletedAt = nil
		out = append(out, &c)
	}
	return out
}

// fileRows gives the place's people the copied files: an attachments row per
// file, on its stored object.
func fileRows(ctx context.Context, files []*dgraphStruct.DgraphAttachment, by uuid.UUID, kind, place string) error {
	for _, f := range files {
		id, err := uuid.Parse(f.Uuid)
		if err != nil {
			id = uuid.New()
		}
		if err := attachmentDomain.CreateAttachment(ctx, id, f.ObjectKey, by, kind, place); err != nil {
			return err
		}
	}
	return nil
}

// ------------------------------------------------------------------ a group

type groupTarget struct {
	conv       uuid.UUID
	groupingID string
	people     []*dgraphStruct.DgraphUser
}

func (g *groupTarget) conversation() uuid.UUID { return g.conv }

func (g *groupTarget) participants() []*openSearchStruct.OpenSearchChatParticipants {
	ps := make([]*openSearchStruct.OpenSearchChatParticipants, 0, len(g.people))
	for _, p := range g.people {
		ps = append(ps, &openSearchStruct.OpenSearchChatParticipants{Uuid: p.Uuid, Name: p.DisplayName(), ProfileKey: p.ProfileKey})
	}
	return ps
}

func (g *groupTarget) writeMessage(ctx context.Context, id uuid.UUID, m conversationDomain.SharedMessage, replyTo string) error {
	author, _ := uuid.Parse(m.By.Uuid)
	at := m.At
	zero := time.Time{}
	files := copiedFiles(m.Attachments, m.By, at)
	if err := fileRows(ctx, files, author, postgressStruct.ATTACHMENT_SRC_GRP_CHAT, g.groupingID); err != nil {
		return err
	}
	if err := chatDomain.CreateChatAt(ctx, id, author, g.groupingID, at); err != nil {
		return err
	}
	body := copiedHTML(m.HTML)
	dm := dgraphStruct.DgraphDm{
		Uid: "uid(dm)", GroupingId: g.groupingID, DType: []string{"Dm"},
		Chats: []*dgraphStruct.DgraphChat{{
			Uid: "uid(ch)", Uuid: id.String(), DType: []string{"Chat"},
			CreatedAt: &at, DeletedAt: &zero,
			From:     &dgraphStruct.DgraphUser{Uid: m.By.Uid},
			DM:       &dgraphStruct.DgraphDm{Uid: "uid(dm)"},
			Body:     body,
			MediaObj: files,
			CopyOf:   m.UUID,
		}},
	}
	if _, _, err := chatDomain.CreateDgraphChat(ctx, &dm, id.String()); err != nil {
		_ = helpers.CompensateOnFailure(ctx, "copied chat row "+id.String(), func(c context.Context) error {
			return chatDomain.HardDeleteChatByUUID(c, id)
		})
		return err
	}
	refsBusiness.Saved(ctx, refsBusiness.Chat, id.String(), m.HTML, true)
	if replyTo != "" {
		if err := chatDomain.LinkReplyTo(ctx, id.String(), replyTo); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Conversation a copy's quote wasn't linked: %+v", err)
		}
	}
	plain := helpers.HTMLToPlainText(body)
	ps := g.participants()
	helpers.GoSafeNamed("conversation.share.search", func() {
		chatDomain.CreateChatWithAttachmentsInOpenSearch(&openSearchStruct.OpenSearchChat{
			Uuid: id.String(), ChatCreatedAt: at.Unix(), ChatBody: plain,
			ChatByProfile: m.By.ProfileKey, ChatByUserFullName: m.By.DisplayName(), ChatByUserUuid: m.By.Uuid,
			ChatGrpId: g.groupingID, ChatParticipants: ps,
		}, files)
	})
	/* AI call omitted in v1 */
	return nil
}

func (g *groupTarget) writeReply(ctx context.Context, root, id uuid.UUID, r conversationDomain.SharedReply) error {
	author, _ := uuid.Parse(r.By.Uuid)
	at := r.At
	zero := time.Time{}
	files := copiedFiles(r.Attachments, r.By, at)
	if err := fileRows(ctx, files, author, postgressStruct.ATTACHMENT_SRC_GRP_CHAT, g.groupingID); err != nil {
		return err
	}
	if err := commentDomain.CreateComment(ctx, id, author, at); err != nil {
		return err
	}
	chat := dgraphStruct.DgraphChat{
		Uid: "uid(cha)", Uuid: root.String(),
		Comments: []*dgraphStruct.DgraphComment{{
			DType: []string{"Comment"}, Uid: "uid(co)", Uuid: id.String(),
			Text:           r.HTML,
			Chat:           &dgraphStruct.DgraphChat{Uid: "uid(cha)"},
			ChatGroupingId: g.groupingID,
			Attachments:    files,
			CreatedAt:      &at, DeletedAt: &zero,
			CommentBy: &dgraphStruct.DgraphUser{Uid: r.By.Uid},
		}},
	}
	if _, err := commentDomain.CreateOrUpdateCommentInChat(ctx, &chat, id.String()); err != nil {
		return err
	}
	refsBusiness.Saved(ctx, refsBusiness.Comment, id.String(), r.HTML, true)
	plain := helpers.HTMLToPlainText(r.HTML)
	ps := g.participants()
	helpers.GoSafeNamed("conversation.share.search", func() {
		commentDomain.CreateChatCommentWithAttachmentsInOpenSearch(&openSearchStruct.OpenSearchComment{
			Uuid: id.String(), CommentBody: plain, CommentByUserUuid: r.By.Uuid,
			CommentChatGrpId: g.groupingID, CommentChatUuid: root.String(), CommentCreatedAt: at.Unix(),
			CommentByUserFullName: r.By.DisplayName(), CommentByProfile: r.By.ProfileKey,
			CommentChatFromUserUuid: r.By.Uuid, CommentChatParticipants: ps,
		}, files)
	})
	/* AI call omitted in v1 */
	return nil
}

func (g *groupTarget) shared(ctx context.Context, by *dgraphStruct.DgraphUser, from string, count int) error {
	if count == 0 {
		return nil
	}
	_, err := writeGroupLine(ctx, g.groupingID, by, Line{Kind: LineHistoryShared, Actor: by.Uuid, From: from, Count: count}, sharedText(count), nil)
	return err
}

// sharedText is the line a finished share writes. Pure.
func sharedText(count int) string {
	if count == 1 {
		return "1 earlier message was shared from the conversation before."
	}
	return fmt.Sprintf("%d earlier messages and replies were shared from the conversation before.", count)
}

// --------------------------------------------------------------- a channel

type channelTarget struct {
	conv        uuid.UUID
	channelUUID uuid.UUID
	channelUID  string
	name        string
}

func (c *channelTarget) conversation() uuid.UUID { return c.conv }

func (c *channelTarget) writeMessage(ctx context.Context, id uuid.UUID, m conversationDomain.SharedMessage, replyTo string) error {
	author, _ := uuid.Parse(m.By.Uuid)
	at := m.At
	zero := time.Time{}
	files := copiedFiles(m.Attachments, m.By, at)
	if err := fileRows(ctx, files, author, postgressStruct.ATTACHMENT_SRC_CHANNEL, c.channelUUID.String()); err != nil {
		return err
	}
	if err := postDomain.CreatePostAt(ctx, id, author, c.channelUUID, at); err != nil {
		return err
	}
	body := copiedHTML(m.HTML)
	post := dgraphStruct.DgraphPost{
		Uid: "uid(po)", Uuid: id.String(), DType: []string{"Post"},
		Text: body, MediaObj: files,
		PostBy:    &dgraphStruct.DgraphUser{Uid: m.By.Uid, Posts: []*dgraphStruct.DgraphPost{{Uid: "uid(po)"}}},
		Channel:   &dgraphStruct.DgraphChannel{Uid: c.channelUID, Posts: []*dgraphStruct.DgraphPost{{Uid: "uid(po)"}}},
		CreatedAt: &at, DeletedAt: &zero,
		CopyOf: m.UUID,
	}
	if _, err := postDomain.CreateOrUpdateDgraphPost(ctx, &post); err != nil {
		_ = helpers.CompensateOnFailure(ctx, "copied post row "+id.String(), func(cc context.Context) error {
			return postDomain.HardDeletePostByUUIUD(cc, id)
		})
		return err
	}
	refsBusiness.Saved(ctx, refsBusiness.Message, id.String(), m.HTML, true)
	plain := helpers.HTMLToPlainText(body)
	helpers.GoSafeNamed("conversation.share.search", func() {
		postDomain.CreatePostWithAttachmentsInOpenSearch(&openSearchStruct.OpenSearchPost{
			Uuid: id.String(), PostBody: plain, PostChannelUuid: c.channelUUID.String(), PostChannelName: c.name,
			PostByUserUuid: m.By.Uuid, PostByProfile: m.By.ProfileKey, PostByUserFullName: m.By.DisplayName(),
			PostCreatedAt: at.Unix(), PostUpdatedAt: at.Unix(),
		}, files)
	})
	/* AI call omitted in v1 */
	return nil
}

func (c *channelTarget) writeReply(ctx context.Context, root, id uuid.UUID, r conversationDomain.SharedReply) error {
	author, _ := uuid.Parse(r.By.Uuid)
	at := r.At
	zero := time.Time{}
	files := copiedFiles(r.Attachments, r.By, at)
	if err := fileRows(ctx, files, author, postgressStruct.ATTACHMENT_SRC_CHANNEL, c.channelUUID.String()); err != nil {
		return err
	}
	if err := commentDomain.CreateComment(ctx, id, author, at); err != nil {
		return err
	}
	post := dgraphStruct.DgraphPost{
		Uid: "uid(po)", Uuid: root.String(),
		Comments: []*dgraphStruct.DgraphComment{{
			DType: []string{"Comment"}, Uid: "uid(co)", Uuid: id.String(),
			Text:        r.HTML,
			Post:        &dgraphStruct.DgraphPost{Uid: "uid(po)"},
			Attachments: files,
			CreatedAt:   &at, DeletedAt: &zero,
			CommentBy: &dgraphStruct.DgraphUser{Uid: r.By.Uid},
		}},
	}
	if _, err := commentDomain.CreateDgraphCommentInAPost(ctx, &post, id.String()); err != nil {
		return err
	}
	refsBusiness.Saved(ctx, refsBusiness.Comment, id.String(), r.HTML, true)
	plain := helpers.HTMLToPlainText(r.HTML)
	helpers.GoSafeNamed("conversation.share.search", func() {
		commentDomain.CreatePostCommentWithAttachmentsInOpenSearch(&openSearchStruct.OpenSearchComment{
			Uuid: id.String(), CommentBody: plain, CommentByUserUuid: r.By.Uuid,
			CommentPostUuid: root.String(), CommentChannelUuid: c.channelUUID.String(), CommentCreatedAt: at.Unix(),
			CommentByUserFullName: r.By.DisplayName(), CommentByProfile: r.By.ProfileKey,
		}, files)
	})
	/* AI call omitted in v1 */
	return nil
}

func (c *channelTarget) shared(ctx context.Context, by *dgraphStruct.DgraphUser, from string, count int) error {
	if count == 0 {
		return nil
	}
	_, err := writeChannelLine(ctx, c.channelUUID.String(), c.channelUID, by, Line{Kind: LineHistoryShared, Actor: by.Uuid, From: from, Count: count}, sharedText(count))
	return err
}

// ------------------------------------------------------------ the target

// errShareNowhere is a share whose target has gone.
var errShareNowhere = errors.New("conversation: nowhere to share into")

// loadTarget is where p copies to, or nil when it has gone.
func loadTarget(ctx context.Context, p sharePayload) (copyTarget, error) {
	row, err := models.ByRef(ctx, p.To)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, nil
	}
	switch p.ToKind {
	case KindGroup:
		people, err := groupPeopleOf(ctx, p.To)
		if errors.Is(err, errShareNowhere) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return &groupTarget{conv: row.ID, groupingID: p.To, people: people}, nil
	case KindChannel:
		id, err := uuid.Parse(p.To)
		if err != nil {
			return nil, nil
		}
		ch, err := channelNode(ctx, id)
		if errors.Is(err, errShareNowhere) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return &channelTarget{conv: row.ID, channelUUID: id, channelUID: ch.Uid, name: ch.Name}, nil
	}
	return nil, nil
}

// finishShare tells the target's people, with a line, and their open apps
// read the conversation again.
func finishShare(ctx context.Context, t copyTarget, p sharePayload) error {
	by, err := personByUUID(ctx, p.By)
	if err != nil {
		return err
	}
	if by == nil {
		return nil
	}
	if err := t.shared(ctx, by, p.From, p.Made); err != nil {
		return err
	}
	helpers.LogInfoWithContext(ctx, "business/Conversation shared %d messages and replies (%d new) from %s into %s %s", p.Copied, p.Made, p.From, p.ToKind, p.To)
	refreshOpenApps(ctx, p.ToKind, p.To)
	return nil
}
