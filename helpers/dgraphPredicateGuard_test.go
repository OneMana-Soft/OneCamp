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
	byFunc := queriesByFunc(queries)
	var hits []string
	for _, q := range queries {
		for _, name := range unknownPredicates(q.text, held, queryVariables(q, vars[q.file], byFunc)) {
			hits = append(hits, q.file+":"+strconv.Itoa(q.line)+": "+name)
		}
	}
	if len(hits) > 0 {
		t.Errorf("a query asks for a predicate the graph never holds, so it answers nothing:\n  %s", strings.Join(hits, "\n  "))
	}
}

// The guard itself: it finds a name nothing holds, and leaves aliases, block
// names, query variables, value variables, format verbs and the language's
// words alone.
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
		found_owner(func: uid(dm_p)) { uid }
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

// queriesByFunc is the queries each function builds, by its "dir.Name".
func queriesByFunc(queries []dgraphQuery) map[string][]dgraphQuery {
	byFunc := map[string][]dgraphQuery{}
	for _, q := range queries {
		if q.fn != "" {
			byFunc[q.fn] = append(byFunc[q.fn], q)
		}
	}
	return byFunc
}

// queryVariables is the variables a query may use: its file's, and those a
// fragment it calls defines (models/dgraph/Recording.MemberRecordingsBlock sets
// ch_recs and dm_recs in the query that calls it, and only there).
func queryVariables(q dgraphQuery, fileVars map[string]bool, byFunc map[string][]dgraphQuery) map[string]bool {
	if len(q.calls) == 0 {
		return fileVars
	}
	vars := make(map[string]bool, len(fileVars))
	for v := range fileVars {
		vars[v] = true
	}
	for _, c := range q.calls {
		for v := range valueVariables(byFunc[c]) {
			vars[v] = true
		}
	}
	return vars
}

// A fragment another package builds defines its variables in the query that
// calls it, and in no other: the guard learns them through the call, without
// letting a query that doesn't call it use them.
func TestAQueryKnowsTheVariablesOfTheFragmentsItCalls(t *testing.T) {
	dir := t.TempDir()
	src := "package p\n\nimport rec \"" + modulePath + "/domain/Recording\"\n\n" +
		"var q = `x(func: uid(ch_recs)) { uid }` + rec.Block(`uid($u)`) + local()\n"
	path := filepath.Join(dir, "p.go")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	found := dgraphQueriesIn(t, path)
	if len(found) != 1 || strings.Join(found[0].calls, ",") != "domain/Recording.Block,"+filepath.Dir(found[0].file)+".local" {
		t.Fatalf("read %+v, want the query's calls named through the file's imports and its own dir", found)
	}
	frag := dgraphQuery{file: "domain/Recording/r.go", fn: "domain/Recording.Block", text: "var(func: uid($u)) { ~ch_members { ch_recs as ch_recording } }"}
	byFunc := queriesByFunc([]dgraphQuery{frag})
	held := map[string]bool{"ch_recording": true, "ch_members": true}
	caller := dgraphQuery{file: "domain/User/u.go", text: "x(func: uid(ch_recs)) { uid }", calls: []string{"domain/Recording.Block"}}
	if got := unknownPredicates(caller.text, held, queryVariables(caller, nil, byFunc)); len(got) != 0 {
		t.Errorf("the caller of the fragment is flagged for %v", got)
	}
	other := dgraphQuery{file: "domain/User/u.go", text: "x(func: uid(ch_recs)) { uid }", calls: []string{"domain/Recording.Other"}}
	if got := unknownPredicates(other.text, held, queryVariables(other, nil, byFunc)); len(got) != 1 || got[0] != "ch_recs" {
		t.Errorf("a query that doesn't call the fragment got %v, want ch_recs flagged", got)
	}
}

// unknownPredicates is each predicate-like name in q that held lacks, sorted:
// never a query variable ($ch_id), an alias (ch_is_member:), a value variable
// (dm_p as …), the end of a format verb (%s_private) or the language's own
// words.
func unknownPredicates(q string, held, vars map[string]bool) []string {
	// The task filter's mark (models/taskread Mark) is filled before a query
	// is sent: it names no predicate.
	q = strings.ReplaceAll(q, "@@hidden_tasks@@", "")
	var out []string
	for _, at := range predicateLike.FindAllStringIndex(q, -1) {
		name := q[at[0]:at[1]]
		if at[0] > 0 && strings.ContainsAny(q[at[0]-1:at[0]], "$%._abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") {
			continue
		}
		rest := strings.TrimLeft(q[at[1]:], " \t")
		if strings.HasPrefix(rest, ":") || strings.HasPrefix(rest, "(func") || vars[name] || dqlWords[name] || held[name] {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
