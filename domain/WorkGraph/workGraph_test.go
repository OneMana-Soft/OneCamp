package workGraphDomain

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// schemaText is the graph schema as the server alters it at every start.
func schemaText(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "initializers", "dgraphInit", "connectDgraphDB.go"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// declaration is pred's line in the schema ("pred: [uid] @reverse ."), or "".
func declaration(schema, pred string) string {
	m := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(pred) + `\s*:\s*([^\n]*)\.\s*$`).FindStringSubmatch(schema)
	if m == nil {
		return ""
	}
	return m[1]
}

// TestEveryEdgeIsDeclared: each predicate the allow-list walks is a uid edge
// the schema declares, and each it walks backwards ("~pred") is declared
// @reverse: a reverse walk of an edge without one returns nothing, silently.
func TestEveryEdgeIsDeclared(t *testing.T) {
	schema := schemaText(t)
	for _, e := range Edges {
		pred := strings.TrimPrefix(e.Pred, "~")
		decl := declaration(schema, pred)
		switch {
		case decl == "":
			t.Errorf("%s %s walks %s, which the schema doesn't declare", e.From, e.Rel, pred)
		case !strings.Contains(decl, "uid"):
			t.Errorf("%s is not a uid edge: %q", pred, decl)
		case strings.HasPrefix(e.Pred, "~") && !e.Scan && !strings.Contains(decl, "@reverse"):
			t.Errorf("%s %s walks ~%s, but %s has no @reverse (%q)", e.From, e.Rel, pred, pred, decl)
		}
	}
}

// TestNoGateIsAConnection: membership edges decide who sees a thing and are
// never walked (#general's 200 members aren't connected to each other).
func TestNoGateIsAConnection(t *testing.T) {
	gates := map[string]bool{}
	for _, g := range Gates {
		gates[g] = true
	}
	for _, e := range Edges {
		if gates[strings.TrimPrefix(e.Pred, "~")] {
			t.Errorf("%s %s walks the membership edge %s", e.From, e.Rel, e.Pred)
		}
	}
}

// TestEveryEdgeHasItsWayBack: a path search may run from either end, so
// every edge walked one way (but a hub's scan and a comment's) has an edge
// in the allow-list for walking it back, which names the hop from there.
func TestEveryEdgeHasItsWayBack(t *testing.T) {
	for _, e := range Edges {
		if e.Opt || e.From == KindComment || e.From == KindMemory {
			continue
		}
		for _, to := range e.To {
			if _, ok := EdgeFrom(to, Inverse(e.Pred)); !ok {
				t.Errorf("%s -%s-> %s: no edge from %s along %s", e.From, e.Pred, to, to, Inverse(e.Pred))
			}
		}
	}
}

func TestInverse(t *testing.T) {
	for a, b := range map[string]string{
		"task_project": "project_tasks", "project_tasks": "task_project",
		"task_parent_task": "task_sub_tasks", "linked_docs": "~linked_docs", "~doc_parent": "doc_parent",
		"post_channel": "~post_channel", "project_team": "team_projects",
	} {
		if got := Inverse(a); got != b {
			t.Errorf("Inverse(%s) = %s, want %s", a, got, b)
		}
	}
}

var blockRe = regexp.MustCompile(`e_\d+: (~?[a-z_]+)([^{]*)\{`)

