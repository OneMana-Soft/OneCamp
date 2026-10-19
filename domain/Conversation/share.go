package domain

// What a shared history reads from the graph: how much a DM or group has to
// share since a time, and its shareable messages a page at a time, with their
// replies and files.
//
// A message is shareable when it is live and a person's own words: a forward
// is left out (what it forwarded belongs to another conversation, whose people
// may be fewer than the new set's), and so is a system line. A reply goes with
// its message; its audience is the conversation's, as its message's is.

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// The filters a shareable message and a live reply pass.
const (
	shareableChat = `not gt(chat_deleted_at, "1970-01-01T00:00:00Z") AND not has(chat_fwd_msg_post) AND not has(chat_fwd_msg_chat) AND not eq(chat_kind, "system")`
	liveComment   = `not gt(comment_deleted_at, "1970-01-01T00:00:00Z")`
)

// shareRepliesFirst bounds one message's replies a page reads: as many as a
// whole share copies at most (business/Conversation shareCap, messages and
// replies together). Without a first, Dgraph stops a sorted block at 1,000
// and says nothing (models/dgraph TestEverySortedBlockNamesItsFirst).
const shareRepliesFirst = "5000"

// ShareCount is how much there is to share: messages, and replies to them.
type ShareCount struct {
	Messages int `json:"messages"`
	Replies  int `json:"replies"`
}

// Total is messages and replies together, what a share's cap counts.
func (c ShareCount) Total() int { return c.Messages + c.Replies }

// ShareCounts is how much the DM or group groupingID has to share since each
// of since (the zero time for everything).
func ShareCounts(ctx context.Context, groupingID string, since []time.Time) ([]ShareCount, error) {
	if dgraphInit.DgraphClient == nil {
		return nil, errors.New("no graph connection")
	}
	vars := map[string]string{"$grp": groupingID}
	decl := "$grp: string"
	var counts, sums, rows string
	for i, s := range since {
		n := strconv.Itoa(i)
		filter := shareableChat
		if !s.IsZero() {
			vars["$s"+n] = s.UTC().Format(time.RFC3339Nano)
			decl += ", $s" + n + ": string"
			filter = `ge(chat_created_at, $s` + n + `) AND ` + filter
		}
		counts += ` m` + n + `: count(dm_chats @filter(` + filter + `))`
		rows += ` var(func: eq(dm_grouping_id, $grp)) { dm_chats @filter(` + filter + `) { r` + n + ` as count(chat_comments @filter(` + liveComment + `)) } }`
		sums += ` r` + n + `: sum(val(r` + n + `))`
	}
	q := `query Share(` + decl + `) { g(func: eq(dm_grouping_id, $grp)) {` + counts + ` }` + rows + ` s() {` + sums + ` } }`
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(ctx, q, vars)
	if err != nil {
		return nil, err
	}
	return parseShareCounts(resp.Json, len(since))
}

