// Package business (ThreadIndex) keeps chat threads' index as replies are
// written (chat P2): a channel's, a DM's or a group's thread made an anchored
// threads row the first time it comes up (its id the root message's uuid),
// its latest reply, and who takes part. The reply paths call it
// (business/Comment for a channel's threads, business/Chat for a DM's or a
// group's), so it imports nothing above the models; what a person sees of
// their threads, and following by hand, is business/ChatThread.
package business

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	models "github.com/akashc777/OneCamp/models/postgres/Thread"
	"github.com/google/uuid"
)

// The conversations whose threads are indexed (the Access Resolver's names).
const (
	KindChannel = "channel"
	KindDM      = "dm"
	KindGroup   = "group"
)

// Kinds are the conversation kinds whose threads are indexed.
var Kinds = []string{KindChannel, KindDM, KindGroup}

// Thread names a chat thread: its conversation (Kind and ID: a channel's
// uuid, a DM's or group's grouping id) and its root message, which RootBy
// wrote at RootAt.
type Thread struct {
	Kind   string
	ID     string
	Root   string
	RootBy string
	RootAt time.Time
}

// OfPost is the thread under a channel's post, as far as post tells of it.
// Pure.
func OfPost(post *dgraphStruct.DgraphPost) Thread {
	if post == nil {
		return Thread{}
	}
	t := Thread{Kind: KindChannel, Root: post.Uuid, RootAt: helpers.Deref(post.CreatedAt)}
	if post.Channel != nil {
		t.ID = post.Channel.Uuid
	}
	if post.PostBy != nil {
		t.RootBy = post.PostBy.Uuid
	}
	return t
}

// OfChat is the thread under a DM's or group's message, in the conversation
// whose grouping id is groupingID, as far as chat tells of it. Pure.
func OfChat(groupingID string, chat *dgraphStruct.DgraphChat) Thread {
	if chat == nil {
		return Thread{}
	}
	t := Thread{Kind: KindOfChat(groupingID), ID: groupingID, Root: chat.Uuid, RootAt: helpers.Deref(chat.CreatedAt)}
	if chat.From != nil {
		t.RootBy = chat.From.Uuid
	}
	return t
}

// Reply is a reply just written in a thread: By wrote it at At, mentioning
// Mentioned (user uuids).
type Reply struct {
	Thread
	By        string
	Mentioned []string
	At        time.Time
}

// Seams for tests.
var (
	insertThread = models.Insert
	addPeople    = models.AddParticipants
	touchReply   = models.TouchReply
	lastReplyAt  = models.LastReplyAt
	followersOf  = models.Followers
	now          = func() time.Time { return time.Now().UTC() }
)

