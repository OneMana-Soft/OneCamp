package entityLinkBusiness

// The kinds the work graph (DG5) names beside docs, tasks, projects and
// people, each read and decided for one reader as a chip is, so the Connected
// panel, the chips and ReadableTitles all name a thing by the one rule:
//
//   - a channel as the Access Resolver decides it (business/Conversation);
//   - a channel post as its channel is, and gone when it's deleted;
//   - a direct or group message as its conversation is (its people), and
//     gone when it's deleted;
//   - a whiteboard by its own rule (business/Board CanRead);
//   - a team for its members and admins only;
//   - a comment as what it's on (a task, a doc, a post or a direct message);
//   - a memory item for its owner alone.

import (
	"context"
	"strings"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	domain "github.com/akashc777/OneCamp/domain/EntityLink"
	"github.com/akashc777/OneCamp/helpers"
)

// snippetLen is how much of a post's or a comment's text an entry shows.
const snippetLen = 120

// Where each kind reads from (seams for tests).
var (
	readChannelChips = domain.ChannelChips
	readMessageChips = domain.MessageChips
	readChatChips    = domain.ChatChips
	readTeamChips    = domain.TeamChips
	readCommentChips = domain.CommentChips
	readMemoryChips  = domain.MemoryChips
	readBoardTitles  = boardBusiness.ReadableBoardTitles
)

func init() {
	RegisterKind("channel", readChannels)
	RegisterKind("message", readMessages)
	RegisterKind("chat", readChats)
	RegisterKind("board", readBoards)
	RegisterKind("team", readTeams)
	RegisterKind("comment", readComments)
	RegisterKind("memory", readMemory)
}

func deleted(t *time.Time) bool { return helpers.IsSoftDeleted(t) }

func snippet(html string) string { return helpers.OneLine(helpers.HTMLToPlainText(html), snippetLen) }

// channelLevels is the reader's decision on each channel, by lower-case uuid,
// in one call to the resolver.
func channelLevels(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]accessBusiness.Decision, error) {
	out := map[string]accessBusiness.Decision{}
	if len(ids) == 0 {
		return out, nil
	}
	res := make([]accessBusiness.Resource, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if l := strings.ToLower(id); !seen[l] {
			seen[l] = true
			res = append(res, accessBusiness.Resource{Kind: "channel", UUID: l})
		}
	}
	got, err := accessBusiness.ResolveAll(ctx, who, res)
	if err != nil {
		return nil, err
	}
	for r, d := range got {
		out[strings.ToLower(r.UUID)] = d
	}
	return out, nil
}

// readChannels: a channel the reader may read, and gone when it was deleted.
func readChannels(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]Entity, error) {
	asked := askedAs(ids)
	levels, err := channelLevels(ctx, who, ids)
	if err != nil {
		return nil, err
	}
	var open []string
	for id, d := range levels {
		if d.Level >= accessBusiness.LevelView || d.Was >= accessBusiness.LevelView {
			open = append(open, id)
		}
	}
	chips, err := readChannelChips(ctx, open)
	if err != nil {
		return nil, err
	}
	out := map[string]Entity{}
	for _, c := range chips {
		lower := strings.ToLower(c.UUID)
		e := Entity{Access: AccessGone}
		if !deleted(c.DeletedAt) {
			e = Entity{Access: AccessOK, Title: c.Name, Data: map[string]any{"name": c.Name, "private": c.Private}}
		}
		for _, id := range asked[lower] {
			out[id] = e
		}
	}
	return out, nil
}

