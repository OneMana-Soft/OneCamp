package domain

// The notes doc backfill (V-1209 B). recording_notes_doc exists since v2.77.0;
// a call's notes doc made before then is named only by the recap the call
// posted in its conversation. Query text here, run in models/dgexec.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/models/dgexec"
)

// RecapWindow is how long after a call ended the recap it posted is looked
// for in its conversation.
const RecapWindow = 15 * time.Minute

// UnlinkedCall is a call's transcript node filed under a channel or DM that
// names no notes doc, with the recaps posted in its conversation as the call
// ended: each such post's HTML. Ended is when the call ended.
type UnlinkedCall struct {
	Uid    string
	Ended  time.Time
	Recaps []string
}

// UnlinkedNotesCalls reads up to first such nodes after the uid after ("" for
// the first page), in uid order, with the recaps their conversation got in
// the RecapWindow after each ended: two reads per page, the calls and then
// every call's recaps in one request (a filter can't take a time from the
// node above it, so each call's window is its own block). A recap is a post
// or message whose author is bot, the AI bot's Dgraph uid as the server
// stored it: never a name or the text, which a member can copy (CR-429 F1,
// MAIN 18 Nov). Without a bot nothing is read.
//
// A call is a call session's node (no media file: a recording's node is the
// media, its session's node is the call) that has ended, is filed under a
// channel or a DM or group, and names no notes doc. A call kept on an event
// names its notes doc since its event does (03087a78b), so none is unlinked.
func UnlinkedNotesCalls(ctx context.Context, bot, after string, first int) ([]UnlinkedCall, error) {
	if bot == "" {
		return nil, errors.New("domain/UnlinkedNotesCalls: no AI bot to read recaps by")
	}
	if after == "" {
		after = "0x0"
	}
	page, err := dgexec.Read[unlinkedCallsRead](ctx, unlinkedCallsQuery,
		map[string]string{"$after": after, "$first": strconv.Itoa(first)})
	if err != nil || len(page.Calls) == 0 {
		return nil, err
	}
	q, vars := recapsQuery(bot, page.Calls)
	recaps, err := dgexec.Read[map[string][]recapsRead](ctx, q, vars)
	if err != nil {
		return nil, err
	}
	out := make([]UnlinkedCall, len(page.Calls))
	for i, c := range page.Calls {
		out[i] = UnlinkedCall{Uid: c.Uid, Ended: c.Ended}
		for _, home := range recaps["r"+strconv.Itoa(i)] {
			out[i].Recaps = append(out[i].Recaps, home.html()...)
		}
	}
	return out, nil
}

// unlinkedCallsQuery is UnlinkedNotesCalls' page of calls.
var unlinkedCallsQuery = `query UnlinkedNotesCalls($after: string, $first: int) {
	calls(func: has(recording_egress_id), first: $first, after: $after) @filter(type(Recording) AND NOT has(recording_obj_key)
		AND NOT has(recording_notes_doc) AND ` + dgraphquery.TimeSet("recording_ended_at") + `
		AND (has(recording_channel) OR has(recording_dm))) {
		uid
		ended: recording_ended_at
	}
}`

type unlinkedCallsRead struct {
	Calls []unlinkedCallRow `json:"calls"`
}

// unlinkedCallRow is a call as the page reads it.
type unlinkedCallRow struct {
	Uid   string    `json:"uid"`
	Ended time.Time `json:"ended"`
}

// recapsQuery reads, for each call (block r<i>), the posts its channel and
// the messages its DM or group got from the bot in the RecapWindow after it
// ended. The uids and times are query variables, never query text. Pure.
func recapsQuery(bot string, calls []unlinkedCallRow) (string, map[string]string) {
	decls := make([]string, 0, 1+3*len(calls))
	blocks := make([]string, 0, len(calls))
	vars := make(map[string]string, 1+3*len(calls))
	decls = append(decls, "$b: string")
	vars["$b"] = bot
	for i, c := range calls {
		n := strconv.Itoa(i)
		decls = append(decls, "$c"+n+": string", "$f"+n+": string", "$t"+n+": string")
		vars["$c"+n] = c.Uid
		vars["$f"+n], vars["$t"+n] = recapWindow(c.Ended)
		blocks = append(blocks, fmt.Sprintf(`r%[1]s(func: uid($c%[1]s)) {
		recording_channel { ch_posts @filter(ge(post_created_at, $f%[1]s) AND le(post_created_at, $t%[1]s) AND uid_in(post_by, $b)) { html: post_text } }
		recording_dm { dm_chats @filter(ge(chat_created_at, $f%[1]s) AND le(chat_created_at, $t%[1]s) AND uid_in(chat_from, $b)) { html: chat_body_text } }
	}`, n))
	}
	return "query NotesRecaps(" + strings.Join(decls, ", ") + ") {\n\t" + strings.Join(blocks, "\n\t") + "\n}", vars
}

// recapWindow is the RecapWindow after a call ended, as query values: the
// recaps are read from it, and the notes doc a recap names was made in it
// (business/AI makes the doc after the call ended, just before it posts the
// recap). Pure.
func recapWindow(ended time.Time) (from, to string) {
	return ended.UTC().Format(time.RFC3339), ended.Add(RecapWindow).UTC().Format(time.RFC3339)
}

type recapText struct {
	HTML string `json:"html"`
}

