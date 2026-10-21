package workGraphDomain

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const taskA = "0b1f1c5a-0000-4000-8000-0000000000c1"

// TestExpandQueryIsOneLiveCappedHop: one block per kind of root, the roots
// held live, every hop live, capped and one deep, no hub scan, each target
// with its last change.
func TestExpandQueryIsOneLiveCappedHop(t *testing.T) {
	q, err := ExpandQuery([]Ref{{KindDoc, docA}, {KindTask, taskA}, {KindDoc, strings.ToUpper(docB)}, {KindDoc, docA}}, DefaultKinds, 4)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(q, "r_doc(") != 1 || strings.Count(q, "r_task(") != 1 {
		t.Errorf("one block per kind of root:\n%s", q)
	}
	if !strings.Contains(q, `eq(doc_uuid, ["`+docA+`", "`+docB+`"])`) {
		t.Errorf("the docs aren't read once each, by uuid:\n%s", q)
	}
	for _, line := range strings.Split(q, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "r_") && !strings.Contains(line, "not gt(") {
			t.Errorf("a root isn't held live: %s", line)
		}
	}
	hops := blockRe.FindAllStringSubmatch(q, -1)
	if len(hops) == 0 {
		t.Fatalf("no hops:\n%s", q)
	}
	for _, m := range hops {
		if !strings.Contains(m[2], "@filter((has(") || !strings.Contains(m[2], "(first: 4)") {
			t.Errorf("the hop %s is not live and capped: %q", m[1], m[2])
		}
	}
	if strings.Contains(q, "has(post_doc)") || strings.Contains(q, "~post_channel") {
		t.Errorf("a seed's hub is scanned:\n%s", q)
	}
	if strings.Count(q, "{") != strings.Count(q, "}") || strings.Count(q, "e_") != len(hops) {
		t.Errorf("not one hop deep, or unbalanced:\n%s", q)
	}
	if !strings.Contains(q, "task_updated_at") || !strings.Contains(q, "doc_updated_at") {
		t.Errorf("targets carry no last change:\n%s", q)
	}
}

// TestExpandTakesUUIDsOnly: a graph uid, query text or an unknown kind is
// never written into the query; too many roots are cut to ExpandMaxRoots.
func TestExpandTakesUUIDsOnly(t *testing.T) {
	for _, bad := range []Ref{{KindDoc, "0x1"}, {KindDoc, docA + `"]) { uid } q(func: has(doc_uuid`}, {Kind("folder"), docA}} {
		if q, err := ExpandQuery([]Ref{bad}, DefaultKinds, 4); err == nil {
			t.Errorf("%+v made a query:\n%s", bad, q)
		}
	}
	var many []Ref
	for i := 0; i < ExpandMaxRoots+5; i++ {
		many = append(many, Ref{KindDoc, fmt.Sprintf("0b1f1c5a-0000-4000-8000-%012x", i+1)})
	}
	q, err := ExpandQuery(many, DefaultKinds, 4)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(q, `"0b1f1c5a-`); n != ExpandMaxRoots {
		t.Errorf("%d roots read, want %d", n, ExpandMaxRoots)
	}
	if q, err := ExpandQuery(nil, DefaultKinds, 4); err != nil || q != "" {
		t.Errorf("no roots: %q %v", q, err)
	}
}

// TestParseExpand: each target with the relationship from its root's side and
// its last change, roots in the order asked, a root reaching itself left out.
func TestParseExpand(t *testing.T) {
	q, _ := ExpandQuery([]Ref{{KindDoc, docA}}, DefaultKinds, 4)
	edge := func(pred string) string {
		for _, m := range blockRe.FindAllStringSubmatch(q, -1) {
			if m[1] == pred {
				return strings.SplitN(m[0], ":", 2)[0]
			}
		}
		t.Fatalf("no %s edge in\n%s", pred, q)
		return ""
	}
	linked, mentions := edge("~linked_docs"), edge("doc_mentions")
	raw := map[string]json.RawMessage{
		"r_doc": json.RawMessage(`[{"doc_uuid":"` + docA + `",
			"` + linked + `":[{"task_uuid":"` + taskA + `","task_updated_at":"2026-10-12T09:00:00Z"}],
			"` + mentions + `":[{"doc_uuid":"` + docB + `"},{"doc_uuid":"` + docA + `"}]}]`),
	}
	got, err := parseExpand(raw, []Ref{{KindDoc, docA}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	if got[0].Rel != "linked_from" || got[0].Node != (Ref{KindTask, taskA}) || got[0].Changed.IsZero() || got[0].Root != (Ref{KindDoc, docA}) {
		t.Errorf("first %+v", got[0])
	}
	if got[1].Rel != "links_to" || got[1].Node != (Ref{KindDoc, docB}) || !got[1].Changed.IsZero() {
		t.Errorf("second %+v", got[1])
	}
}

// TestExpandIsOneRead: however many roots, one query; none for no roots.
func TestExpandIsOneRead(t *testing.T) {
	calls := 0
	defer func(r func(context.Context, string, map[string]string) (map[string]json.RawMessage, error)) { read = r }(read)
	read = func(context.Context, string, map[string]string) (map[string]json.RawMessage, error) {
		calls++
		return map[string]json.RawMessage{}, nil
	}
	if _, err := Expand(context.Background(), []Ref{{KindDoc, docA}, {KindTask, taskA}, {KindDoc, docB}}, DefaultKinds, 4); err != nil {
		t.Fatal(err)
	}
	if _, err := Expand(context.Background(), nil, DefaultKinds, 4); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("%d reads, want 1", calls)
	}
}