// TestNeighboursQueryHopsAreLiveAndCapped: every hop of a one- and a
// two-hop read, from every kind, keeps live nodes of its kinds only and
// reads its first N; the root is found live by its uuid, as a variable.
func TestNeighboursQueryHopsAreLiveAndCapped(t *testing.T) {
	all := Kinds()
	for _, k := range all {
		for _, depth := range []int{1, 2} {
			q, err := NeighboursQuery(k, depth, all)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(q, "eq("+specs[k].UUID+", $id)") {
				t.Errorf("%s: the root isn't read by its uuid variable:\n%s", k, q)
			}
			if specs[k].Deleted != "" && !strings.Contains(strings.SplitN(q, "\n", 3)[1], "not gt("+specs[k].Deleted) {
				t.Errorf("%s: the root isn't held live:\n%s", k, q)
			}
			for _, m := range blockRe.FindAllStringSubmatch(q, -1) {
				if !strings.Contains(m[2], "@filter((has(") || !strings.Contains(m[2], "(first: ") {
					t.Errorf("%s depth %d: the hop %s is not live and capped: %q", k, depth, m[1], m[2])
				}
			}
			if strings.Count(q, "{") != strings.Count(q, "}") {
				t.Errorf("%s depth %d: unbalanced braces:\n%s", k, depth, q)
			}
			if depth == 1 && strings.Count(q, "(first: "+itoa(Cap2)+")") > 0 && Cap2 != Cap1 {
				t.Errorf("%s: a one-hop read reads a second hop:\n%s", k, q)
			}
		}
	}
}

// TestNeighboursQueryHubs: a channel's and a person's posts are found by the
// posts that share a doc (one scan of has(post_doc)), never by walking a hub's
// list; a two-hop read goes on from no hub.
func TestNeighboursQueryHubs(t *testing.T) {
	q, _ := NeighboursQuery(KindChannel, 2, DefaultKinds)
	if strings.Contains(q, "ch_posts") || !strings.Contains(q, "has(post_doc)) @filter(uid_in(post_channel, uid(R))") {
		t.Errorf("a channel's read walks its posts:\n%s", q)
	}
	q, _ = NeighboursQuery(KindPerson, 2, DefaultKinds)
	if strings.Contains(q, "user_posts") || !strings.Contains(q, "uid_in(post_by, uid(R))") {
		t.Errorf("a person's read walks their posts:\n%s", q)
	}
	// From a doc, its creator is reached and not gone on from.
	q, _ = NeighboursQuery(KindDoc, 2, DefaultKinds)
	i := strings.Index(q, "doc_created_by @filter")
	if i < 0 {
		t.Fatalf("no creator hop:\n%s", q)
	}
	block := q[i:]
	block = block[:strings.Index(block, "}")]
	if strings.Contains(block, "e_") {
		t.Errorf("a two-hop read goes on from a person:\n%s", block)
	}
}

