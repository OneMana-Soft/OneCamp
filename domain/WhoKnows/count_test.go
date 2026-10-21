package whoKnowsDomain

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

const (
	d1 = "10000000-0000-0000-0000-000000000001"
	t1 = "20000000-0000-0000-0000-000000000001"
	p1 = "30000000-0000-0000-0000-000000000001"
)

// The people a read names are live and never a bot or an external account
// (bots are external by class, so both flags are checked).
func TestCountNamesNoBotsAndNoExternalAccounts(t *testing.T) {
	q := CountQuery(Items{Docs: []string{d1}})
	for _, want := range []string{"NOT eq(is_bot, true)", "NOT eq(is_external, true)", `not gt(user_deleted_at, "1970-01-01T00:00:00Z")`} {
		if !strings.Contains(q, "who(func:") || !strings.Contains(q[strings.Index(q, "who(func:"):], want) {
			t.Errorf("the who block doesn't filter with %q:\n%s", want, q)
		}
	}
}

// Counts are @groupby value variables, one per role, read with val(); every
// item kind is live; comments are capped per item.
func TestCountIsOneGroupedRead(t *testing.T) {
	q := CountQuery(Items{Docs: []string{d1}, Tasks: []string{t1}, Messages: []string{p1}})
	for _, want := range []string{
		"@groupby(doc_created_by)", "@groupby(task_created_by)", "@groupby(post_by)", "@groupby(task_assignee)", "@groupby(comment_by)",
		`not gt(doc_deleted_at, "1970-01-01T00:00:00Z")`, `not gt(task_deleted_at, "1970-01-01T00:00:00Z")`, `not gt(post_deleted_at, "1970-01-01T00:00:00Z")`,
		`not gt(comment_deleted_at, "1970-01-01T00:00:00Z")`, "(first: 50)", "val(cm)", "first: 1000",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("missing %q:\n%s", want, q)
		}
	}
	if strings.Count(q, "{\n") < 1 || strings.Contains(q, "query ") {
		t.Errorf("not one anonymous query block:\n%s", q)
	}
}

// Only uuids reach the query; nothing to read is no query at all.
func TestCountTakesUUIDsOnly(t *testing.T) {
	if q := CountQuery(Items{Docs: []string{"0x1", `"]) } delete {`}}); q != "" {
		t.Errorf("non-uuids made a query:\n%s", q)
	}
	called := false
	old := read
	read = func(context.Context, string) (map[string]json.RawMessage, error) { called = true; return nil, nil }
	t.Cleanup(func() { read = old })
	got, err := Count(context.Background(), Items{Tasks: []string{"nope"}})
	if err != nil || called || len(got.People) != 0 {
		t.Errorf("an empty read ran (%v) or answered %+v, %v", called, got, err)
	}
	if q := CountQuery(Items{Docs: []string{strings.ToUpper(d1)}}); !strings.Contains(q, `"`+d1+`"`) {
		t.Errorf("an upper-case uuid isn't written lower case:\n%s", q)
	}
}

// The answer: counts from val(), evidence only for people the who block named.
func TestCountReadsCountsAndEvidence(t *testing.T) {
	raw := map[string]json.RawMessage{
		"who": json.RawMessage(`[{"user_uuid":"A0000000-0000-0000-0000-00000000000A","user_email_id":"a@x","wD0":2,"wT0":4,"wP0":5,"aT":1},{"user_uuid":"b0000000-0000-0000-0000-00000000000b","cm":3}]`),
		"ev_doc": json.RawMessage(`[{"doc_uuid":"` + d1 + `","doc_created_by":{"user_uuid":"a0000000-0000-0000-0000-00000000000a"},
			"doc_comments":[{"comment_by":{"user_uuid":"b0000000-0000-0000-0000-00000000000b"}},{"comment_by":{"user_uuid":"c0000000-0000-0000-0000-0000000000b0"}}]}]`),
		"ev_task": json.RawMessage(`[{"task_uuid":"` + t1 + `","task_assignee":{"user_uuid":"a0000000-0000-0000-0000-00000000000a"}}]`),
	}
	got, err := parseCounted(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.People) != 2 || got.People[0].UUID != "a0000000-0000-0000-0000-00000000000a" || got.People[0].Wrote != 2 || got.People[0].Created != 4 || got.People[0].Posted != 5 || got.People[0].Assigned != 1 || got.People[1].Commented != 3 {
		t.Errorf("people %+v", got.People)
	}
	if len(got.Contributions) != 3 {
		t.Errorf("contributions %+v, want 3 (the unnamed bot's comment left out)", got.Contributions)
	}
	for _, c := range got.Contributions {
		if strings.HasPrefix(c.Person, "c") {
			t.Errorf("evidence for someone the who block didn't name: %+v", c)
		}
	}
}

// The seed query: values marshalled, only docs, tasks and posts, never chats.
func TestSeedQueryKeepsToReachAndNeverReadsChats(t *testing.T) {
	q := SeedQuery(Reach{UserUUID: d1, Channels: []string{p1}}, `budget" } }, { "match_all": {} `, 200)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(q), &parsed); err != nil {
		t.Fatalf("not JSON (the words broke out?): %v\n%s", err, q)
	}
	if strings.Contains(q, `"chats"`) || strings.Contains(q, "chat_") {
		t.Errorf("the seed query reads chats:\n%s", q)
	}
	for _, want := range []string{`"_index": "docs"`, `"_index": "tasks"`, `"_index": "posts"`, `"post_ch_id": ["` + p1 + `"]`, `"size": 200`, "deleted_date"} {
		if !strings.Contains(q, want) {
			t.Errorf("missing %q", want)
		}
	}
}
