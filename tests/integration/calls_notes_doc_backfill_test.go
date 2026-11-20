//go:build integration
// +build integration

package integration_test

// The notes doc backfill (V-1209 B) against Dgraph: a call from before calls
// named their notes doc is found with the recaps posted in its conversation in
// the 15 minutes after it ended (a channel's posts, a DM's messages), and only
// those; a call that already names its notes doc, a call still going and a
// call with no conversation are not read; the walk goes a page at a time in
// uid order; and the write names a call's notes doc only where the call still
// names none.
//
// Run: go test -tags=integration ./tests/integration/ -run TestACallsNotesFromBeforeLinks -v

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	recordingBusiness "github.com/akashc777/OneCamp/business/Recording"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/tests/integration"
)

const (
	bfDocA   = "0a000000-0000-4000-8000-00000000000a"
	bfDocB   = "0b000000-0000-4000-8000-00000000000b"
	bfDocOld = "0b000000-0000-4000-8000-0000000000ff"
	bfDocD   = "0d000000-0000-4000-8000-00000000000d"
	bfDocF   = "0f000000-0000-4000-8000-00000000000f"
	bfDocX   = "0e000000-0000-4000-8000-0000000000e1"
	bfDocY   = "0e000000-0000-4000-8000-0000000000e2"
	bfDocM   = "0c000000-0000-4000-8000-0000000000c1"
)

// bfDoc is a doc node: private or not, made d after t0.
func bfDoc(id string, private bool, t0 time.Time, d time.Duration) map[string]any {
	return map[string]any{"dgraph.type": "Doc", "doc_uuid": id, "doc_title": "Notes", "doc_private": private,
		"doc_created_at": t0.Add(d).Format(time.RFC3339)}
}

// bfBotName is the AI bot's display name; a member can take it, so it
// decides nothing (MAIN 18 Nov).
const bfBotName = "OneCamp AI"

// bfNotesDoc is the notes doc the node uid names, "" for none. uid is read
// too, so a node that names none still answers.
func bfNotesDoc(t *testing.T, dg *integration.DgraphEnv, uid string) string {
	t.Helper()
	var out struct {
		Q []struct {
			Doc string `json:"recording_notes_doc"`
		} `json:"q"`
	}
	if err := json.Unmarshal(dg.Query(t, `{ q(func: uid(`+uid+`)) { uid recording_notes_doc } }`), &out); err != nil || len(out.Q) != 1 {
		t.Fatalf("read %s: %v %v", uid, err, out.Q)
	}
	return out.Q[0].Doc
}

// bfRecap is a recap as business/AI recapToHTML wrote it: the call's
// recording embed (data-egress) and its notes link.
func bfRecap(egress, doc string) string {
	return `<div data-ai-generated="true"><p>Recap</p><div data-egress="` + egress + `" data-kind="channel"></div>` +
		`<p><a href="/app/doc/` + doc + `">📄 Open the meeting notes</a></p></div>`
}