// TestNeighboursQueryKinds: a read for some kinds follows only edges that
// lead to them (or, two hops out, through a mid); comments and memory only
// when asked for by name.
func TestNeighboursQueryKinds(t *testing.T) {
	q, _ := NeighboursQuery(KindDoc, 1, []Kind{KindPerson})
	if strings.Contains(q, "linked_docs") || !strings.Contains(q, "doc_created_by") {
		t.Errorf("a read for people follows other edges:\n%s", q)
	}
	q, _ = NeighboursQuery(KindDoc, 1, DefaultKinds)
	if strings.Contains(q, "doc_comments") {
		t.Errorf("comments are read unasked:\n%s", q)
	}
	q, _ = NeighboursQuery(KindDoc, 1, append(DefaultKinds, KindComment))
	if !strings.Contains(q, "doc_comments") {
		t.Errorf("comments asked for aren't read:\n%s", q)
	}
	if _, err := NeighboursQuery(KindDoc, 3, DefaultKinds); err == nil {
		t.Error("a three-hop read is accepted")
	}
	if _, err := NeighboursQuery("folder", 1, DefaultKinds); err == nil {
		t.Error("an unknown kind is accepted")
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

const (
	docA  = "0b1f1c5a-0000-4000-8000-00000000000a"
	docB  = "0b1f1c5a-0000-4000-8000-00000000000b"
	task1 = "0b1f1c5a-0000-4000-8000-000000000001"
	proj1 = "0b1f1c5a-0000-4000-8000-0000000000f1"
	sam   = "0b1f1c5a-0000-4000-8000-0000000000c1"
	msg1  = "0b1f1c5a-0000-4000-8000-0000000000d1"
	chan1 = "0b1f1c5a-0000-4000-8000-0000000000e1"
)

func edgeIndex(t *testing.T, from Kind, pred string) string {
	t.Helper()
	for i, e := range Edges {
		if e.From == from && e.Pred == pred {
			return itoa(i)
		}
	}
	t.Fatalf("no edge %s %s", from, pred)
	return ""
}

// TestParseNeighbours: each node with the path that reached it, the root
// and loops back along a path left out, a single uid edge read as one node,
// a hub's scan as the root's hop, shortest paths first.
func TestParseNeighbours(t *testing.T) {
	linked := edgeIndex(t, KindDoc, "~linked_docs")
	inProject := edgeIndex(t, KindTask, "task_project")
	links := edgeIndex(t, KindTask, "linked_docs")
	creator := edgeIndex(t, KindDoc, "doc_created_by")
	raw := map[string]json.RawMessage{
		"root": json.RawMessage(`[{"uid":"0x1","e_` + linked + `":[{"uid":"0x2","task_uuid":"` + strings.ToUpper(task1) + `",
			"e_` + inProject + `":{"uid":"0x3","project_uuid":"` + proj1 + `"},
			"e_` + links + `":[{"uid":"0x1","doc_uuid":"` + docA + `"},{"uid":"0x4","doc_uuid":"` + docB + `"}]}],
			"e_` + creator + `":{"uid":"0x5","user_uuid":"` + sam + `"}}]`),
	}
	got, err := parseNeighbours(raw, Ref{KindDoc, docA})
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, r := range got {
		var hops []string
		for _, h := range r.Path {
			hops = append(hops, h.Rel+">"+string(h.To.Kind)+":"+h.To.UUID[len(h.To.UUID)-2:])
		}
		lines = append(lines, strings.Join(hops, " "))
	}
	want := []string{
		"linked_from>task:01",
		"created_by>person:c1",
		"linked_from>task:01 links_to>doc:0b",
		"linked_from>task:01 in_project>project:f1",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	if _, err := parseNeighbours(map[string]json.RawMessage{"root": json.RawMessage(`[]`)}, Ref{KindDoc, docA}); err != ErrNotFound {
		t.Errorf("an empty root is %v, want ErrNotFound", err)
	}
	// A channel's scan: its posts that share a doc, then the docs.
	scan := edgeIndex(t, KindChannel, "~post_channel")
	shares := edgeIndex(t, KindMessage, "post_doc")
	raw = map[string]json.RawMessage{
		"root":      json.RawMessage(`[{"uid":"0x9"}]`),
		"s_" + scan: json.RawMessage(`[{"uid":"0x7","post_uuid":"` + msg1 + `","e_` + shares + `":{"uid":"0x4","doc_uuid":"` + docB + `"}}]`),
	}
	got, err = parseNeighbours(raw, Ref{KindChannel, chan1})
	if err != nil || len(got) != 2 || got[0].Path[0].Rel != "message" || got[1].Node != (Ref{KindDoc, docB}) || got[1].Path[1].Rel != "shares" {
		t.Errorf("the scan read %+v, %v", got, err)
	}
}

// TestNeighboursTakesUUIDsOnly: anything but a uuid is not found, without a
// query (a graph uid from a client never reaches the graph).
func TestNeighboursTakesUUIDsOnly(t *testing.T) {
	calls := 0
	defer func(r func(context.Context, string, map[string]string) (map[string]json.RawMessage, error)) { read = r }(read)
	read = func(context.Context, string, map[string]string) (map[string]json.RawMessage, error) {
		calls++
		return nil, nil
	}
	for _, id := range []string{"0x1", "uid(R)", "", docA + `") { uid }`} {
		if _, err := Neighbours(context.Background(), Ref{KindDoc, id}, 1, DefaultKinds); err != ErrNotFound {
			t.Errorf("%q: %v", id, err)
		}
		if _, err := Paths(context.Background(), Ref{KindDoc, id}, Ref{KindDoc, docB}); err != ErrNotFound {
			t.Errorf("path from %q: %v", id, err)
		}
	}
	if calls != 0 {
		t.Errorf("%d queries ran", calls)
	}
}

// TestPathsQuery: one shortest block, at most three paths of four hops, every
// predicate live and capped, no hub scan and no comment or memory edge.
func TestPathsQuery(t *testing.T) {
	q, err := PathsQuery(KindDoc, KindProject)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(q, "shortest(") != 1 || !strings.Contains(q, "numpaths: 3, depth: 4") {
		t.Errorf("not one bounded shortest block:\n%s", q)
	}
	body := q[strings.Index(q, "shortest("):]
	body = body[strings.Index(body, "{")+1 : strings.Index(body, "}")]
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "@filter((has(") || !strings.HasSuffix(line, "(first: 25)") {
			t.Errorf("a hop isn't live and capped: %q", line)
		}
		for _, banned := range []string{"~post_channel", "~post_by", "comments", "mem_", "ch_posts", "_members"} {
			if strings.Contains(strings.Fields(line)[0], banned) {
				t.Errorf("a path walks %s: %q", banned, line)
			}
		}
	}
}

// TestParsePaths: paths named by the allow-list, a path through a node of
// no known kind left out, and a search run from the far end turned round.
func TestParsePaths(t *testing.T) {
	raw := map[string]json.RawMessage{
		"nodes": json.RawMessage(`[{"uid":"0x1","doc_uuid":"` + docA + `"},{"uid":"0x2","task_uuid":"` + task1 + `"},{"uid":"0x3","project_uuid":"` + proj1 + `"},{"uid":"0x8"}]`),
		"_path_": json.RawMessage(`[
			{"uid":"0x1","_weight_":2,"~linked_docs":{"uid":"0x2","task_project":{"uid":"0x3"}}},
			{"uid":"0x1","_weight_":2,"doc_mentions":{"uid":"0x8","linked_docs":{"uid":"0x3"}}}]`),
	}
	got, err := parsePaths(raw)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %+v, %v", got, err)
	}
	p := got[0]
	if p.From != (Ref{KindDoc, docA}) || len(p.Hops) != 2 || p.Hops[0].Rel != "linked_from" || p.Hops[1].Rel != "in_project" {
		t.Errorf("path %+v", p)
	}
	back, ok := reversePath(p)
	if !ok || back.From != (Ref{KindProject, proj1}) || back.Hops[0].Rel != "task" || back.Hops[1].Rel != "links_to" || back.Hops[1].To != (Ref{KindDoc, docA}) {
		t.Errorf("reversed %+v %v", back, ok)
	}
}

