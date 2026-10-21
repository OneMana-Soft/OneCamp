package domain

// A thread's faces on a page of messages (chat P2; found by DESIGN-BUILD-2, 18
// Oct). The pages of a channel's, a DM's and a group's messages read each
// message's replies only for the newest one's time, so the faces under a
// thread's reply bar came only from replies that arrived during the session
// (the web adds those with their authors): the demo's seeded threads said "2
// replies · Last reply …" with nobody beside it, and a check-in "2 answered"
// with no faces.
//
// Now every page query selects the same thing of each message's replies
// (RepliersSelection): its newest live replies' ids, times and authors, never
// their words. KeepRepliers turns those into up to three people, each once,
// in the order they last replied, so the last of them is the thread's latest
// reply, which the reply bar's "Last reply" reads. A reply the Guests or Slack
// bot relayed is its person's: two guests are two faces, and neither is the
// bot's. Their words are read apart, for those replies only, and what the page
// sends of them is the label naming the person (botpost.LabelledHTML), which
// the web's relay rule reads (lib/relayedAuthor), never the reply itself.

import (
	"context"
	"fmt"
	"html"
	"regexp"
	"strings"

	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

const (
	// RepliersRead is how many of a message's newest live replies a page reads
	// to find its thread's people.
	RepliersRead = 10
	// RepliersShown is how many people a thread's reply bar shows.
	RepliersShown = 3
)

// RepliersSelection is what a page of messages selects of each one's replies,
// edge being post_comments or chat_comments: the newest live ones' ids, times
// and authors (their emails say which bot relays; KeepRepliers drops them).
func RepliersSelection(edge string) string {
	return fmt.Sprintf(`%s (orderdesc: comment_created_at, first: %d) @filter(%s) {
							comment_uuid
							comment_created_at
							comment_by {
								user_uuid
								user_name
								user_full_name
								user_profile_object_key
								is_bot
								user_email_id
							}
						}`, edge, RepliersRead, dgraphquery.Live("comment_deleted_at"))
}

// Seams for tests.
var relayTexts = readRelayTexts

// KeepPostRepliers keeps each post's thread people (KeepRepliers).
func KeepPostRepliers(ctx context.Context, posts []*dgraphStruct.DgraphPost) {
	lists := make([]*[]*dgraphStruct.DgraphComment, 0, len(posts))
	for _, p := range posts {
		if p != nil {
			lists = append(lists, &p.Comments)
		}
	}
	KeepRepliers(ctx, lists...)
}

// KeepChatRepliers keeps each DM's or group's message's thread people (KeepRepliers).
func KeepChatRepliers(ctx context.Context, chats []*dgraphStruct.DgraphChat) {
	lists := make([]*[]*dgraphStruct.DgraphComment, 0, len(chats))
	for _, c := range chats {
		if c != nil {
			lists = append(lists, &c.Comments)
		}
	}
	KeepRepliers(ctx, lists...)
}

// KeepRepliers turns each list of a message's newest replies, newest first as
// RepliersSelection reads them, into its thread's people: up to RepliersShown,
// each once, oldest of their latest replies first. Each is their latest
// reply's id and time and its author's face (no email); a relayed one carries
// the label naming its person as its text. When a relayed reply's words can't
// be read, it counts as its bot's.
func KeepRepliers(ctx context.Context, lists ...*[]*dgraphStruct.DgraphComment) {
	var relayed []string
	for _, list := range lists {
		for _, c := range *list {
			if c != nil && c.CommentBy != nil && relays(c.CommentBy) {
				relayed = append(relayed, c.Uuid)
			}
		}
	}
	labels := map[string]string{}
	if len(relayed) > 0 {
		texts, err := relayTexts(ctx, relayed)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "domain/Message KeepRepliers: the relayed replies' labels: %+v", err)
		}
		for id, text := range texts {
			if label, ok := RelayLabel(text); ok {
				labels[id] = label
			}
		}
	}
	for _, list := range lists {
		*list = keepPeople(*list, labels)
	}
}

// keepPeople is one thread's people from its newest replies, newest first,
// with the labels of its relayed replies by id. Pure.
func keepPeople(newestFirst []*dgraphStruct.DgraphComment, labels map[string]string) []*dgraphStruct.DgraphComment {
	seen := map[string]bool{}
	var kept []*dgraphStruct.DgraphComment
	for _, c := range newestFirst {
		if c == nil || c.CommentBy == nil || len(kept) >= RepliersShown {
			continue
		}
		label := labels[c.Uuid]
		who := strings.ToLower(c.CommentBy.Uuid)
		if label != "" {
			who += ":" + strings.ToLower(relayName(label))
		}
		if who == "" || seen[who] {
			continue
		}
		seen[who] = true
		kept = append(kept, &dgraphStruct.DgraphComment{
			Uuid:      c.Uuid,
			CreatedAt: c.CreatedAt,
			Text:      label,
			CommentBy: &dgraphStruct.DgraphUser{
				Uuid:         c.CommentBy.Uuid,
				UserName:     c.CommentBy.UserName,
				UserFullName: c.CommentBy.UserFullName,
				ProfileKey:   c.CommentBy.ProfileKey,
				IsBot:        c.CommentBy.IsBot,
			},
		})
	}
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	return kept
}

// relays is whether by is the Guests or the Slack bot, which post others' words.
func relays(by *dgraphStruct.DgraphUser) bool {
	if !by.IsBot {
		return false
	}
	switch userDomain.ClassifyBot(by.EmailID) {
	case userDomain.BotKindGuest, userDomain.BotKindBridge:
		return true
	}
	return false
}

// The label leading a relayed message, as botpost.LabelledHTML writes it: a
// paragraph of its own, or inline before plain text. The label is HTML-escaped,
// so it holds no "<".
var (
	labelBlock  = regexp.MustCompile(`(?i)^\s*<p>\s*<strong>\[([^<]*)\]</strong>\s*</p>`)
	labelInline = regexp.MustCompile(`(?i)^\s*<p>\s*<strong>\[([^<]*)\]</strong>`)
)

// RelayLabel is the label paragraph leading a relayed message's HTML, as the
// web's relay rule reads it ("<p><strong>[Priya (Acme) (guest)]</strong></p>"),
// and whether it has one. Pure.
func RelayLabel(text string) (string, bool) {
	m := labelBlock.FindStringSubmatch(text)
	if m == nil {
		m = labelInline.FindStringSubmatch(text)
	}
	if m == nil {
		return "", false
	}
	return "<p><strong>[" + m[1] + "]</strong></p>", true
}

// relayName is the person a label names, to tell two relayed people apart. Pure.
func relayName(label string) string {
	m := labelBlock.FindStringSubmatch(label)
	if m == nil {
		return ""
	}
	return strings.Join(strings.Fields(html.UnescapeString(m[1])), " ")
}

// readRelayTexts reads the words of the replies ids name, by id.
func readRelayTexts(ctx context.Context, ids []string) (map[string]string, error) {
	list := dgraphquery.UUIDList(ids)
	if list == "" {
		return map[string]string{}, nil
	}
	var out struct {
		C []struct {
			ID   string `json:"id"`
			Text string `json:"text"`
		} `json:"c"`
	}
	q := fmt.Sprintf(`{ c(func: eq(comment_uuid, [%s]), first: %d) { id: comment_uuid text: comment_text } }`, list, len(ids))
	if err := dgraphInit.ReadInto(ctx, q, nil, &out); err != nil {
		return nil, err
	}
	texts := make(map[string]string, len(out.C))
	for _, c := range out.C {
		texts[c.ID] = c.Text
	}
	return texts, nil
}
