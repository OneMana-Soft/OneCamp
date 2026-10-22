package business

// Replies an import filed in another conversation's thread, found and put
// right (v2.75.1). Before a message was known by its conversation and its ts
// (msgkey.go), a reply whose thread ts another conversation also held could
// be filed under that conversation's message, for its people to read: a
// private channel's reply in a public channel's thread, or a DM's reply in
// another DM. A DM's reply never landed in a channel (the import refused a
// chat reply on a post).
//
// Each finished Slack import is checked once (import_jobs.replies_checked_at),
// by the hourly import clean-up, which keeps an unchecked import's export
// until it has been read:
//   - With its export, each reply the import made is compared with the
//     conversation the export says it belongs to. One in another
//     conversation is moved under its own message there, or, when that
//     message never came across (the collision dropped it), taken out: its
//     exposure matters more than keeping it. Its mappings go too, so an
//     import of the same export brings it back where it belongs.
//   - Without its export (cleared a week after the import), a DM's or
//     group's reply whose author isn't one of its people is taken out. A
//     channel's reply can't be told from its own, and the import is counted
//     as one that couldn't be checked for those.
//
// It is safe to run again: a reply where it belongs, or already taken out, is
// left alone. The admins are told counts only, in the audit log.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	adminAuditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	commentBusiness "github.com/akashc777/OneCamp/business/Comment"
	postBusiness "github.com/akashc777/OneCamp/business/Post"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	chatDomain "github.com/akashc777/OneCamp/domain/Chat"
	commentDomain "github.com/akashc777/OneCamp/domain/Comment"
	"github.com/akashc777/OneCamp/helpers"
	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

// RepairReport is what a repair found and did, in counts alone.
type RepairReport struct {
	// Checked is the imports checked.
	Checked int `json:"checked"`
	// Moved is the replies moved under their own conversation's message.
	Moved int `json:"moved"`
	// TakenOut is the replies taken out of a conversation they weren't in,
	// with no message of their own to go under.
	TakenOut int `json:"taken_out"`
	// ChannelsUnchecked is the imports whose export was gone: their channels'
	// replies couldn't be checked (their DMs' were).
	ChannelsUnchecked int `json:"channels_unchecked"`
}

func (r *RepairReport) add(o RepairReport) {
	r.Checked += o.Checked
	r.Moved += o.Moved
	r.TakenOut += o.TakenOut
	r.ChannelsUnchecked += o.ChannelsUnchecked
}

// repairBatch is how many imports one run checks.
const repairBatch = 20

// errExportGone is an import whose export is no longer kept.
var errExportGone = errors.New("the export is gone")

// RepairMisfiledReplies checks the finished Slack imports not checked yet,
// a batch a run, and records what it found for the admins. An import it
// couldn't check (the store or the graph didn't answer) stays unchecked, and
// its export kept, for the next run.
func RepairMisfiledReplies(ctx context.Context) (RepairReport, error) {
	var total RepairReport
	imports, err := importModels.UncheckedImports(ctx, repairBatch)
	if err != nil {
		return total, err
	}
	for _, im := range imports {
		r, err := repairImport(ctx, im)
		if err != nil {
			helpers.LogWarnWithContext(ctx, "SlackImport repair job=%s: left unchecked: %+v", im.Id, err)
			continue
		}
		if err := importModels.MarkRepliesChecked(ctx, im.Id); err != nil {
			helpers.LogWarnWithContext(ctx, "SlackImport repair job=%s: marking it checked: %+v", im.Id, err)
			continue
		}
		r.Checked = 1
		total.add(r)
	}
	if total.Moved+total.TakenOut+total.ChannelsUnchecked > 0 {
		recordRepair(ctx, total)
	}
	return total, nil
}

// repairImport checks one import's replies, with its export when it is kept.
func repairImport(ctx context.Context, im importModels.UncheckedImport) (RepairReport, error) {
	rows, err := importModels.RepliesMade(ctx, im.Id)
	if err != nil || len(rows) == 0 {
		return RepairReport{}, err
	}
	ids := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		ids[i] = r.Comment
	}
	homes, err := commentDomain.Homes(ctx, ids)
	if err != nil {
		return RepairReport{}, fmt.Errorf("where the replies are: %w", err)
	}
	// The repair writes as the import did: nobody is notified of history.
	ctx = context.WithValue(ctx, helpers.BulkImportContextKey, true)
	export, err := exportReplies(ctx, im)
	if errors.Is(err, errExportGone) {
		return repairWithoutExport(ctx, im, rows, homes)
	}
	if err != nil {
		return RepairReport{}, err
	}
	return repairWithExport(ctx, im, rows, homes, export)
}