// Indexed is whether a conversation kind's threads are indexed.
func Indexed(kind string) bool {
	for _, k := range Kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// KindOfChat is the kind of conversation a DM's or group's grouping id names,
// as business/Conversation ChatRef has it. Pure.
func KindOfChat(groupingID string) string {
	if helpers.IsDMGroupingID(groupingID) {
		return KindDM
	}
	return KindGroup
}

// rowOf is the threads row t is: its id the root's, its resource the
// conversation, its anchor the root message. Made now when the root's time
// isn't known.
func rowOf(t Thread) (*models.Row, error) {
	if !Indexed(t.Kind) || t.ID == "" {
		return nil, fmt.Errorf("business/ThreadIndex: no thread in %q %q", t.Kind, t.ID)
	}
	id, err := uuid.Parse(t.Root)
	if err != nil {
		return nil, err
	}
	by, _ := uuid.Parse(t.RootBy)
	at := t.RootAt
	if at.IsZero() {
		at = now()
	}
	meta, _ := json.Marshal(map[string]string{"message": t.Root})
	return &models.Row{
		ID: id, ResourceKind: t.Kind, ResourceUUID: t.ID, AnchorKind: "message", AnchorMeta: meta,
		Status: "open", CreatedBy: by, CreatedAt: at, UpdatedAt: at,
	}, nil
}

// Adopt makes t a threads row if it isn't one, with its root's author as its
// author, who has read it up to the root (or not at all, when the root's time
// isn't known), and answers its id.
func Adopt(ctx context.Context, t Thread) (uuid.UUID, error) {
	row, err := rowOf(t)
	if err != nil {
		return uuid.Nil, err
	}
	if _, err := insertThread(ctx, row, true); err != nil {
		return uuid.Nil, err
	}
	if row.CreatedBy != uuid.Nil {
		var read *time.Time
		if !t.RootAt.IsZero() {
			read = &row.CreatedAt
		}
		if err := addPeople(ctx, []models.Participant{{ThreadID: row.ID, UserUUID: row.CreatedBy, Role: "author", LastReadAt: read}}); err != nil {
			return uuid.Nil, err
		}
	}
	return row.ID, nil
}

// AdoptAsRead makes t a threads row only when it isn't one: a thread from
// before replies were indexed, whose latest reply was at last, with its root's
// author and its repliers as its people, each having read it to then, so it
// joins their threads without a flood of unread. A thread that is a row
// already is left as it is, and nobody's unread is lost. made says whether it
// made it.
func AdoptAsRead(ctx context.Context, t Thread, last time.Time, repliers []string) (made bool, err error) {
	row, err := rowOf(t)
	if err != nil {
		return false, err
	}
	if last.IsZero() || last.Before(row.CreatedAt) {
		last = row.CreatedAt
	}
	if made, err = insertThread(ctx, row, true); err != nil || !made {
		return false, err
	}
	if _, err := touchReply(ctx, row.ID, last); err != nil {
		return true, err
	}
	people := []models.Participant{}
	seen := map[uuid.UUID]bool{}
	add := func(u uuid.UUID, role string) {
		if u == uuid.Nil || seen[u] {
			return
		}
		seen[u] = true
		read := last
		people = append(people, models.Participant{ThreadID: row.ID, UserUUID: u, Role: role, LastReadAt: &read})
	}
	add(row.CreatedBy, "author")
	for _, r := range repliers {
		if u, err := uuid.Parse(r); err == nil {
			add(u, "replier")
		}
	}
	if err := addPeople(ctx, people); err != nil {
		return true, err
	}
	return true, nil
}

// Replied records a reply: the thread made a row if it wasn't one, its latest
// reply, its writer as a replier who has read it to now, and those it
// mentions. Best effort beside the reply itself: a failure is logged and the
// reply stands.
func Replied(ctx context.Context, r Reply) {
	if err := replied(ctx, r); err != nil {
		helpers.LogErrorWithContext(ctx, "business/ThreadIndex Replied: thread %s in %s %s: %+v", r.Root, r.Kind, r.ID, err)
	}
}

// LatestReply is when the thread rooted at root last had a reply, as the index
// has it: zero for none, or for a root that isn't one.
func LatestReply(ctx context.Context, root string) (time.Time, error) {
	id, err := uuid.Parse(root)
	if err != nil {
		return time.Time{}, nil
	}
	return lastReplyAt(ctx, id)
}

func replied(ctx context.Context, r Reply) error {
	if !Indexed(r.Kind) || r.ID == "" {
		return nil
	}
	id, err := uuid.Parse(r.Root)
	if err != nil {
		return err
	}
	at := r.At
	if at.IsZero() {
		at = now()
	}
	// Most replies are in a thread that is a row already: one write. The
	// first makes it one, then is recorded.
	touched, err := touchReply(ctx, id, at)
	if err != nil {
		return err
	}
	if !touched {
		if _, err := Adopt(ctx, r.Thread); err != nil {
			return err
		}
		if _, err := touchReply(ctx, id, at); err != nil {
			return err
		}
	}
	// The writer and those the reply mentions, in one write.
	var people []models.Participant
	if by, err := uuid.Parse(r.By); err == nil {
		people = append(people, models.Participant{ThreadID: id, UserUUID: by, Role: "replier", LastReadAt: &at})
	}
	for _, m := range r.Mentioned {
		u, err := uuid.Parse(m)
		if err != nil || strings.EqualFold(m, r.By) {
			continue
		}
		people = append(people, models.Participant{ThreadID: id, UserUUID: u, Role: "mentioned"})
	}
	return addPeople(ctx, people)
}

// Following is, for the thread rooted at root, who follows it by their choice
// or part, and who turned it off: a reply's audience adds the first and
// leaves out the second (business/Notification ThreadAudience). Nil lists for
// a thread with no row yet, or a root that isn't a uuid.
func Following(ctx context.Context, root string) (on, off []string, err error) {
	id, err := uuid.Parse(root)
	if err != nil {
		return nil, nil, nil
	}
	onIDs, offIDs, err := followersOf(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	for _, u := range onIDs {
		on = append(on, u.String())
	}
	for _, u := range offIDs {
		off = append(off, u.String())
	}
	return on, off, nil
}
