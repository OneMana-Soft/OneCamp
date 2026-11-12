package business

import (
	"context"
	"time"

	"github.com/google/uuid"

	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	domain "github.com/akashc777/OneCamp/domain/Message"
	models "github.com/akashc777/OneCamp/models/postgres/Thread"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// Item is one of someone's threads, as the Threads view shows it.
type Item struct {
	Root         string                   `json:"root"`
	Conversation conversationBusiness.Ref `json:"conversation"`
	// Place is the conversation as the row names it.
	Place   domain.Place `json:"place"`
	Message Root         `json:"message"`
	// Replies is how many live replies it has; New, how many came after the
	// person last read it from someone else (NewMore when past the latest
	// replies read, "20+").
	Replies int  `json:"replies"`
	New     int  `json:"new"`
	NewMore bool `json:"new_more,omitempty"`
	// Latest is its newest replies, newest first.
	Latest      []domain.Reply `json:"latest"`
	LastReplyAt *time.Time     `json:"last_reply_at,omitempty"`
	Unread      bool           `json:"unread"`
	// Follow is the person's own choice for it: auto, on or off.
	Follow string `json:"follow"`
}

// Root is a thread's first message.
type Root struct {
	HTML      string         `json:"html"`
	By        *domain.Person `json:"by,omitempty"`
	CreatedAt *time.Time     `json:"created_at,omitempty"`
	Files     int            `json:"files,omitempty"`
}

// Page is a page of someone's threads, and how many of all they follow are
// unread.
type Page struct {
	Threads []Item `json:"threads"`
	// Next is where the next page starts, or 0 for none.
	Next   int `json:"next,omitempty"`
	Unread int `json:"unread"`
}

// shownLatest is how many of a thread's newest replies its row shows.
const shownLatest = 2

// Mine is a page of the threads who follows, unread first and each group
// newest reply first, from offset, and how many of them are unread. A thread
// whose conversation they can no longer read, or whose root is gone, is left
// out of both (shown): the count is the list's.
func Mine(ctx context.Context, who *userModels.UserInfo, offset int) (Page, error) {
	user, ok := userOf(who)
	if !ok {
		return Page{}, conversationBusiness.ErrNotFound
	}
	if offset < 0 || offset >= MaxThreads {
		offset = 0
	}
	rows, err := mineOf(ctx, user, Kinds, PageSize+1, offset)
	if err != nil {
		return Page{}, err
	}
	page := Page{Threads: []Item{}}
	if len(rows) > PageSize {
		rows = rows[:PageSize]
		if next := offset + PageSize; next < MaxThreads {
			page.Next = next
		}
	}
	unread, err := unreadOf(ctx, user, Kinds, MaxThreads)
	if err != nil {
		return Page{}, err
	}
	// The page's threads and the unread ones are asked about together: one
	// ask of who may read their conversations, one read of each kind's roots.
	kept, err := shown(ctx, who, append(append([]models.Mine{}, rows...), unread...))
	if err != nil {
		return Page{}, err
	}
	page.Unread = countKept(unread, kept)
	// A channel's roots and replies are posts' and their comments'; a DM's or
	// group's, messages' and theirs: one read of each kind, of the page's
	// threads that are kept.
	var posts, chats []string
	for _, r := range rows {
		if !kept[r.ThreadID] {
			continue
		}
		if r.Kind == conversationBusiness.KindChannel {
			posts = append(posts, r.ThreadID.String())
		} else {
			chats = append(chats, r.ThreadID.String())
		}
	}
	roots := map[string]domain.Message{}
	replies := map[string]domain.Replies{}
	if err := readThreads(ctx, true, posts, roots, replies); err != nil {
		return Page{}, err
	}
	if err := readThreads(ctx, false, chats, roots, replies); err != nil {
		return Page{}, err
	}
	me := user.String()
	for _, r := range rows {
		if !kept[r.ThreadID] {
			continue
		}
		root := r.ThreadID.String()
		m, ok := roots[root]
		if !ok || !live(m) {
			continue
		}
		rs := replies[root]
		item := Item{
			Root: root, Conversation: conversationBusiness.Ref{Kind: r.Kind, ID: r.Conversation}, Place: m.Place,
			Message: Root{HTML: m.HTML, By: m.By, CreatedAt: m.CreatedAt, Files: m.Files},
			Replies: rs.Count, LastReplyAt: r.LastReplyAt, Unread: r.Unread, Follow: r.Follow,
		}
		item.New, item.NewMore = newReplies(rs.Latest, r.LastReadAt, me)
		if len(rs.Latest) > shownLatest {
			item.Latest = rs.Latest[:shownLatest]
		} else {
			item.Latest = rs.Latest
		}
		if item.Latest == nil {
			item.Latest = []domain.Reply{}
		}
		page.Threads = append(page.Threads, item)
	}
	return page, nil
}

// unreadCountOf is how many threads user (who) follows are unread, of those
// the Threads list shows them (shown): at most MaxThreads, as many as the
// list can hold.
func unreadCountOf(ctx context.Context, who *userModels.UserInfo, user uuid.UUID) (int, error) {
	unread, err := unreadOf(ctx, user, Kinds, MaxThreads)
	if err != nil || len(unread) == 0 {
		return 0, err
	}
	kept, err := shown(ctx, who, unread)
	if err != nil {
		return 0, err
	}
	return countKept(unread, kept), nil
}

// countKept is how many of rows are kept. Pure.
func countKept(rows []models.Mine, kept map[uuid.UUID]bool) int {
	n := 0
	for _, r := range rows {
		if kept[r.ThreadID] {
			n++
		}
	}
	return n
}

// shown is the Threads list's one rule, which its count keeps too: of rows,
// the threads whose conversation who may still read and whose root is there
// and not deleted. It asks about every conversation once and which roots are
// live once, both kinds in one read of the graph (liveOf: however many, where
// a full read of roots takes 100). rows is left as given: readableRows keeps in place, so it is
// handed a copy. NOTIF-H4: the count counted every followed thread, so a
// thread in a conversation they left or lost, or whose root was deleted, kept
// the count up while the list left it out.
func shown(ctx context.Context, who *userModels.UserInfo, rows []models.Mine) (map[uuid.UUID]bool, error) {
	kept := map[uuid.UUID]bool{}
	rows, err := readableRows(ctx, who, append([]models.Mine(nil), rows...))
	if err != nil || len(rows) == 0 {
		return kept, err
	}
	var posts, chats []string
	seen := map[uuid.UUID]bool{}
	for _, r := range rows {
		if seen[r.ThreadID] {
			continue
		}
		seen[r.ThreadID] = true
		if r.Kind == conversationBusiness.KindChannel {
			posts = append(posts, r.ThreadID.String())
		} else {
			chats = append(chats, r.ThreadID.String())
		}
	}
	alive, err := liveOf(ctx, posts, chats)
	if err != nil {
		return nil, err
	}
	for id := range seen {
		if alive[id.String()] {
			kept[id] = true
		}
	}
	return kept, nil
}

// live is whether a thread's root is still there: not deleted. Pure.
func live(m domain.Message) bool {
	return m.DeletedAt == nil || m.DeletedAt.Year() <= 1970
}

// readThreads reads the roots and the latest replies of one kind's threads
// (ids): a channel's posts and their comments, or a DM's or group's messages
// and theirs, into roots and replies.
func readThreads(ctx context.Context, channel bool, ids []string, roots map[string]domain.Message, replies map[string]domain.Replies) error {
	if len(ids) == 0 {
		return nil
	}
	msgs, err := readRoots(ctx, channel, ids)
	if err != nil {
		return err
	}
	for _, m := range msgs {
		roots[m.ID] = m
	}
	got, err := readReplies(ctx, channel, ids, latestReplies)
	if err != nil {
		return err
	}
	for id, r := range got {
		replies[id] = r
	}
	return nil
}

// newReplies is how many of latest (newest first) came after read from
// someone other than me, and whether there may be more than latest holds.
// Pure.
func newReplies(latest []domain.Reply, read *time.Time, me string) (int, bool) {
	n := 0
	for _, r := range latest {
		if r.CreatedAt == nil || (read != nil && !r.CreatedAt.After(*read)) {
			break
		}
		if r.By != nil && r.By.UUID == me {
			continue
		}
		n++
	}
	return n, n > 0 && n == len(latest) && len(latest) >= latestReplies
}

// readableRows leaves out the threads whose conversation who can no longer
// read, asking about every conversation on the page at once.
func readableRows(ctx context.Context, who *userModels.UserInfo, rows []models.Mine) ([]models.Mine, error) {
	refs := make([]conversationBusiness.Ref, 0, len(rows))
	seen := make(map[conversationBusiness.Ref]bool, len(rows))
	for _, r := range rows {
		ref := conversationBusiness.Ref{Kind: r.Kind, ID: r.Conversation}
		if !seen[ref] {
			seen[ref] = true
			refs = append(refs, ref)
		}
	}
	can, err := resolveAll(ctx, who, refs)
	if err != nil {
		return nil, err
	}
	out := rows[:0]
	for _, r := range rows {
		if _, ok := can[conversationBusiness.Ref{Kind: r.Kind, ID: r.Conversation}]; ok {
			out = append(out, r)
		}
	}
	return out, nil
}