// parseShareCounts reads ShareCounts' answer. Pure.
func parseShareCounts(raw []byte, n int) ([]ShareCount, error) {
	var parsed struct {
		G []map[string]float64 `json:"g"`
		S []map[string]float64 `json:"s"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	out := make([]ShareCount, n)
	for i := range out {
		k := strconv.Itoa(i)
		for _, g := range parsed.G {
			out[i].Messages += int(g["m"+k])
		}
		for _, s := range parsed.S {
			out[i].Replies += int(s["r"+k])
		}
	}
	return out, nil
}

// ShareCutoff is the time from which sharing the DM or group's history since
// since keeps within limit messages and replies: the newest whole messages
// (each with its replies) that fit, the oldest left out. The zero time when
// everything since since fits.
func ShareCutoff(ctx context.Context, groupingID string, since time.Time, limit int) (time.Time, error) {
	if dgraphInit.DgraphClient == nil {
		return time.Time{}, errors.New("no graph connection")
	}
	vars := map[string]string{"$grp": groupingID, "$first": strconv.Itoa(limit + 1)}
	filter := shareableChat
	decl := "$grp: string, $first: int"
	if !since.IsZero() {
		vars["$since"] = since.UTC().Format(time.RFC3339Nano)
		decl += ", $since: string"
		filter = `ge(chat_created_at, $since) AND ` + filter
	}
	q := `query Cutoff(` + decl + `) { g(func: eq(dm_grouping_id, $grp)) {
		dm_chats @filter(` + filter + `) (orderdesc: chat_created_at, first: $first) {
			chat_created_at
			n: count(chat_comments @filter(` + liveComment + `))
		} } }`
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(ctx, q, vars)
	if err != nil {
		return time.Time{}, err
	}
	var parsed struct {
		G []struct {
			Chats []struct {
				At      time.Time `json:"chat_created_at"`
				Replies int       `json:"n"`
			} `json:"dm_chats"`
		} `json:"g"`
	}
	if err := json.Unmarshal(resp.Json, &parsed); err != nil {
		return time.Time{}, err
	}
	var newestFirst []CutoffMessage
	for _, g := range parsed.G {
		for _, c := range g.Chats {
			newestFirst = append(newestFirst, CutoffMessage{At: c.At, Replies: c.Replies})
		}
	}
	return Cutoff(newestFirst, limit), nil
}

// CutoffMessage is one message a cutoff weighs: when it was written, and its
// replies.
type CutoffMessage struct {
	At      time.Time
	Replies int
}

// Cutoff is the time of the oldest whole message (newest first, each with its
// replies) that keeps the total within limit; the zero time when all of them
// fit. Pure.
func Cutoff(newestFirst []CutoffMessage, limit int) time.Time {
	total := 0
	var last time.Time
	for _, m := range newestFirst {
		if total+1+m.Replies > limit {
			if last.IsZero() {
				// Not even the newest message fits whole: it goes alone, and
				// the share copies its first replies up to the limit.
				return m.At
			}
			return last
		}
		total += 1 + m.Replies
		last = m.At
	}
	return time.Time{}
}

// SharedMessage is one message a share copies, with its replies. By is nil
// for a message nobody can be named as the writer of (a node left half made):
// a share passes it, copying nothing.
type SharedMessage struct {
	UUID        string
	HTML        string
	At          time.Time
	By          *dgraphStruct.DgraphUser
	ReplyTo     string
	Attachments []*dgraphStruct.DgraphAttachment
	Replies     []SharedReply
}

// SharedReply is one reply a share copies.
type SharedReply struct {
	UUID        string
	HTML        string
	At          time.Time
	By          *dgraphStruct.DgraphUser
	Attachments []*dgraphStruct.DgraphAttachment
}

const attachmentFields = `{ attachment_uuid attachment_file_name attachment_obj_key attachment_width attachment_height attachment_size attachment_raw_type attachment_type attachment_duration }`
const authorFields = `{ uid user_uuid user_name user_full_name user_profile_object_key is_bot }`

// SharePage is up to size shareable messages of the DM or group groupingID
// written from since up to until, oldest first (by time, then uuid), after
// the cursor (at, afterUUID): the next page of a share. The zero at starts at
// since.
func SharePage(ctx context.Context, groupingID string, since, until time.Time, at time.Time, afterUUID string, size int) ([]SharedMessage, error) {
	if dgraphInit.DgraphClient == nil {
		return nil, errors.New("no graph connection")
	}
	from := since
	if at.After(from) {
		from = at
	}
	vars := map[string]string{
		"$grp":   groupingID,
		"$from":  from.UTC().Format(time.RFC3339Nano),
		"$until": until.UTC().Format(time.RFC3339Nano),
		"$first": strconv.Itoa(size),
		"$at":    at.UTC().Format(time.RFC3339Nano),
		"$after": afterUUID,
	}
	// The messages at the cursor's instant that are done are left out by the
	// query, so a page is never filled with them however many share it.
	after := ""
	if !at.IsZero() {
		after = ` AND NOT (eq(chat_created_at, $at) AND le(chat_uuid, $after))`
	}
	q := `query Page($grp: string, $from: string, $until: string, $first: int, $at: string, $after: string) {
		g(func: eq(dm_grouping_id, $grp)) {
			dm_chats @filter(ge(chat_created_at, $from) AND le(chat_created_at, $until)` + after + ` AND ` + shareableChat + `)
				(orderasc: chat_created_at, orderasc: chat_uuid, first: $first) {
				chat_uuid chat_body_text chat_created_at
				chat_from ` + authorFields + `
				chat_reply_to { chat_uuid }
				chat_attachments ` + attachmentFields + `
				chat_comments @filter(` + liveComment + `) (orderasc: comment_created_at, first: ` + shareRepliesFirst + `) {
					comment_uuid comment_text comment_created_at
					comment_by ` + authorFields + `
					comment_attachments ` + attachmentFields + `
				}
			}
		} }`
	resp, err := dgraphInit.DgraphClient.NewReadOnlyTxn().QueryWithVars(ctx, q, vars)
	if err != nil {
		return nil, err
	}
	return parseSharePage(resp.Json)
}

// parseSharePage reads SharePage's answer. Pure.
func parseSharePage(raw []byte) ([]SharedMessage, error) {
	var parsed struct {
		G []struct {
			Chats []struct {
				UUID     string                           `json:"chat_uuid"`
				Body     string                           `json:"chat_body_text"`
				At       time.Time                        `json:"chat_created_at"`
				By       *dgraphStruct.DgraphUser         `json:"chat_from"`
				ReplyTo  *dgraphStruct.DgraphChat         `json:"chat_reply_to"`
				Files    []*dgraphStruct.DgraphAttachment `json:"chat_attachments"`
				Comments []struct {
					UUID  string                           `json:"comment_uuid"`
					Text  string                           `json:"comment_text"`
					At    time.Time                        `json:"comment_created_at"`
					By    *dgraphStruct.DgraphUser         `json:"comment_by"`
					Files []*dgraphStruct.DgraphAttachment `json:"comment_attachments"`
				} `json:"chat_comments"`
			} `json:"dm_chats"`
		} `json:"g"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	var out []SharedMessage
	for _, g := range parsed.G {
		for _, c := range g.Chats {
			m := SharedMessage{UUID: c.UUID, HTML: c.Body, At: c.At, By: c.By, Attachments: c.Files}
			if c.By == nil || c.By.Uid == "" || c.By.Uuid == "" {
				// Nobody to attribute a copy to: kept in the page, so the
				// share's cursor passes it, with nothing to copy.
				m.By = nil
				out = append(out, m)
				continue
			}
			if c.ReplyTo != nil {
				m.ReplyTo = c.ReplyTo.Uuid
			}
			for _, r := range c.Comments {
				if r.By == nil || r.By.Uid == "" {
					continue
				}
				m.Replies = append(m.Replies, SharedReply{UUID: r.UUID, HTML: r.Text, At: r.At, By: r.By, Attachments: r.Files})
			}
			out = append(out, m)
		}
	}
	return out, nil
}