// readMessages: a post the reader may read in its channel; gone once deleted.
func readMessages(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]Entity, error) {
	asked := askedAs(ids)
	posts, err := readMessageChips(ctx, ids)
	if err != nil {
		return nil, err
	}
	var channels []string
	for _, p := range posts {
		if p.Channel != nil && p.Channel.UUID != "" {
			channels = append(channels, p.Channel.UUID)
		}
	}
	levels, err := channelLevels(ctx, who, channels)
	if err != nil {
		return nil, err
	}
	out := map[string]Entity{}
	for _, p := range posts {
		if p.Channel == nil || levels[strings.ToLower(p.Channel.UUID)].Level < accessBusiness.LevelView {
			continue
		}
		e := Entity{Access: AccessGone}
		if !deleted(p.DeletedAt) && !deleted(p.Channel.DeletedAt) {
			text := snippet(p.Text)
			data := map[string]any{"snippet": text, "channel_id": strings.ToLower(p.Channel.UUID), "channel_name": p.Channel.Name}
			if p.By != nil && p.By.UUID != "" {
				data["by_uuid"], data["by_name"] = p.By.UUID, p.By.Display()
			}
			if p.CreatedAt != nil && !p.CreatedAt.IsZero() {
				data["created_at"] = p.CreatedAt.UTC().Format(time.RFC3339)
			}
			e = Entity{Access: AccessOK, Title: text, Data: data}
		}
		for _, id := range asked[strings.ToLower(p.UUID)] {
			out[id] = e
		}
	}
	return out, nil
}

// conversationOf is the Access Resolver's resource for a direct or group
// message's conversation, by its grouping id: a DM's is its two people's ids
// with a space between, a group's has none (business/Conversation ChatRef,
// which this package doesn't import: the rules are registered under these
// kinds by that package's init).
func conversationOf(groupingID string) accessBusiness.Resource {
	if strings.Contains(groupingID, " ") {
		return accessBusiness.Resource{Kind: "dm", UUID: groupingID}
	}
	return accessBusiness.Resource{Kind: "group", UUID: groupingID}
}

// readChats: a direct or group message its conversation's people may read,
// each conversation decided once, all in one call; gone once deleted.
func readChats(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]Entity, error) {
	asked := askedAs(ids)
	out := map[string]Entity{}
	if who.UserUUID == "" {
		return out, nil
	}
	chats, err := readChatChips(ctx, ids)
	if err != nil {
		return nil, err
	}
	var res []accessBusiness.Resource
	seen := map[accessBusiness.Resource]bool{}
	for _, c := range chats {
		if c.Dm == nil || c.Dm.GroupingID == "" {
			continue
		}
		if r := conversationOf(c.Dm.GroupingID); !seen[r] {
			seen[r] = true
			res = append(res, r)
		}
	}
	if len(res) == 0 {
		return out, nil
	}
	decided, err := accessBusiness.ResolveAll(ctx, who, res)
	if err != nil {
		return nil, err
	}
	for _, c := range chats {
		if c.Dm == nil || c.Dm.GroupingID == "" || decided[conversationOf(c.Dm.GroupingID)].Level < accessBusiness.LevelView {
			continue
		}
		e := Entity{Access: AccessGone}
		if !deleted(c.DeletedAt) {
			text := snippet(c.Text)
			data := map[string]any{"snippet": text, "path": helpers.ChatPath(c.Dm.GroupingID, who.UserUUID, strings.ToLower(c.UUID))}
			if c.By != nil && c.By.UUID != "" {
				data["by_uuid"], data["by_name"] = c.By.UUID, c.By.Display()
			}
			if c.CreatedAt != nil && !c.CreatedAt.IsZero() {
				data["created_at"] = c.CreatedAt.UTC().Format(time.RFC3339)
			}
			e = Entity{Access: AccessOK, Title: text, Data: data}
		}
		for _, id := range asked[strings.ToLower(c.UUID)] {
			out[id] = e
		}
	}
	return out, nil
}

// readBoards: a whiteboard by its own rule, in one read.
func readBoards(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]Entity, error) {
	asked := askedAs(ids)
	titles, err := readBoardTitles(ctx, ids, who.UserUUID)
	if err != nil {
		return nil, err
	}
	out := map[string]Entity{}
	for id, title := range titles {
		for _, a := range asked[strings.ToLower(id)] {
			out[a] = Entity{Access: AccessOK, Title: title, Data: map[string]any{"title": title}}
		}
	}
	return out, nil
}