// recapsRead is one call's recaps: recording_channel and recording_dm are
// single uid edges, which Dgraph answers as one object.
type recapsRead struct {
	Channel *struct {
		Posts []recapText `json:"ch_posts"`
	} `json:"recording_channel"`
	DM *struct {
		Chats []recapText `json:"dm_chats"`
	} `json:"recording_dm"`
}

func (r recapsRead) html() []string {
	var out []string
	if r.Channel != nil {
		for _, p := range r.Channel.Posts {
			out = append(out, p.HTML)
		}
	}
	if r.DM != nil {
		for _, c := range r.DM.Chats {
			out = append(out, c.HTML)
		}
	}
	return out
}

// NotesLink is a call to link to the notes doc its recap named: the call's
// uid, when it ended, and the doc's uuid.
type NotesLink struct {
	Call  string
	Ended time.Time
	Doc   string
}

// LinkNotesDocs writes each call's notes doc, only where the node is a call
// that still names none, in one conditional upsert, and answers how many it
// wrote.
func LinkNotesDocs(ctx context.Context, links []NotesLink) (int, error) {
	if len(links) == 0 {
		return 0, nil
	}
	q, vars, muts, err := linkNotesDocsUpsert(links)
	if err != nil {
		return 0, err
	}
	resp, err := dgexec.Mutate(ctx, q, vars, muts...)
	if err != nil {
		return 0, err
	}
	return linkedOf(resp.GetJson(), len(muts))
}

// linkedOf is how many of the upsert's calls were written: those whose call
// (block l<i>) and doc (block k<i>) each counted one, as the mutation's
// condition asks. Pure.
func linkedOf(resp []byte, calls int) (int, error) {
	var out map[string][]struct {
		N int `json:"n"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		return 0, err
	}
	one := func(block string) bool { r := out[block]; return len(r) == 1 && r[0].N == 1 }
	n := 0
	for i := 0; i < calls; i++ {
		if one("l"+strconv.Itoa(i)) && one("k"+strconv.Itoa(i)) {
			n++
		}
	}
	return n, nil
}

// linkNotesDocsUpsert is LinkNotesDocs' upsert: per call a variable that is
// the node only when it is a call naming no notes doc, one that is the doc
// only when it is a private doc made in the call's recap window, as a call's
// notes doc is (so a doc a recap-shaped post names that is no notes doc of
// this call is never written: CR-429 F1), a mutation on both, and the counts
// that say what it wrote. Pure.
func linkNotesDocsUpsert(links []NotesLink) (string, map[string]string, []dgexec.Mutation, error) {
	links = slices.Clone(links)
	slices.SortFunc(links, func(a, b NotesLink) int { return strings.Compare(a.Call, b.Call) })
	decls := make([]string, len(links))
	lines := make([]string, len(links))
	vars := make(map[string]string, len(links))
	muts := make([]dgexec.Mutation, len(links))
	for i, link := range links {
		uid := link.Call
		doc, err := uuid.Parse(link.Doc)
		if err != nil {
			return "", nil, nil, fmt.Errorf("domain/LinkNotesDocs: a notes doc is a uuid: %w", err)
		}
		k := strconv.Itoa(i)
		n, d := "n"+k, "d"+k
		decls[i] = "$" + n + ": string, $" + d + ": string, $f" + k + ": string, $t" + k + ": string"
		vars["$"+n], vars["$"+d] = uid, doc.String()
		vars["$f"+k], vars["$t"+k] = recapWindow(link.Ended)
		lines[i] = n + ` as var(func: uid($` + n + `)) @filter(type(Recording) AND NOT has(recording_obj_key) AND NOT has(recording_notes_doc))
	` + d + ` as var(func: eq(doc_uuid, $` + d + `)) @filter(eq(doc_private, true) AND ge(doc_created_at, $f` + k + `) AND le(doc_created_at, $t` + k + `))
	l` + k + `(func: uid(` + n + `)) { n: count(uid) }
	k` + k + `(func: uid(` + d + `)) { n: count(uid) }`
		muts[i] = dgexec.Mutation{Cond: "@if(eq(len(" + n + "), 1) AND eq(len(" + d + "), 1))",
			Set: map[string]string{"uid": dgexec.Var(n), "recording_notes_doc": doc.String()}}
	}
	q := "query LinkNotesDocs(" + strings.Join(decls, ", ") + ") {\n\t" + strings.Join(lines, "\n\t") + "\n}"
	return q, vars, muts, nil
}

// RecapNotesDoc is the notes doc a call's recap post links to, "" for none.
// Pure.
func RecapNotesDoc(html string) string {
	m := recapNotesLink.FindStringSubmatch(html)
	if m == nil {
		return ""
	}
	id, err := uuid.Parse(m[1])
	if err != nil {
		return ""
	}
	return id.String()
}

// recapNotesLink is the notes link a recap writes (business/AI recapToHTML),
// by its words, so an ordinary doc link in a recap is never taken. The words
// prove nothing about who wrote them (a member can edit a post into them):
// only the bot's posts are read, and the write checks the doc (CR-429 F1).
var recapNotesLink = regexp.MustCompile(`<a\b[^>]*\bhref="/app/doc/([0-9a-fA-F-]{36})"[^>]*>\s*📄 Open the meeting notes\s*</a>`)
