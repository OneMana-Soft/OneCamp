package domain

import (
	"strings"
	"testing"
	"time"
)

// Each call's recaps are read from its own 15 minutes after it ended, and
// only the AI bot's own (F1, CR-429: the author's uid is the bot's stored
// uid, never a name or the text), by values passed as variables, never
// written into the query.
func TestEachCallsRecapsAreReadFromItsOwnFifteenMinutes(t *testing.T) {
	ended := time.Date(2026, 10, 20, 10, 0, 0, 0, time.FixedZone("IST", 5*3600+1800))
	q, vars := recapsQuery("0xb07", []unlinkedCallRow{{Uid: "0x1", Ended: ended}, {Uid: "0x2", Ended: ended.Add(time.Hour)}})
	want := map[string]string{
		"$b": "0xb07", "$c0": "0x1", "$f0": "2026-10-20T04:30:00Z", "$t0": "2026-10-20T04:45:00Z",
		"$c1": "0x2", "$f1": "2026-10-20T05:30:00Z", "$t1": "2026-10-20T05:45:00Z",
	}
	for k, v := range want {
		if vars[k] != v {
			t.Fatalf("%s = %q, want %q", k, vars[k], v)
		}
	}
	for _, frag := range []string{
		"r0(func: uid($c0))", "r1(func: uid($c1))",
		"ch_posts @filter(ge(post_created_at, $f0) AND le(post_created_at, $t0) AND uid_in(post_by, $b))",
		"dm_chats @filter(ge(chat_created_at, $f1) AND le(chat_created_at, $t1) AND uid_in(chat_from, $b))",
		"$c1: string, $f1: string, $t1: string", "($b: string, ",
	} {
		if !strings.Contains(q, frag) {
			t.Fatalf("the read lacks %q:\n%s", frag, q)
		}
	}
	if strings.Contains(q, "0x1") || strings.Contains(q, "0xb07") || strings.Contains(q, "2026") {
		t.Fatalf("a value was written into the query:\n%s", q)
	}
}

// The page reads only call sessions that ended, are filed under a
// conversation and name no notes doc.
func TestThePageReadsOnlyEndedUnlinkedCallsWithAConversation(t *testing.T) {
	for _, frag := range []string{"first: $first, after: $after", "type(Recording)", "NOT has(recording_obj_key)",
		"NOT has(recording_notes_doc)", `gt(recording_ended_at, "1970-01-01T00:00:00Z")`, "(has(recording_channel) OR has(recording_dm))"} {
		if !strings.Contains(unlinkedCallsQuery, frag) {
			t.Fatalf("the page read lacks %q", frag)
		}
	}
}

// The write names a notes doc only on a call that names none, and (F1,
// CR-429) only a doc that is a private doc made inside the call's own window,
// as a call's notes doc is: each on its own condition, in the one upsert, and
// it counts what it writes; a notes doc that is not a uuid is refused before
// anything is sent.
func TestTheWriteNamesANotesDocOnlyWhereACallNamesNone(t *testing.T) {
	ended := time.Date(2026, 10, 20, 10, 0, 0, 0, time.FixedZone("IST", 5*3600+1800))
	q, vars, muts, err := linkNotesDocsUpsert([]NotesLink{{Call: "0x2", Ended: ended.Add(time.Hour), Doc: "0B000000-0000-4000-8000-00000000000B"},
		{Call: "0x1", Ended: ended, Doc: "0a000000-0000-4000-8000-00000000000a"}})
	if err != nil {
		t.Fatal(err)
	}
	if vars["$n0"] != "0x1" || vars["$n1"] != "0x2" || len(muts) != 2 {
		t.Fatalf("vars %v, %d mutations", vars, len(muts))
	}
	if vars["$d1"] != "0b000000-0000-4000-8000-00000000000b" || vars["$f1"] != "2026-10-20T05:30:00Z" || vars["$t1"] != "2026-10-20T05:45:00Z" {
		t.Fatalf("the doc check's vars %v", vars)
	}
	for _, frag := range []string{
		"n0 as var(func: uid($n0)) @filter(type(Recording) AND NOT has(recording_obj_key) AND NOT has(recording_notes_doc))",
		"n1 as var(func: uid($n1)) @filter(type(Recording) AND NOT has(recording_obj_key) AND NOT has(recording_notes_doc))",
		"d1 as var(func: eq(doc_uuid, $d1)) @filter(eq(doc_private, true) AND ge(doc_created_at, $f1) AND le(doc_created_at, $t1))",
		"l1(func: uid(n1)) { n: count(uid) }", "k1(func: uid(d1)) { n: count(uid) }",
	} {
		if !strings.Contains(q, frag) {
			t.Fatalf("the write lacks %q:\n%s", frag, q)
		}
	}
	set, _ := muts[1].Set.(map[string]string)
	if muts[1].Cond != "@if(eq(len(n1), 1) AND eq(len(d1), 1))" || set["uid"] != "uid(n1)" || set["recording_notes_doc"] != "0b000000-0000-4000-8000-00000000000b" {
		t.Fatalf("mutation %+v", muts[1])
	}
	if strings.Contains(q, "0b000000") || strings.Contains(q, "2026") {
		t.Fatalf("a value was written into the write:\n%s", q)
	}
	if _, _, _, err := linkNotesDocsUpsert([]NotesLink{{Call: "0x1", Doc: `x" .`}}); err == nil {
		t.Fatal("a notes doc that is no uuid was taken")
	}
}

// The upsert counts a call as written only when its node and its doc each
// matched once, as the mutation's condition asks.
func TestTheWriteCountsOnlyCallsWhoseNodeAndDocBothMatched(t *testing.T) {
	resp := `{"l0": [{"n": 1}], "k0": [{"n": 1}], "l1": [{"n": 1}], "k1": [{"n": 0}], "l2": [{"n": 0}], "k2": [{"n": 1}], "l3": [{"n": 1}], "k3": [{"n": 2}]}`
	if n, err := linkedOf([]byte(resp), 5); err != nil || n != 1 {
		t.Fatalf("linked %d, %v; want 1", n, err)
	}
}