// replyAt is a reply the export holds: its conversation and its thread's ts.
type replyAt struct {
	conv, thread string
}

// exportReplies is every reply the import's export holds, by its ts, with
// where each conversation came across (placeOf). errExportGone when the
// export is no longer kept.
type exportView struct {
	replies map[string][]replyAt
	// place is each conversation's OneCamp place: a channel's uuid, or a
	// DM's or group's grouping id.
	place map[string]string
}

func exportReplies(ctx context.Context, im importModels.UncheckedImport) (*exportView, error) {
	if im.RawObjectKey == nil || *im.RawObjectKey == "" {
		return nil, errExportGone
	}
	arc, done, err := OpenStagedZipFromMinIO(ctx, *im.RawObjectKey)
	if err != nil {
		if exportMissing(err) {
			return nil, errExportGone
		}
		return nil, fmt.Errorf("open the export: %w", err)
	}
	defer done()
	parsed, err := ParseManifests(arc.Reader)
	if err != nil {
		return nil, fmt.Errorf("read the export: %w", err)
	}
	view := &exportView{replies: map[string][]replyAt{}, place: map[string]string{}}
	var convs []string
	for conv, days := range parsed.MessageFiles {
		convs = append(convs, conv)
		for _, day := range days {
			zf, err := OpenInZip(arc.Reader, day)
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", day, err)
			}
			err = IterMessages(ctx, arc, zf, func(m *SlackMessage) bool {
				if m.ThreadTs != "" && m.ThreadTs != m.Ts {
					view.replies[m.Ts] = append(view.replies[m.Ts], replyAt{conv: conv, thread: m.ThreadTs})
				}
				return ctx.Err() == nil
			})
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", day, err)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if channels, err := importModels.LookupIdMappingsMany(ctx, im.Id, importModels.EntityChannel, convs); err == nil {
		for conv, id := range channels {
			if id != uuid.Nil {
				view.place[conv] = id.String()
			}
		}
	} else {
		return nil, err
	}
	dms, err := importModels.IdMapMetadataMany(ctx, im.Id, importModels.EntityDM, convs)
	if err != nil {
		return nil, err
	}
	groups, err := importModels.IdMapMetadataMany(ctx, im.Id, importModels.EntityMPIM, convs)
	if err != nil {
		return nil, err
	}
	for _, meta := range []map[string]json.RawMessage{dms, groups} {
		for conv, raw := range meta {
			var md struct {
				GroupingID string `json:"grouping_id"`
			}
			if jsonUnmarshalRaw(raw, &md) == nil && md.GroupingID != "" {
				view.place[conv] = md.GroupingID
			}
		}
	}
	return view, nil
}

// exportMissing is whether opening an export failed because the store no
// longer holds it (rather than not answering).
func exportMissing(err error) bool {
	var er minio.ErrorResponse
	return errors.As(err, &er) && er.Code == "NoSuchKey"
}

// placeOfHome is where a reply sits now: its post's channel or its chat's
// conversation, "" for neither (the graph lost its parent).
func placeOfHome(h commentDomain.Home) string {
	if h.Post != nil && h.Post.Channel != nil {
		return h.Post.Channel.UUID
	}
	return h.Grouping
}

// tsOfKey is the ts of a reply's key, written "<conversation>/<ts>" or, by
// an import before that, the bare ts. Pure.
func tsOfKey(key string) string {
	if i := strings.LastIndex(key, "/"); i >= 0 {
		return key[i+1:]
	}
	return key
}

// misfiled is where a reply at ts that sits in place belongs, by the
// export: ok false when it belongs where it is (or the export can't say);
// home "" when it belongs elsewhere but which conversation can't be told.
// Pure.
func misfiled(view *exportView, ts, place string) (home replyAt, ok bool) {
	at := view.replies[ts]
	if len(at) == 0 {
		return replyAt{}, false
	}
	var elsewhere []replyAt
	for _, r := range at {
		if view.place[r.conv] == place {
			return replyAt{}, false
		}
		elsewhere = append(elsewhere, r)
	}
	if len(elsewhere) == 1 {
		return elsewhere[0], true
	}
	return replyAt{}, true
}

