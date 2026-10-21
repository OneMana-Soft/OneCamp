// Package business (Draft) is drafts kept on the server (chat P5; PROGRAM.md
// B24's drafts, D-M4): a message someone has started and not sent, one per
// person and place, so it survives a reload, a crash or a deploy, and waits on
// their other devices. A place is a channel, a DM or a group, or a thread in
// one, named by its draft key:
//
//	channel:<channel uuid>
//	dm:<uuid> <uuid>           (both people, as helpers.GetGroupingId sorts them)
//	group:<grouping id>
//	post-thread:<root uuid>    (a channel message's thread)
//	chat-thread:<root uuid>    (a DM's or group's message's thread)
//
// A thread is named by its root alone, as the server's thread kind is
// (business/Conversation KindThread); the two thread prefixes say which of the
// app's thread stores holds its draft.
//
// A draft is its writer's alone: only they read it, never through a guest's
// link or a connected workspace (the routes are members' own), and it is kept
// until they send it or empty the box (O17: never deleted on its own). Two
// devices writing one draft: the last write wins. A person keeps up to 500.
package business

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/MessageDraft"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/google/uuid"
)

const (
	// Max is the most drafts a person keeps.
	Max = 500
	// MaxHTML is the most characters a draft's text holds.
	MaxHTML = 100_000
	// MaxFiles is the most attachments a draft holds.
	MaxFiles = 20
	// WritesPerMinute is how often one person's drafts may be written or
	// emptied: the app saves a draft a second or so after typing stops.
	WritesPerMinute = 120
	// maxReplyText is how much of the message a draft answers it keeps.
	maxReplyText = 300
)

// ReplyTo is the message a draft answers inline (the reply pill).
type ReplyTo struct {
	UUID       string `json:"uuid"`
	AuthorName string `json:"author_name,omitempty"`
	Text       string `json:"text,omitempty"`
}

// File is an attachment already uploaded for the draft, as the composer holds
// it (the web app's AttachmentMediaReq).
type File struct {
	UUID      string   `json:"attachment_uuid"`
	FileName  string   `json:"attachment_file_name"`
	ObjKey    string   `json:"attachment_obj_key,omitempty"`
	Type      string   `json:"attachment_type"`
	Size      int64    `json:"attachment_size"`
	CreatedAt string   `json:"attachment_created_at,omitempty"`
	Width     *int     `json:"attachment_width,omitempty"`
	Height    *int     `json:"attachment_height,omitempty"`
	Duration  *float64 `json:"attachment_duration,omitempty"`
	RawType   string   `json:"attachment_raw_type,omitempty"`
}

