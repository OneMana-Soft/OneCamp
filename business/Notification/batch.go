package notification

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/helpers"
	emailQueueModels "github.com/akashc777/OneCamp/models/postgres/NotificationEmailQueue"
	emailService "github.com/akashc777/OneCamp/services/Email"
)

// batchWindow is how long a batched notice waits for the ones after it, so
// a burst (a chat run, a thread exchange, a bulk move) is one email: as
// Slack's default cadence, at most one email every 15 minutes, and an
// offline person still hears within a quarter hour.
const batchWindow = 15 * time.Minute

// batchedKinds are the kinds whose emails wait the window and go out with
// the person's other notices in one email: talk and work. What is due at a
// time (calendar, calls), urgent (task.urgent, task.sla), asked for
// (check-in, digest) or an access grant (doc.shared) never waits.
var batchedKinds = []EventType{
	EventChatDM, EventChannelMention, EventGroupChatMention, EventDocMention, EventTableMention, EventTaskMention,
	EventPostComment, EventChatComment, EventDocComment, EventTableComment, EventTaskComment,
	EventDocProposal, EventTaskAssigned, EventTaskStatus, EventProjectUpdate,
}

// batched is whether an email of kind t is batched (batchedKinds).
func batched(t EventType) bool { return slices.Contains(batchedKinds, t) }

// matesMost is how many window-mates one claim takes at most.
const matesMost = 200

// claimMates is, for the worker's claim, a batched row's window-mates: the
// person's other batched rows due within the window.
var claimMates = emailQueueModels.Mates{Kinds: kindNames(batchedKinds), Window: batchWindow, Max: matesMost}

// kindNames is kinds as the queue's event_type says them. Pure.
func kindNames(kinds []EventType) []string {
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = string(k)
	}
	return out
}

// noticeLine is a batched notice's line in a list email, kept on its queue
// row at the dispatch: what happened, where, and the words it quotes.
type noticeLine struct {
	Title   string `json:"title"`
	Place   string `json:"place,omitempty"`
	Snippet string `json:"snippet,omitempty"`
}

// lineSnippet is how much of a notice's words its line quotes.
const lineSnippet = 160

// listShown is how many notices a list email lists; it says how many more.
const listShown = 20

// listKind is a list email's kind, as its log row and its tag say it.
const listKind = "notice.list"

// group is the rows one email stands for: a person's batched rows claimed
// together, newest first, or one row of any other kind.
type group []*emailQueueModels.QueueItem

// groupsOf groups items, in their order, each person's batched rows (by
// user and address) as one, of the rows tried as often: a list claimed again
// after its send (a close that didn't land, the reaper, a retry) is the same
// rows, so its key repeats and the provider sends it once. Pure.
func groupsOf(items []*emailQueueModels.QueueItem) []group {
	var out []group
	at := map[string]int{}
	for _, it := range items {
		if !batched(EventType(it.EventType)) {
			out = append(out, group{it})
			continue
		}
		k := fmt.Sprintf("%s|%s|%d", it.UserID, strings.ToLower(strings.TrimSpace(it.ToEmail)), it.Attempts)
		if i, ok := at[k]; ok {
			out[i] = append(out[i], it)
			continue
		}
		at[k] = len(out)
		out = append(out, group{it})
	}
	for _, g := range out {
		slices.SortStableFunc(g, func(a, b *emailQueueModels.QueueItem) int { return b.CreatedAt.Compare(a.CreatedAt) })
	}
	return out
}

// ids is g's rows' ids.
func (g group) ids() []uuid.UUID { return emailQueueModels.IDsOf(g) }

// attempts is the most tries any of g's rows has had.
func (g group) attempts() int {
	most := 0
	for _, it := range g {
		most = max(most, it.Attempts)
	}
	return most
}

// shell is what every email of a batch says alike, read once per batch:
// who it is from, and the logo a list email shows.
type shell struct {
	sender, logo string
}

// mail is the one email a group sends.
type mail struct {
	to, subject, html, text, key, unsubscribe, kind string
}

// mail is g's email: a single row's as its dispatch rendered it; for more,
// one list of them, newest first, in sh's shell.
func (g group) mail(sh shell) (mail, error) {
	newest := g[0]
	m := mail{to: newest.ToEmail, unsubscribe: buildUnsubscribeFromQueue(newest)}
	if len(g) == 1 {
		m.subject, m.html, m.text, m.key, m.kind = newest.Subject, newest.HTMLBody, newest.TextBody, newest.DedupKey, newest.EventType
		return m, nil
	}
	d := emailService.NoticeList{TemplateData: emailService.TemplateData{
		RecipientName:  newest.ToEmail,
		Title:          fmt.Sprintf("%d new notices", len(g)),
		CTAText:        "Open OneCamp",
		CTAURL:         AppURL("/app"),
		UnsubscribeURL: m.unsubscribe,
		BrandName:      "OneCamp",
		BrandLogo:      sh.logo,
	}}
	for _, it := range g[:min(len(g), listShown)] {
		d.Items = append(d.Items, lineOf(it))
	}
	d.More = len(g) - len(d.Items)
	html, text, err := emailService.RenderNoticeList(d)
	ids := helpers.UUIDStrings(g.ids())
	slices.Sort(ids)
	m.subject = fmt.Sprintf("%s, and %d more", newest.Subject, len(g)-1)
	m.html, m.text, m.kind = html, text, listKind
	m.key = listKind + "|" + helpers.HashString(strings.Join(ids, ","))
	return m, err
}

// options is how g's email m is sent, from sh's sender. A batched notice's
// email, one or a list, keeps the day's last NoticeReserve emails for what
// can't wait (a reminder, a call, an urgent task, a sign-in link).
func (g group) options(m mail, sh shell) emailService.SendOptions {
	keep := 0
	if batched(EventType(g[0].EventType)) {
		keep = emailService.NoticeReserve
	}
	return emailService.SendOptions{
		Keep:                keep,
		From:                sh.sender,
		To:                  m.to,
		Subject:             m.subject,
		HTML:                m.html,
		Text:                m.text,
		IdempotencyKey:      m.key,
		UnsubscribeURL:      m.unsubscribe,
		ListUnsubscribePost: true,
		Tags: map[string]string{
			"event_type": m.kind,
		},
	}
}

// lineOf is a row's line in a list email: as its dispatch wrote it, or its
// subject for a row queued before lines were; with the row's own link.
func lineOf(it *emailQueueModels.QueueItem) emailService.NoticeItem {
	var meta struct {
		Line noticeLine `json:"line"`
	}
	_ = json.Unmarshal(it.Metadata, &meta)
	item := emailService.NoticeItem{Title: meta.Line.Title, Place: meta.Line.Place, Snippet: meta.Line.Snippet}
	if item.Title == "" {
		item.Title = it.Subject
	}
	if it.CTAURL != nil {
		item.Link = *it.CTAURL
	}
	return item
}