// readTeams: a team for its members and admins; gone once archived.
func readTeams(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]Entity, error) {
	asked := askedAs(ids)
	if who.UserUID == "" {
		return map[string]Entity{}, nil
	}
	teams, err := readTeamChips(ctx, ids, who.UserUID)
	if err != nil {
		return nil, err
	}
	out := map[string]Entity{}
	for _, tm := range teams {
		if tm.Member == 0 && tm.Admin == 0 {
			continue
		}
		e := Entity{Access: AccessGone}
		if !deleted(tm.DeletedAt) {
			e = Entity{Access: AccessOK, Title: tm.Name, Data: map[string]any{"name": tm.Name}}
		}
		for _, id := range asked[strings.ToLower(tm.UUID)] {
			out[id] = e
		}
	}
	return out, nil
}

// readComments: a comment as what it's on, each parent kind read once.
func readComments(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]Entity, error) {
	asked := askedAs(ids)
	comments, err := readCommentChips(ctx, ids)
	if err != nil {
		return nil, err
	}
	var tasks, docs, posts, chats []string
	for _, c := range comments {
		switch {
		case c.Task != nil && c.Task.UUID != "":
			tasks = append(tasks, c.Task.UUID)
		case c.Doc != nil && c.Doc.UUID != "":
			docs = append(docs, c.Doc.UUID)
		case c.Post != nil && c.Post.UUID != "":
			posts = append(posts, c.Post.UUID)
		case c.Chat != nil && c.Chat.UUID != "":
			chats = append(chats, c.Chat.UUID)
		}
	}
	parents := map[string]map[string]Entity{}
	if parents["task"], err = readIfAny(ctx, who, tasks, readTasks); err != nil {
		return nil, err
	}
	if parents["doc"], err = readIfAny(ctx, who, docs, readDocs); err != nil {
		return nil, err
	}
	if parents["message"], err = readIfAny(ctx, who, posts, readMessages); err != nil {
		return nil, err
	}
	if parents["chat"], err = readIfAny(ctx, who, chats, readChats); err != nil {
		return nil, err
	}
	out := map[string]Entity{}
	for _, c := range comments {
		kind, on := "", ""
		switch {
		case c.Task != nil && c.Task.UUID != "":
			kind, on = "task", c.Task.UUID
		case c.Doc != nil && c.Doc.UUID != "":
			kind, on = "doc", c.Doc.UUID
		case c.Post != nil && c.Post.UUID != "":
			kind, on = "message", c.Post.UUID
		case c.Chat != nil && c.Chat.UUID != "":
			kind, on = "chat", c.Chat.UUID
		}
		parent, ok := parents[kind][on]
		if !ok || parent.Access != AccessOK {
			continue
		}
		e := Entity{Access: AccessGone}
		if !deleted(c.DeletedAt) {
			text := snippet(c.Text)
			data := map[string]any{"snippet": text, "on_kind": kind, "on_id": strings.ToLower(on), "on_title": parent.Title}
			if p, ok := parent.Data["path"].(string); ok && kind == "chat" {
				// A reply in a direct message opens where its message does.
				data["on_path"] = p
			}
			e = Entity{Access: AccessOK, Title: text, Data: data}
		}
		for _, id := range asked[strings.ToLower(c.UUID)] {
			out[id] = e
		}
	}
	return out, nil
}

// readIfAny is read for ids, nothing at all for none.
func readIfAny(ctx context.Context, who accessBusiness.Subject, ids []string, read Reader) (map[string]Entity, error) {
	if len(ids) == 0 {
		return map[string]Entity{}, nil
	}
	return read(ctx, who, ids)
}

// readMemory: a memory item for its owner alone.
func readMemory(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]Entity, error) {
	asked := askedAs(ids)
	if who.UserUUID == "" {
		return map[string]Entity{}, nil
	}
	items, err := readMemoryChips(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := map[string]Entity{}
	for _, m := range items {
		if m.Owner == nil || !strings.EqualFold(m.Owner.UUID, who.UserUUID) {
			continue
		}
		text := helpers.OneLine(m.Content, snippetLen)
		for _, id := range asked[strings.ToLower(m.UUID)] {
			out[id] = Entity{Access: AccessOK, Title: text, Data: map[string]any{"snippet": text, "kind": m.Kind, "status": m.Status}}
		}
	}
	return out, nil
}