// Draft is one draft as the app reads and writes it.
type Draft struct {
	Key       string    `json:"key"`
	HTML      string    `json:"html"`
	ReplyTo   *ReplyTo  `json:"reply_to,omitempty"`
	Files     []File    `json:"files,omitempty"`
	AlsoSend  bool      `json:"also_send,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Drafts is a person's drafts, newest first, and how many they may keep.
type Drafts struct {
	Drafts []Draft `json:"drafts"`
	Max    int     `json:"max"`
}

// Seams for tests.
var (
	resolve    = conversationBusiness.Resolve
	putDraft   = models.Put
	delDraft   = models.Delete
	listDrafts = models.List
	allowWrite = func(ctx context.Context, user uuid.UUID) bool {
		return redisStore.AllowFixedWindow(ctx, registry.DraftWriteRate, []string{user.String()}, WritesPerMinute).Allowed
	}
	now = time.Now
)

func refuse(status int, msg string) error {
	return &sendBusiness.Rejection{Status: status, Msg: msg}
}

var (
	errNotAPlace = refuse(http.StatusBadRequest, "That isn't a place a draft can be kept.")
	errTooFast   = refuse(http.StatusTooManyRequests, "Drafts are being saved faster than they can be kept. Yours is safe on this device and saves again in a minute.")
	groupID      = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

// ParseKey is the place a draft key names, as the Access Resolver names it:
// a channel, a DM or a group, or a thread (by its root). An error for a key
// that names nothing. Pure.
func ParseKey(key string) (conversationBusiness.Ref, error) {
	kind, id, ok := strings.Cut(key, ":")
	if !ok || id == "" || len(key) > 200 {
		return conversationBusiness.Ref{}, errNotAPlace
	}
	isUUID := func(s string) bool { _, err := uuid.Parse(s); return err == nil && len(s) == 36 }
	switch kind {
	case conversationBusiness.KindChannel:
		if isUUID(id) {
			return conversationBusiness.Ref{Kind: kind, ID: strings.ToLower(id)}, nil
		}
	case conversationBusiness.KindDM:
		a, b, two := strings.Cut(id, " ")
		if two && isUUID(a) && isUUID(b) && helpers.GetGroupingId(a, b) == id {
			return conversationBusiness.Ref{Kind: kind, ID: id}, nil
		}
	case conversationBusiness.KindGroup:
		if groupID.MatchString(id) {
			return conversationBusiness.Ref{Kind: kind, ID: id}, nil
		}
	case "post-thread", "chat-thread":
		if isUUID(id) {
			return conversationBusiness.Ref{Kind: conversationBusiness.KindThread, ID: strings.ToLower(id)}, nil
		}
	}
	return conversationBusiness.Ref{}, errNotAPlace
}

// blank is a draft with nothing in it: no words or picture, no files, no
// reply. Pure.
func blank(d Draft) bool {
	return strings.TrimSpace(helpers.HTMLToPlainText(d.HTML)) == "" && !strings.Contains(d.HTML, "<img") &&
		len(d.Files) == 0 && d.ReplyTo == nil
}

// tidy is d as it is kept, or a refusal in words: the reply's preview cut to
// size, a channel thread's "also send" kept for channel threads only, and every
// file named.
func tidy(key string, d Draft) (Draft, error) {
	if utf8.RuneCountInString(d.HTML) > MaxHTML {
		return d, refuse(http.StatusRequestEntityTooLarge, "That draft is too long to keep. Shorten it, or send it in parts.")
	}
	if len(d.Files) > MaxFiles {
		return d, refuse(http.StatusBadRequest, "A draft can hold up to 20 files.")
	}
	for _, f := range d.Files {
		if f.UUID == "" || f.FileName == "" || len(f.FileName) > 255 || len(f.ObjKey) > 512 || len(f.Type) > 32 {
			return d, refuse(http.StatusBadRequest, "One of that draft's files can't be kept. Attach it again.")
		}
	}
	if r := d.ReplyTo; r != nil {
		if _, err := uuid.Parse(r.UUID); err != nil {
			d.ReplyTo = nil
		} else {
			r.AuthorName = helpers.TruncateRunes(r.AuthorName, 200)
			r.Text = helpers.TruncateRunes(r.Text, maxReplyText)
		}
	}
	d.Key = key
	d.AlsoSend = d.AlsoSend && strings.HasPrefix(key, "post-thread:")
	return d, nil
}

func userOf(who *userModels.UserInfo) (uuid.UUID, bool) {
	if who == nil || who.UserPostgresInfo.Id == uuid.Nil {
		return uuid.Nil, false
	}
	return who.UserPostgresInfo.Id, true
}

// Put keeps d as who's draft at key, in place of the one there, and answers it
// as kept. A blank draft empties the place instead. Refused in words: a key
// that names no place, a place who can't read, a draft too big, the 501st
// draft, writes faster than WritesPerMinute.
func Put(ctx context.Context, who *userModels.UserInfo, key string, d Draft) (Draft, error) {
	user, ok := userOf(who)
	if !ok {
		return Draft{}, conversationBusiness.ErrNotFound
	}
	ref, err := ParseKey(key)
	if err != nil {
		return Draft{}, err
	}
	if blank(d) {
		return Draft{Key: key, UpdatedAt: now().UTC()}, Delete(ctx, who, key)
	}
	d, err = tidy(key, d)
	if err != nil {
		return Draft{}, err
	}
	if _, err := resolve(ctx, who, ref); errors.Is(err, conversationBusiness.ErrNotFound) {
		if ref.Kind == conversationBusiness.KindThread {
			return Draft{}, refuse(http.StatusNotFound, "That thread isn't there any more, so its draft can't be kept.")
		}
		return Draft{}, refuse(http.StatusNotFound, "That conversation isn't there any more, so its draft can't be kept.")
	} else if err != nil {
		return Draft{}, err
	}
	if !allowWrite(ctx, user) {
		return Draft{}, errTooFast
	}
	row := models.Draft{Key: key, HTML: d.HTML, AlsoSend: d.AlsoSend}
	if d.ReplyTo != nil {
		row.ReplyTo, _ = json.Marshal(d.ReplyTo)
	}
	if len(d.Files) > 0 {
		row.Files, _ = json.Marshal(d.Files)
	}
	at, err := putDraft(ctx, user, row, Max)
	if errors.Is(err, models.ErrFull) {
		return Draft{}, refuse(http.StatusConflict, "You have 500 drafts, the most that can be kept. Send or empty some first.")
	}
	if err != nil {
		return Draft{}, err
	}
	d.UpdatedAt = at.UTC()
	return d, nil
}

// Delete empties who's draft at key: a sent message's, or one they cleared.
// Their own to empty, wherever it was (a conversation they have since left
// too).
func Delete(ctx context.Context, who *userModels.UserInfo, key string) error {
	user, ok := userOf(who)
	if !ok {
		return conversationBusiness.ErrNotFound
	}
	if _, err := ParseKey(key); err != nil {
		return err
	}
	if !allowWrite(ctx, user) {
		return errTooFast
	}
	_, err := delDraft(ctx, user, key)
	return err
}

// List is who's drafts, newest first. Every one they keep, a place they can no
// longer open included: it is theirs until they empty it.
func List(ctx context.Context, who *userModels.UserInfo) (Drafts, error) {
	user, ok := userOf(who)
	if !ok {
		return Drafts{}, conversationBusiness.ErrNotFound
	}
	rows, err := listDrafts(ctx, user, Max)
	if err != nil {
		return Drafts{}, err
	}
	out := Drafts{Drafts: make([]Draft, 0, len(rows)), Max: Max}
	for _, r := range rows {
		d := Draft{Key: r.Key, HTML: r.HTML, AlsoSend: r.AlsoSend, UpdatedAt: r.UpdatedAt.UTC()}
		if len(r.ReplyTo) > 0 {
			var reply ReplyTo
			if json.Unmarshal(r.ReplyTo, &reply) == nil && reply.UUID != "" {
				d.ReplyTo = &reply
			}
		}
		if len(r.Files) > 0 {
			_ = json.Unmarshal(r.Files, &d.Files)
		}
		out.Drafts = append(out.Drafts, d)
	}
	return out, nil
}