func repairWithExport(ctx context.Context, im importModels.UncheckedImport, rows []importModels.ReplyRow, homes map[string]commentDomain.Home, view *exportView) (RepairReport, error) {
	var out RepairReport
	for _, row := range rows {
		h, found := homes[row.Comment.String()]
		if !found || helpers.IsSoftDeleted(h.DeletedAt) {
			continue
		}
		place := placeOfHome(h)
		if place == "" {
			continue
		}
		home, wrong := misfiled(view, tsOfKey(row.Key), place)
		if !wrong {
			continue
		}
		ts := tsOfKey(row.Key)
		moved := false
		if home.conv != "" && view.place[home.conv] != "" {
			var err error
			moved, err = moveReply(ctx, im, row, h, home, ts, view.place[home.conv])
			if err != nil {
				return out, err
			}
		}
		if err := takeOut(ctx, im, row, h); err != nil {
			return out, err
		}
		if moved {
			out.Moved++
		} else {
			out.TakenOut++
		}
	}
	return out, nil
}

func repairWithoutExport(ctx context.Context, im importModels.UncheckedImport, rows []importModels.ReplyRow, homes map[string]commentDomain.Home) (RepairReport, error) {
	var out RepairReport
	var groupings []string
	channels := false
	for _, row := range rows {
		h, found := homes[row.Comment.String()]
		if !found {
			continue
		}
		switch {
		case h.Post != nil:
			channels = true
		case h.Grouping != "" && !slices.Contains(groupings, h.Grouping):
			groupings = append(groupings, h.Grouping)
		}
	}
	if channels {
		out.ChannelsUnchecked = 1
	}
	if len(groupings) == 0 {
		return out, nil
	}
	people, err := chatDomain.GroupingPeople(ctx, groupings)
	if err != nil {
		return out, fmt.Errorf("who is in the DMs: %w", err)
	}
	for _, row := range rows {
		h, found := homes[row.Comment.String()]
		if !found || h.Grouping == "" || h.By == nil || helpers.IsSoftDeleted(h.DeletedAt) {
			continue
		}
		in, known := people[h.Grouping]
		if !known || slices.Contains(in, h.By.UUID) {
			continue
		}
		if err := takeOut(ctx, im, row, h); err != nil {
			return out, err
		}
		out.TakenOut++
	}
	return out, nil
}

// moveReply writes the reply again under its own conversation's message, as
// its author, at its time, and maps it there; false when that message never
// came across. The misfiled reply is taken out by the caller, after: a run
// stopped between the two finds the reply written already, and only takes
// the misfiled one out.
func moveReply(ctx context.Context, im importModels.UncheckedImport, row importModels.ReplyRow, h commentDomain.Home, home replyAt, ts, place string) (bool, error) {
	key := messageKey(home.conv, ts)
	if done, err := importModels.LookupIdMapping(ctx, im.Id, importModels.EntityComment, key); err != nil {
		return false, err
	} else if done != uuid.Nil && done != row.Comment {
		return true, nil
	}
	parentKey := messageKey(home.conv, home.thread)
	parent, err := importModels.LookupIdMapping(ctx, im.Id, importModels.EntityMessage, parentKey)
	if err != nil {
		return false, err
	}
	if parent == uuid.Nil {
		// The message the collision left unmapped: the one its conversation
		// holds at its thread's time, the import having written it then.
		if parent, err = importModels.MessageAt(ctx, place, h.Post != nil, slackTsToTime(home.thread)); err != nil {
			return false, err
		}
	}
	if parent == uuid.Nil || h.By == nil {
		return false, nil
	}
	author, err := personByUUID(ctx, h.By.UUID)
	if err != nil || author == nil {
		return false, nil
	}
	at := time.Now()
	if h.CreatedAt != nil {
		at = *h.CreatedAt
	}
	backdated := helpers.WithImportTimestamp(ctx, at)
	var made string
	if h.Post != nil {
		post, err := postBusiness.GetUnDeletedDgraphPostChannelBasicInfo(ctx, parent.String())
		if err != nil || post == nil {
			return false, nil
		}
		created, err := postBusiness.CreatePostComment(backdated, &postAdapter.InputCreateOrUpdateCommentToPost{
			HTMLText: h.Text, PostUuid: parent.String(),
		}, author, nil, post)
		if err != nil {
			return false, fmt.Errorf("write the reply in its own thread: %w", err)
		}
		made = created.Uuid
	} else {
		chat, err := chatBusiness.GetDgraphChatByUUID(ctx, parent.String())
		if err != nil || chat == nil {
			return false, nil
		}
		created, err := chatBusiness.CreateChatComment(backdated, &chatAdapter.InputCreateOrUpdateCommentToChat{
			HTMLText: h.Text, ChatUuid: parent.String(),
		}, author, nil, place, author.UserDgraphInfo.Uid, chat)
		if err != nil {
			return false, fmt.Errorf("write the reply in its own thread: %w", err)
		}
		made = created.Uuid
	}
	id, err := uuid.Parse(made)
	if err != nil {
		return false, fmt.Errorf("the reply written again: %w", err)
	}
	err = importModels.RecordCreated(ctx, im.Id, importModels.EntityComment, key, id, &parentKey,
		mustMarshal(map[string]interface{}{"thread_ts": home.thread, "moved_from": row.Comment.String()}), im.Workspace)
	return err == nil, err
}

