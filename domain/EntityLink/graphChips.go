package entityLinkDomain

// Reads for the kinds the work graph (DG5) names beside docs, tasks,
// projects and people: channels, channel posts, teams, comments and memory
// items, each kind in one query however many are asked. As with the chips
// above, none decides who may see what: business/EntityLink does, before
// anything here reaches a reader.

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/models/dgexec"
	"github.com/akashc777/OneCamp/models/taskread"
)

// ChannelChip is what a channel's entry shows.
type ChannelChip struct {
	UUID      string     `json:"ch_uuid"`
	Name      string     `json:"ch_name"`
	Private   bool       `json:"ch_private"`
	DeletedAt *time.Time `json:"ch_deleted_at"`
}

// ChannelChips reads channels by uuid, deleted ones too (the reader says gone).
func ChannelChips(ctx context.Context, ids []string) ([]ChannelChip, error) {
	list := dgraphquery.UUIDList(ids)
	if list == "" {
		return nil, nil
	}
	out, err := dgexec.List[ChannelChip](ctx, `{ c(func: eq(ch_uuid, [`+list+`])) { ch_uuid ch_name ch_private ch_deleted_at } }`, nil, "c")
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/ChannelChips err: %+v", err)
	}
	return out, err
}

// PersonRef is a person as an entry names them.
type PersonRef struct {
	UUID     string `json:"user_uuid"`
	Name     string `json:"user_name"`
	FullName string `json:"user_full_name"`
}

// Display is the person's name as the app shows it.
func (p *PersonRef) Display() string {
	if p == nil {
		return ""
	}
	if p.FullName != "" {
		return p.FullName
	}
	return p.Name
}

// MessageChip is what a channel post's entry shows, with the channel that
// decides who reads it.
type MessageChip struct {
	UUID      string     `json:"post_uuid"`
	Text      string     `json:"post_text"`
	CreatedAt *time.Time `json:"post_created_at"`
	DeletedAt *time.Time `json:"post_deleted_at"`
	Channel   *struct {
		UUID      string     `json:"ch_uuid"`
		Name      string     `json:"ch_name"`
		DeletedAt *time.Time `json:"ch_deleted_at"`
	} `json:"post_channel"`
	By *PersonRef `json:"post_by"`
}

// MessageChips reads channel posts by uuid.
func MessageChips(ctx context.Context, ids []string) ([]MessageChip, error) {
	list := dgraphquery.UUIDList(ids)
	if list == "" {
		return nil, nil
	}
	out, err := dgexec.List[MessageChip](ctx, `{ m(func: eq(post_uuid, [`+list+`])) {
		post_uuid post_text post_created_at post_deleted_at
		post_channel { ch_uuid ch_name ch_deleted_at }
		post_by { user_uuid user_name user_full_name }
	} }`, nil, "m")
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/MessageChips err: %+v", err)
	}
	return out, err
}

// ChatChip is what a direct or group message's entry shows, with the
// conversation (its grouping id) that decides who reads it.
type ChatChip struct {
	UUID      string     `json:"chat_uuid"`
	Text      string     `json:"chat_body_text"`
	CreatedAt *time.Time `json:"chat_created_at"`
	DeletedAt *time.Time `json:"chat_deleted_at"`
	Dm        *struct {
		GroupingID string `json:"dm_grouping_id"`
	} `json:"chat_dm"`
	By *PersonRef `json:"chat_from"`
}

// ChatChips reads direct and group messages by uuid.
func ChatChips(ctx context.Context, ids []string) ([]ChatChip, error) {
	list := dgraphquery.UUIDList(ids)
	if list == "" {
		return nil, nil
	}
	out, err := dgexec.List[ChatChip](ctx, `{ c(func: eq(chat_uuid, [`+list+`])) {
		chat_uuid chat_body_text chat_created_at chat_deleted_at
		chat_dm { dm_grouping_id }
		chat_from { user_uuid user_name user_full_name }
	} }`, nil, "c")
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/ChatChips err: %+v", err)
	}
	return out, err
}