func TestACallsNotesFromBeforeLinksAreTiedByTheRecapItPosted(t *testing.T) {
	dg := integration.SetupDgraph(t)
	ctx := context.Background()
	ended := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return ended.Add(d).Format(time.RFC3339) }
	// F1 (CR-429, MAIN 18 Nov): only the AI bot's own posts are recaps. The
	// bot is its stored uid; a member who took its name is a member.
	post := func(d time.Duration, html string) map[string]any {
		return map[string]any{"dgraph.type": "Post", "post_text": html, "post_created_at": at(d), "post_by": map[string]any{"uid": "_:bot"}}
	}
	by := func(who string, m map[string]any) map[string]any {
		m["post_by"] = map[string]any{"uid": "_:" + who}
		return m
	}
	uids := dg.Mutate(t, []map[string]any{
		{"uid": "_:bot", "dgraph.type": "User", "user_uuid": "b0700000-0000-4000-8000-000000000001", "user_name": bfBotName, "user_full_name": bfBotName},
		{"uid": "_:mem", "dgraph.type": "User", "user_uuid": "b0700000-0000-4000-8000-000000000002", "user_name": bfBotName, "user_full_name": bfBotName},
		// The docs the recaps name: A and D are notes docs made in their
		// call's window; F is not private, Y was made before the call.
		bfDoc(bfDocA, true, ended, 2*time.Minute), bfDoc(bfDocD, true, ended, 4*time.Minute), bfDoc(bfDocB, true, ended, time.Minute),
		bfDoc(bfDocF, false, ended, 3*time.Minute), bfDoc(bfDocY, true, ended, -48*time.Hour), bfDoc(bfDocM, true, ended, -72*time.Hour),
		{"uid": "_:chan", "dgraph.type": "Channel", "ch_name": "launch", "ch_posts": []map[string]any{
			// A member's old post edited into the recap's link, and one by a
			// member who took the bot's name: in A's window, never read.
			by("mem", post(7*time.Minute, bfRecap("e-a", bfDocM))),
			post(-5*time.Minute, bfRecap("e-a", bfDocY)), // before A ended
			post(3*time.Minute, bfRecap("e-a", bfDocA)),  // A's recap
			post(4*time.Minute, bfRecap("e-f", bfDocF)),  // F's recap, in A's window too
			post(2*time.Minute, bfRecap("e-b", bfDocB)),  // B's recap; B names its doc already
			post(20*time.Minute, bfRecap("e-a", bfDocX)), // after A's 15 minutes
			post(6*time.Minute, `<p>Thanks all, see you Monday</p>`),
		}},
		{"uid": "_:dm", "dgraph.type": "Dm", "dm_chats": []map[string]any{
			{"dgraph.type": "Chat", "chat_body_text": bfRecap("e-d", bfDocD), "chat_created_at": at(5 * time.Minute), "chat_from": map[string]any{"uid": "_:bot"}},
			{"dgraph.type": "Chat", "chat_body_text": bfRecap("e-d", bfDocM), "chat_created_at": at(6 * time.Minute), "chat_from": map[string]any{"uid": "_:mem"}},
		}},
		{"uid": "_:a", "dgraph.type": "Recording", "recording_egress_id": "e-a", "recording_ended_at": at(0), "recording_channel": map[string]any{"uid": "_:chan"}},
		{"uid": "_:b", "dgraph.type": "Recording", "recording_egress_id": "e-b", "recording_ended_at": at(0), "recording_channel": map[string]any{"uid": "_:chan"},
			"recording_notes_doc": bfDocOld},
		{"uid": "_:d", "dgraph.type": "Recording", "recording_egress_id": "e-d", "recording_ended_at": at(0), "recording_dm": map[string]any{"uid": "_:dm"}},
		{"uid": "_:f", "dgraph.type": "Recording", "recording_egress_id": "e-f", "recording_ended_at": at(time.Minute), "recording_channel": map[string]any{"uid": "_:chan"}},
		// Still going (no end yet), and a call kept on no conversation.
		{"uid": "_:live", "dgraph.type": "Recording", "recording_egress_id": "e-live", "recording_ended_at": "0001-01-01T00:00:00Z", "recording_channel": map[string]any{"uid": "_:chan"}},
		{"uid": "_:lone", "dgraph.type": "Recording", "recording_egress_id": "e-lone", "recording_ended_at": at(0)},
		// A recording's node (its media file) is not its call's node.
		{"uid": "_:media", "dgraph.type": "Recording", "recording_egress_id": "EG_media", "recording_obj_key": "rec/a.mp4",
			"recording_ended_at": at(0), "recording_channel": map[string]any{"uid": "_:chan"}},
	})
	byUid := map[string]string{uids["a"]: "a", uids["b"]: "b", uids["d"]: "d", uids["f"]: "f", uids["live"]: "live", uids["lone"]: "lone", uids["media"]: "media"}
	notesDoc := func(name string) string { t.Helper(); return bfNotesDoc(t, dg, uids[name]) }

	t.Run("the calls with no notes doc and a conversation are read, with the recaps of their 15 minutes", func(t *testing.T) {
		page, err := recordingDomain.UnlinkedNotesCalls(ctx, uids["bot"], "", 100)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string][]string{}
		for _, c := range page {
			var docs []string
			for _, html := range c.Recaps {
				if d := recordingDomain.RecapNotesDoc(html); d != "" {
					docs = append(docs, d)
				}
			}
			sort.Strings(docs)
			got[byUid[c.Uid]] = docs
		}
		// A's and F's windows overlap: each holds the three recaps posted
		// in both (the business side keeps the one naming its call).
		three := strings.Join([]string{bfDocA, bfDocB, bfDocF}, ",")
		if len(got) != 3 || strings.Join(got["a"], ",") != three || strings.Join(got["f"], ",") != three ||
			strings.Join(got["d"], ",") != bfDocD {
			t.Fatalf("read %v", got)
		}
	})

	t.Run("it walks a page at a time in uid order", func(t *testing.T) {
		var seen []string
		after := ""
		for i := 0; i < 5; i++ {
			page, err := recordingDomain.UnlinkedNotesCalls(ctx, uids["bot"], after, 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(page) == 0 {
				break
			}
			if len(page) != 1 {
				t.Fatalf("a page of 1 held %d", len(page))
			}
			after = page[0].Uid
			seen = append(seen, byUid[after])
		}
		sort.Strings(seen)
		if strings.Join(seen, ",") != "a,d,f" {
			t.Fatalf("walked %v", seen)
		}
	})

	t.Run("the write names a notes doc only where the call names none", func(t *testing.T) {
		link := func(call, doc string, d time.Duration) recordingDomain.NotesLink {
			return recordingDomain.NotesLink{Call: uids[call], Ended: ended.Add(d), Doc: doc}
		}
		// F1: the bot's recap names a doc that fails the check (not private;
		// made before the call; no doc at all): nothing is written.
		for _, l := range []recordingDomain.NotesLink{link("f", bfDocF, time.Minute), link("f", bfDocY, time.Minute), link("f", bfDocX, time.Minute)} {
			if n, err := recordingDomain.LinkNotesDocs(ctx, []recordingDomain.NotesLink{l}); err != nil || n != 0 || notesDoc("f") != "" {
				t.Fatalf("a doc failing the check: linked %d, %v, f names %q", n, err, notesDoc("f"))
			}
		}
		n, err := recordingDomain.LinkNotesDocs(ctx, []recordingDomain.NotesLink{link("a", bfDocA, 0), link("b", bfDocB, 0)})
		if err != nil || n != 1 {
			t.Fatalf("linked %d, %v", n, err)
		}
		if got := notesDoc("a"); got != bfDocA {
			t.Fatalf("a names %q", got)
		}
		if got := notesDoc("b"); got != bfDocOld {
			t.Fatalf("b's notes doc was overwritten: %q", got)
		}
		// Written once: the same write again writes nothing.
		if n, err := recordingDomain.LinkNotesDocs(ctx, []recordingDomain.NotesLink{link("a", bfDocD, 0)}); err != nil || n != 0 || notesDoc("a") != bfDocA {
			t.Fatalf("again: %d %v, a names %q", n, err, notesDoc("a"))
		}
		page, err := recordingDomain.UnlinkedNotesCalls(ctx, uids["bot"], "", 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range page {
			if byUid[c.Uid] == "a" {
				t.Fatal("a linked call is still read")
			}
		}
	})

	t.Run("a node that is no call is never written", func(t *testing.T) {
		if n, err := recordingDomain.LinkNotesDocs(ctx, []recordingDomain.NotesLink{{Call: uids["chan"], Ended: ended, Doc: bfDocA}}); err != nil || n != 0 {
			t.Fatalf("linked %d, %v", n, err)
		}
	})
}

// The backfill as a server runs it after starting: each call whose 15 minutes
// hold recaps naming one notes doc is linked to it; a call whose window holds
// two calls' recaps, or none, is left as it is; a call that names its notes
// doc keeps it; and once it has run to the end, its system_configs record says
// so and a later run writes nothing, even for a call that has turned up since.
func TestTheNotesDocBackfillRunsOnceAndLinksFromTheRecap(t *testing.T) {
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(context.Background(), env.DSN); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	ctx := context.Background()
	ended := time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return ended.Add(d).Format(time.RFC3339) }
	// The recaps are the AI bot's, as the server resolves it.
	bot := userBusiness.GetAutomationBot(ctx)
	if bot == nil || bot.DgraphUID == "" {
		t.Fatal("no automation bot")
	}
	post := func(d time.Duration, html string) map[string]any {
		return map[string]any{"dgraph.type": "Post", "post_text": html, "post_created_at": at(d), "post_by": map[string]any{"uid": bot.DgraphUID}}
	}
	call := func(name, home string, d time.Duration, extra map[string]any) map[string]any {
		m := map[string]any{"uid": "_:" + name, "dgraph.type": "Recording", "recording_egress_id": "e-" + name, "recording_ended_at": at(d)}
		if home == "dm" {
			m["recording_dm"] = map[string]any{"uid": "_:dm"}
		} else {
			m["recording_channel"] = map[string]any{"uid": "_:" + home}
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	uids := dg.Mutate(t, []map[string]any{
		bfDoc(bfDocA, true, ended, 3*time.Minute), bfDoc(bfDocD, true, ended, time.Minute), bfDoc(bfDocM, true, ended, time.Minute),
		bfDoc(bfDocB, true, ended, 4*time.Minute), bfDoc(bfDocF, true, ended, 5*time.Minute),
		// MAIN 18 Nov: a member renamed to the bot's name posts a recap's
		// link to a notes doc made in m's window: it is not the bot's.
		{"uid": "_:mem", "dgraph.type": "User", "user_uuid": "b0700000-0000-4000-8000-000000000003", "user_name": bot.Name, "user_full_name": bot.Name},
		{"uid": "_:named", "dgraph.type": "Channel", "ch_name": "named", "ch_posts": []map[string]any{
			{"dgraph.type": "Post", "post_text": bfRecap("e-m", bfDocM), "post_created_at": at(2 * time.Minute), "post_by": map[string]any{"uid": "_:mem"}}}},
		{"uid": "_:calm", "dgraph.type": "Channel", "ch_name": "calm", "ch_posts": []map[string]any{
			post(4*time.Minute, bfRecap("e-p", bfDocA)), post(30*time.Minute, bfRecap("e-q", bfDocX))}},
		{"uid": "_:busy", "dgraph.type": "Channel", "ch_name": "busy", "ch_posts": []map[string]any{
			post(5*time.Minute, bfRecap("e-s1", bfDocB)), post(6*time.Minute, bfRecap("e-s2", bfDocF))}},
		{"uid": "_:dm", "dgraph.type": "Dm", "dm_chats": []map[string]any{
			{"dgraph.type": "Chat", "chat_body_text": bfRecap("e-r", bfDocD), "chat_created_at": at(2 * time.Minute), "chat_from": map[string]any{"uid": bot.DgraphUID}}}},
		call("p", "calm", 0, nil),
		call("q", "calm", 25*time.Minute, map[string]any{"recording_notes_doc": bfDocOld}),
		call("r", "dm", 0, nil),
		call("s1", "busy", 0, nil),
		call("s2", "busy", time.Minute, nil),
		// z ended two hours before calm's recaps: its window holds none.
		call("z", "calm", -2*time.Hour, nil),
		call("m", "named", 0, nil),
	})
	notesDoc := func(name string) string { t.Helper(); return bfNotesDoc(t, dg, uids[name]) }

	rep, ran, err := recordingBusiness.NotesDocBackfill.Run(ctx)
	if err != nil || !ran || rep != (recordingBusiness.NotesDocBackfillCount{Scanned: 6, Linked: 2, NoDoc: 2, ManyDocs: 2}) {
		t.Fatalf("run: %+v ran %v %v", rep, ran, err)
	}
	for name, want := range map[string]string{"p": bfDocA, "r": bfDocD, "q": bfDocOld, "s1": "", "s2": "", "z": "", "m": ""} {
		if got := notesDoc(name); got != want {
			t.Fatalf("%s names %q, want %q", name, got, want)
		}
	}
	var record string
	if err := env.PG.QueryRowContext(ctx, `SELECT value FROM system_configs WHERE key = 'recording_notes_doc_backfill_v1'`).Scan(&record); err != nil ||
		record != "done: scanned 6 calls, linked 2, skipped 2 naming no notes doc and 2 naming more than one" {
		t.Fatalf("record %q, %v", record, err)
	}

	// Done: a call that turned up since is not read, and nothing is written.
	late := dg.Mutate(t, []map[string]any{{"uid": "_:late", "dgraph.type": "Recording", "recording_egress_id": "e-late",
		"recording_ended_at": at(0), "recording_channel": map[string]any{"uid": uids["calm"]}}})
	if rep, ran, err := recordingBusiness.NotesDocBackfill.Run(ctx); err != nil || ran || rep.Scanned != 0 {
		t.Fatalf("second run: %+v ran %v %v", rep, ran, err)
	}
	uids["late"] = late["late"]
	if got := notesDoc("late"); got != "" {
		t.Fatalf("the second run wrote %q", got)
	}
}