// takeOut deletes the misfiled reply where it sits, as its own delete does,
// and forgets its mappings, so an import of the same export brings it back
// where it belongs.
func takeOut(ctx context.Context, im importModels.UncheckedImport, row importModels.ReplyRow, h commentDomain.Home) error {
	author := ""
	if h.By != nil {
		author = h.By.UUID
	}
	if h.Post != nil {
		c, err := commentBusiness.GetDgraphPostCommentInfoByUUID(ctx, row.Comment.String(), "")
		if err == nil && c != nil {
			if err := postBusiness.DeleteCommentOnPost(ctx, row.Comment, c, author); err != nil {
				return fmt.Errorf("take the reply out: %w", err)
			}
		}
	} else {
		c, err := commentBusiness.GetDgraphChatCommentInfoByUUID(ctx, row.Comment.String(), "")
		if err == nil && c != nil {
			chat := ""
			if c.Chat != nil {
				chat = c.Chat.Uuid
			}
			if err := chatBusiness.DeleteCommentOnChat(ctx, row.Comment, author, chat, c); err != nil {
				return fmt.Errorf("take the reply out: %w", err)
			}
		}
	}
	return importModels.ForgetMapped(ctx, im.Id, importModels.EntityComment, row.Comment)
}

// personByUUID is the person a reply was written as.
func personByUUID(ctx context.Context, id string) (*userModels.UserInfo, error) {
	u, err := uuid.Parse(id)
	if err != nil {
		return nil, err
	}
	pg, err := userBusiness.GetUserByUUID(ctx, u)
	if err != nil || pg == nil {
		return nil, err
	}
	dg, err := userBusiness.GetDgraphUserInfoByUUID(ctx, id)
	if err != nil || dg == nil {
		return nil, err
	}
	return &userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *dg}, nil
}

// recordRepair tells the admins, in the audit log, what the repair did: counts
// alone, never a name or a word of a reply.
func recordRepair(ctx context.Context, r RepairReport) {
	summary := fmt.Sprintf("Checked %d Slack imports for replies filed in another conversation: moved %d back to their own, took out %d that had none to go to", r.Checked, r.Moved, r.TakenOut)
	if r.ChannelsUnchecked > 0 {
		summary += fmt.Sprintf("; %d imports' channel replies couldn't be checked, their export being more than a week old (their DMs were)", r.ChannelsUnchecked)
	}
	err := adminAuditBusiness.RecordForPrincipal(ctx, nil, "", adminAuditBusiness.ActorSystem,
		"slack_import.replies_repaired", adminAuditBusiness.CategoryIntegration, summary,
		map[string]interface{}{"checked": r.Checked, "moved": r.Moved, "taken_out": r.TakenOut, "channels_unchecked": r.ChannelsUnchecked})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "SlackImport repair: the audit record: %+v", err)
	}
}