// TeamChip is what a team's entry shows, with whether one reader is in it.
type TeamChip struct {
	UUID      string     `json:"team_uuid"`
	Name      string     `json:"team_name"`
	DeletedAt *time.Time `json:"team_deleted_at"`
	Member    int        `json:"is_member"`
	Admin     int        `json:"is_admin"`
}

// TeamChips reads teams by uuid, counting whether the reader (by graph uid)
// is a member or an admin.
func TeamChips(ctx context.Context, ids []string, readerUID string) ([]TeamChip, error) {
	list := dgraphquery.UUIDList(ids)
	if list == "" {
		return nil, nil
	}
	out, err := dgexec.List[TeamChip](ctx, `query t($u: string) { t(func: eq(team_uuid, [`+list+`])) {
		team_uuid team_name team_deleted_at
		is_member: count(team_members @filter(uid($u)))
		is_admin: count(team_admins @filter(uid($u)))
	} }`, map[string]string{"$u": readerUIDOrNone(readerUID)}, "t")
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/TeamChips err: %+v", err)
	}
	return out, err
}

// readerUIDOrNone is a reader's graph uid for a uid() filter: one that
// names nobody when there is none (uid("") is a syntax error).
func readerUIDOrNone(uid string) string {
	if uid == "" {
		return "0x0"
	}
	return uid
}

// CommentChip is what a comment's entry shows, with what it's on: the thing
// that decides who reads it.
type CommentChip struct {
	UUID      string     `json:"comment_uuid"`
	Text      string     `json:"comment_text"`
	DeletedAt *time.Time `json:"comment_deleted_at"`
	Task      *struct {
		UUID string `json:"task_uuid"`
	} `json:"comment_task"`
	Doc *struct {
		UUID string `json:"doc_uuid"`
	} `json:"comment_doc"`
	Post *struct {
		UUID string `json:"post_uuid"`
	} `json:"comment_post"`
	Chat *struct {
		UUID string `json:"chat_uuid"`
	} `json:"comment_chat"`
}

// CommentChips reads comments by uuid. A comment on a restricted task the
// reader of ctx may not see isn't in it (models/taskread, as TaskChips leaves
// out the task): its text is the task's.
func CommentChips(ctx context.Context, ids []string) ([]CommentChip, error) {
	list := dgraphquery.UUIDList(ids)
	if list == "" {
		return nil, nil
	}
	tr, err := taskread.From(ctx)
	if err != nil {
		return nil, err
	}
	out, err := dgexec.List[CommentChip](ctx, `{ c(func: eq(comment_uuid, [`+list+`])) {
		comment_uuid comment_text comment_deleted_at
		comment_task { task_uuid } comment_doc { doc_uuid } comment_post { post_uuid } comment_chat { chat_uuid }
	} }`, nil, "c")
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/CommentChips err: %+v", err)
		return out, err
	}
	kept := out[:0]
	for _, c := range out {
		if c.Task != nil && tr.Hides(c.Task.UUID) {
			continue
		}
		kept = append(kept, c)
	}
	return kept, nil
}

// MemoryChip is what a memory item's entry shows, with its owner.
type MemoryChip struct {
	UUID    string `json:"mem_uuid"`
	Kind    string `json:"mem_kind"`
	Content string `json:"mem_content"`
	Status  string `json:"mem_status"`
	Owner   *struct {
		UUID string `json:"user_uuid"`
	} `json:"mem_owner"`
}

// MemoryChips reads memory items by uuid.
func MemoryChips(ctx context.Context, ids []string) ([]MemoryChip, error) {
	list := dgraphquery.UUIDList(ids)
	if list == "" {
		return nil, nil
	}
	out, err := dgexec.List[MemoryChip](ctx, `{ m(func: eq(mem_uuid, [`+list+`])) { mem_uuid mem_kind mem_content mem_status mem_owner { user_uuid } } }`, nil, "m")
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/MemoryChips err: %+v", err)
	}
	return out, err
}
