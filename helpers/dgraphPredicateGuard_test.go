package helpers

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Every predicate a graph query asks for is one the graph can hold: declared
// in the schema, or written through one of the graph's structs. A name that
// nothing holds answers nothing, and says so nowhere: fourteen channel queries
// asked for ch_handle, one for post_created_by (a post's author is post_by)
// and one for user_enail_id, a new message's upsert looked itself up by
// chat_id, and a reply's edit looked its mention node up by
// mention_comment_id, so the edit kept the people it dropped (F-23); a
// project's and a team's member lists asked for user_email. This
// reads every query in the source (dgraphQueries), as
// TestNoDgraphQueryRepeatsAFieldInOneBlock does.
func TestNoDgraphQueryAsksForAPredicateNothingHolds(t *testing.T) {
	held := predicatesTheGraphHolds(t)
	queries := dgraphQueries(t)
	byFile := map[string][]dgraphQuery{}
	for _, q := range queries {
		byFile[q.file] = append(byFile[q.file], q)
	}
	vars := map[string]map[string]bool{}
	for file, qs := range byFile {
		vars[file] = valueVariables(qs)
	}
	var hits []string
	for _, q := range queries {
		for _, name := range unknownPredicates(q.text, held, vars[q.file]) {
			hits = append(hits, q.file+":"+strconv.Itoa(q.line)+": "+name)
		}
	}
	if len(hits) > 0 {
		t.Errorf("a query asks for a predicate the graph never holds, so it answers nothing:\n  %s", strings.Join(hits, "\n  "))
	}
}

// The guard itself: it finds a name nothing holds, and leaves aliases, query
// variables, value variables, format verbs and the language's words alone.
func TestUnknownPredicatesFindsTheNamesNothingHolds(t *testing.T) {
	held := map[string]bool{"ch_uuid": true, "ch_members": true, "user_uuid": true, "dm_participants": true, "doc_private": true}
	q := dgraphQuery{text: `query Q($ch_id: string) {
		var(func: uid($ch_id)) { dm_p as dm_participants }
		channelInfo(func: eq(ch_uuid, $ch_id)) @filter(not uid(dm_p) AND uid_in(ch_members, $ch_id)) {
			ch_is_member: count(ch_members)
			private: %s_private
			ch_handle
			ch_members { user_uuid user_enail_id }
			first_seen: val(first_seen)
		}
	}`}
	got := unknownPredicates(q.text, held, valueVariables([]dgraphQuery{q, {text: "first_seen as min(val(s))"}}))
	if strings.Join(got, ",") != "ch_handle,user_enail_id" {
		t.Errorf("found %v, want ch_handle and user_enail_id", got)
	}
}

// predicatesTheGraphHolds is every name in the schema and every json name of
// the graph's structs (a struct can write a predicate the schema doesn't
// declare: F-24).
func predicatesTheGraphHolds(t *testing.T) map[string]bool {
	t.Helper()
	held := map[string]bool{}
	schema, err := os.ReadFile("../initializers/dgraphInit/connectDgraphDB.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`(?m)^\s*([a-z][a-z0-9_.]*)\s*:\s*\[?[a-zA-Z]+\]?`).FindAllSubmatch(schema, -1) {
		held[string(m[1])] = true
	}
	tag := regexp.MustCompile(`json:"([a-z][a-z0-9_.]*)`)
	_ = filepath.WalkDir("../models/dgraph", func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		src, _ := os.ReadFile(p)
		for _, m := range tag.FindAllSubmatch(src, -1) {
			held[string(m[1])] = true
		}
		return nil
	})
	if !held["ch_uuid"] || !held["post_by"] || len(held) < 200 {
		t.Fatalf("read %d names from the schema and the structs; their shape changed", len(held))
	}
	return held
}

var (
	predicateLike = regexp.MustCompile(`[a-z][a-z0-9]*(?:_[a-z0-9]+)+`)
	valueVar      = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s+as\s`)
	// The query language's own words with an underscore in them.
	dqlWords = map[string]bool{"uid_in": true}
)

// valueVariables is every value or uid variable the queries define ("s as
// task_start_date"): a file's queries are often built from pieces, and a
// variable one piece defines is used in another.
func valueVariables(queries []dgraphQuery) map[string]bool {
	vars := map[string]bool{}
	for _, q := range queries {
		for _, m := range valueVar.FindAllStringSubmatch(q.text, -1) {
			vars[m[1]] = true
		}
	}
	return vars
}

// unknownPredicates is each predicate-like name in q that held lacks, sorted:
// never a query variable ($ch_id), an alias (ch_is_member:), a value variable
// (dm_p as …), the end of a format verb (%s_private) or the language's own
// words.
func unknownPredicates(q string, held, vars map[string]bool) []string {
	var out []string
	for _, at := range predicateLike.FindAllStringIndex(q, -1) {
		name := q[at[0]:at[1]]
		if at[0] > 0 && strings.ContainsAny(q[at[0]-1:at[0]], "$%._abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") {
			continue
		}
		if strings.HasPrefix(strings.TrimLeft(q[at[1]:], " \t"), ":") || vars[name] || dqlWords[name] || held[name] {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