// TestPathsRunsFromTheEndItCanLeave: a search from a channel or a person
// runs from the other end, and comes back turned round.
func TestPathsRunsFromTheEndItCanLeave(t *testing.T) {
	defer func(r func(context.Context, string, map[string]string) (map[string]json.RawMessage, error)) { read = r }(read)
	var vars map[string]string
	read = func(_ context.Context, q string, v map[string]string) (map[string]json.RawMessage, error) {
		vars = v
		return map[string]json.RawMessage{
			"nodes":  json.RawMessage(`[{"uid":"0x1","doc_uuid":"` + docA + `"},{"uid":"0x7","post_uuid":"` + msg1 + `"},{"uid":"0x9","ch_uuid":"` + chan1 + `"}]`),
			"_path_": json.RawMessage(`[{"uid":"0x1","~post_doc":{"uid":"0x7","post_channel":{"uid":"0x9"}}}]`),
		}, nil
	}
	got, err := Paths(context.Background(), Ref{KindChannel, chan1}, Ref{KindDoc, docA})
	if err != nil || len(got) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	if vars["$a"] != docA || vars["$b"] != chan1 {
		t.Errorf("searched from %v", vars)
	}
	p := got[0]
	if p.From != (Ref{KindChannel, chan1}) || p.Hops[0].Rel != "message" || p.Hops[1].Rel != "shares" || p.Hops[1].To != (Ref{KindDoc, docA}) {
		t.Errorf("path %+v", p)
	}
	if got, _ := Paths(context.Background(), Ref{KindDoc, docA}, Ref{KindDoc, strings.ToUpper(docA)}); got != nil {
		t.Errorf("a path from a thing to itself: %+v", got)
	}
}
