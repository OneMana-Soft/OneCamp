package business

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	querySpec "github.com/akashc777/OneCamp/business/QuerySpec"
	searchQuery "github.com/akashc777/OneCamp/business/SearchQuery"
	liveness "github.com/akashc777/OneCamp/domain/Liveness"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	opensearchModels "github.com/akashc777/OneCamp/models/openSearch/GlobalSearch"
)

var sample = Reach{UserUUID: "U", Email: "u@example.test", Channels: []string{"C1", "C2"}, Projects: []string{"P1"}, Teams: []string{"T1"},
	Calendars: &EventCalendars{All: []string{"K1"}, Open: []string{"K2"}}}

// The access clause of every index, exactly as the search before the engine
// sent it (domain/GlobalSearch's query, deleted in C2-1): who may find what is
// the one thing the engine must not change. The one difference is said: the
// chats clause now says minimum_should_match 1, which the old one left to
// the default for a bool of only should clauses, the same 1. Events came after
// it (gcal-13): theirs is recorded as it was first sent.
var oldReach = map[string]string{
	"chats": `{"bool":{"should":[{"term":{"chat_to_user_id":"U"}},{"term":{"chat_by_user_id":"U"}},
		{"nested":{"path":"chat_participants","ignore_unmapped":true,"query":{"term":{"chat_participants.user_uuid":"U"}}}}],"minimum_should_match":1}}`,
	"posts": `{"terms":{"post_ch_id":["C1","C2"]}}`,
	"comments": `{"bool":{"should":[{"terms":{"comment_channel_id":["C1","C2"]}},{"term":{"comment_chat_to_user_id":"U"}},
		{"term":{"comment_chat_from_user_id":"U"}},
		{"nested":{"path":"comment_chat_participants","ignore_unmapped":true,"query":{"term":{"comment_chat_participants.user_uuid":"U"}}}},
		{"terms":{"comment_project_id":["P1"]}},{"term":{"comment_doc_created_by_user_id":"U"}},{"term":{"comment_doc_reading_users":"U"}},
		{"term":{"comment_doc_editing_users":"U"}},{"term":{"comment_doc_commenting_users":"U"}},{"term":{"comment_doc_private":false}},
		{"term":{"comment_board_created_by_user_id":"U"}},{"term":{"comment_board_reading_users":"U"}},{"term":{"comment_board_editing_users":"U"}},
		{"term":{"comment_board_commenting_users":"U"}},{"term":{"comment_board_public":true}}],"minimum_should_match":1}}`,
	"attachments": `{"bool":{"should":[{"terms":{"attachment_channel_id":["C1","C2"]}},{"term":{"attachment_chat_to_user_id":"U"}},
		{"term":{"attachment_chat_from_user_id":"U"}},
		{"nested":{"path":"attachment_chat_participants","ignore_unmapped":true,"query":{"term":{"attachment_chat_participants.user_uuid":"U"}}}},
		{"terms":{"attachment_project_id":["P1"]}},{"term":{"attachment_doc_created_by_user_id":"U"}},{"term":{"attachment_doc_reading_users":"U"}},
		{"term":{"attachment_doc_editing_users":"U"}},{"term":{"attachment_doc_commenting_users":"U"}},{"term":{"attachment_doc_private":false}}],"minimum_should_match":1}}`,
	"docs": `{"bool":{"should":[{"term":{"doc_created_by_user_id":"U"}},{"term":{"doc_reading_users":"U"}},{"term":{"doc_editing_users":"U"}},
		{"term":{"doc_commenting_users":"U"}},{"term":{"doc_owner_user_id":"U"}},{"term":{"doc_full_access_users":"U"}},
		{"terms":{"doc_team_uuids":["T1"]}},{"terms":{"doc_space_uuid":["P1","T1"]}},{"term":{"doc_private":false}}],"minimum_should_match":1}}`,
	"boards": `{"bool":{"should":[{"term":{"board_created_by_user_id":"U"}},{"term":{"board_reading_users":"U"}},{"term":{"board_editing_users":"U"}},
		{"term":{"board_commenting_users":"U"}},{"term":{"board_private":false}}],"minimum_should_match":1}}`,
	"tasks": `{"bool":{"should":[{"terms":{"task_project_id":["P1"]}},{"term":{"task_assignee_user_id":"U"}},
		{"bool":{"must":[{"term":{"task_created_by_user_id":"U"}}],"must_not":[{"exists":{"field":"task_project_id"}}]}}],"minimum_should_match":1}}`,
	"projects": `{"terms":{"project_id":["P1"]}}`,
	"channels": `{"terms":{"ch_id":["C1","C2"]}}`,
	"teams":    `{"terms":{"team_id":["T1"]}}`,
	"events": `{"bool":{"should":[{"term":{"event_people":"U"}},{"terms":{"event_calendar_id":["K1"]}},
		{"bool":{"filter":[{"terms":{"event_calendar_id":["K2"]}}],"must_not":[{"term":{"event_private":true}}]}}],"minimum_should_match":1}}`,
}

func canon(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Who may find what is unchanged, index by index; people are anyone's to
// find (the demo's visitor is held to its own after the index answers).
func TestEveryIndexKeepsItsAccessClause(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range sources {
		seen[s.index] = true
		if s.index == "users" {
			if s.reach != nil {
				t.Errorf("people were found by everyone; the users source now has a reach")
			}
			continue
		}
		want, ok := oldReach[s.index]
		if !ok {
			t.Errorf("%s has no recorded access clause", s.index)
			continue
		}
		var w any
		if err := json.Unmarshal([]byte(want), &w); err != nil {
			t.Fatalf("%s: %v", s.index, err)
		}
		if got := canon(t, s.reach(sample)); !reflect.DeepEqual(got, w) {
			g, _ := json.Marshal(got)
			t.Errorf("%s's access clause changed:\n  got  %s\n  want %s", s.index, g, want)
		}
	}
	for index := range oldReach {
		if !seen[index] {
			t.Errorf("%s is no longer searched", index)
		}
	}
	// An asker in nothing reaches nothing by membership: empty lists, never
	// a missing clause.
	if got := canon(t, sources[1].reach(Reach{UserUUID: "U"})); !reflect.DeepEqual(got, canon(t, terms("post_ch_id", nil))) {
		t.Errorf("no channels: %v", got)
	}
}

// compiled is each source's compiled spec, as JSON, "never" for a source the
// spec rules out and "always" for one it doesn't narrow.
func compiled(t *testing.T, filter *querySpec.Filter) map[string]string {
	t.Helper()
	spec := querySpec.Spec{Scope: querySpec.Scope{Kind: querySpec.ScopeSearch}, Filter: filter}
	c := newCompiler(context.Background(), sample, spec, time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC), time.UTC)
	out := map[string]string{}
	for _, s := range sources {
		p := c.compile(s, filter)
		switch {
		case p.never:
			out[s.index] = "never"
		case p.always:
			out[s.index] = "always"
		default:
			raw, _ := json.Marshal(p.q)
			out[s.index] = string(raw)
		}
	}
	return out
}

func cond(field, operator string, values ...querySpec.Value) *querySpec.Filter {
	return &querySpec.Filter{Field: field, Operator: operator, Values: values}
}

func allReady(t *testing.T) {
	saved := fieldReady
	t.Cleanup(func() { fieldReady = saved })
	fieldReady = func(string, string) bool { return true }
}

// A field an index doesn't have rules that index out; negated, it keeps it.
func TestAFilterAnIndexCantHoldRulesItOut(t *testing.T) {
	allReady(t)
	from := cond("from", querySpec.OperatorIn, querySpec.Value{ID: "sam", Type: "user"})
	got := compiled(t, from)
	for _, index := range []string{"channels", "teams", "projects", "users"} {
		if got[index] != "never" {
			t.Errorf("from:@sam on %s: %s, want never", index, got[index])
		}
	}
	for index, field := range map[string]string{"chats": "chat_by_user_id", "posts": "post_by_user_id.q", "comments": "comment_by_user_id.q",
		"attachments": "attachment_by_user_id.q", "docs": "doc_created_by_user_id", "boards": "board_created_by_user_id", "tasks": "task_created_by_user_id"} {
		if !strings.Contains(got[index], `"`+field+`":["sam"]`) || !strings.Contains(got[index], `"boost":0`) {
			t.Errorf("from:@sam on %s: %s, want a filter on %s that doesn't score", index, got[index], field)
		}
	}
	notFrom := compiled(t, &querySpec.Filter{Op: querySpec.OpNot, Filters: []querySpec.Filter{*from}})
	if notFrom["channels"] != "always" {
		t.Errorf("-from:@sam keeps channels: %s", notFrom["channels"])
	}
	types := compiled(t, cond("type", querySpec.OperatorIs, querySpec.Value{Text: "doc"}, querySpec.Value{Text: "task"}))
	for index, want := range map[string]string{"docs": "always", "tasks": "always", "posts": "never", "users": "never"} {
		if types[index] != want {
			t.Errorf("type:doc,task on %s: %s, want %s", index, types[index], want)
		}
	}
	in := compiled(t, cond("in", querySpec.OperatorIs, querySpec.Value{ID: "C1", Type: "channel"}))
	for index, want := range map[string]string{"posts": `{"term":{"post_ch_id":"C1"}}`, "chats": "never", "docs": "never"} {
		if want == "never" && in[index] != "never" || want != "never" && !strings.Contains(in[index], want) {
			t.Errorf("in:#launch on %s: %s, want %s", index, in[index], want)
		}
	}
	dm := compiled(t, cond("in", querySpec.OperatorIs, querySpec.Value{ID: "ana", Type: searchQuery.TypeDMWith}))
	if !strings.Contains(dm["chats"], `"chat_grp_id.keyword"`) || dm["posts"] != "never" {
		t.Errorf("in:@ana: chats %s, posts %s", dm["chats"], dm["posts"])
	}
}

// Every word must match somewhere, forgiving typos; common words don't count
// unless they're all there is; the word being typed also matches the start
// of a title; a phrase matches as written.
func TestWordsMatchAsPeopleMeanThem(t *testing.T) {
	allReady(t)
	word := func(w string, end int) querySpec.Filter {
		s := querySpec.Span{0, end}
		return querySpec.Filter{Field: "text", Operator: querySpec.OperatorContains, Values: []querySpec.Value{{Text: w}}, Span: &s}
	}
	and := &querySpec.Filter{Op: querySpec.OpAnd, Filters: []querySpec.Filter{word("terms", 5), word("and", 9), word("launch", 16)}}
	got := compiled(t, and)
	docs := got["docs"]
	if strings.Contains(docs, `"query":"and"`) {
		t.Errorf("a common word is required: %s", docs)
	}
	if !strings.Contains(docs, `"query":"terms"`) || !strings.Contains(docs, `"fuzziness":"AUTO"`) || !strings.Contains(docs, `"operator":"and"`) {
		t.Errorf("each word must match, all of it, forgiving typos: %s", docs)
	}
	if !strings.Contains(docs, `"match_phrase_prefix":{"doc_title":{"boost":10,"query":"launch"}}`) {
		t.Errorf("the word being typed matches the start of a title: %s", docs)
	}
	if strings.Contains(docs, `"match_phrase_prefix":{"doc_title":{"boost":10,"query":"terms"}}`) {
		t.Errorf("a finished word matched as a start: %s", docs)
	}
	onlyStop := compiled(t, &querySpec.Filter{Op: querySpec.OpAnd, Filters: []querySpec.Filter{word("the", 3), word("and", 7)}})
	if !strings.Contains(onlyStop["docs"], `"query":"the"`) {
		t.Errorf("a search of only common words still needs them: %s", onlyStop["docs"])
	}
	phrase := compiled(t, cond("text", querySpec.OperatorContains, querySpec.Value{Text: "pricing page"}))
	if !strings.Contains(phrase["docs"], `"type":"phrase"`) {
		t.Errorf("a phrase: %s", phrase["docs"])
	}
	not := compiled(t, cond("text", querySpec.OperatorNotContains, querySpec.Value{Text: "draft"}))
	if !strings.Contains(not["docs"], `"must_not"`) {
		t.Errorf("-draft: %s", not["docs"])
	}
	title := compiled(t, cond("title", querySpec.OperatorContains, querySpec.Value{Text: "onboarding"}))
	if title["chats"] != "never" || !strings.Contains(title["docs"], `"fields":["doc_title"]`) {
		t.Errorf("title:onboarding: chats %s, docs %s", title["chats"], title["docs"])
	}
}

// Dates are the days their value stands for, in the asker's zone.
func TestDatesAreTheDaysTheirValueMeans(t *testing.T) {
	allReady(t)
	day := querySpec.Value{Text: "2026-10-01"}
	from, to := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Unix(), time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC).Unix()
	for op, want := range map[string]map[string]int64{
		querySpec.OperatorIs:         {"gte": from, "lt": to},
		querySpec.OperatorBefore:     {"lt": from},
		querySpec.OperatorAfter:      {"gte": to},
		querySpec.OperatorOnOrBefore: {"lt": to},
		querySpec.OperatorOnOrAfter:  {"gte": from},
	} {
		got := compiled(t, cond("created", op, day))["posts"]
		var parsed struct {
			ConstantScore struct {
				Filter struct {
					Range map[string]map[string]int64 `json:"range"`
				} `json:"filter"`
			} `json:"constant_score"`
		}
		if err := json.Unmarshal([]byte(got), &parsed); err != nil || !reflect.DeepEqual(parsed.ConstantScore.Filter.Range["created_date"], want) {
			t.Errorf("created %s 2026-10-01: %s, want %v", op, got, want)
		}
	}
	if got := compiled(t, cond("updated", querySpec.OperatorIs, day)); got["attachments"] != "never" || !strings.Contains(got["docs"], "updated_date.q") {
		t.Errorf("updated: files never change, docs by their copy: %v", got)
	}
}

// What the index can't filter on yet is left out and said; where an index
// on this server didn't take a searchable copy, its entries are left out
// rather than shown unfiltered.
func TestWhatCantBeFilteredIsSaid(t *testing.T) {
	allReady(t)
	spec := querySpec.Spec{Scope: querySpec.Scope{Kind: querySpec.ScopeSearch}, Filter: cond("due", querySpec.OperatorBefore, querySpec.Value{Relative: "today"})}
	c := newCompiler(context.Background(), sample, spec, time.Now(), time.UTC)
	if p := c.compile(sources[6], spec.Filter); !p.always {
		t.Errorf("a field the index can't filter on: %+v", p)
	}
	if notes := c.unfilteredNotes(); len(notes) != 1 || !strings.Contains(notes[0].Message, "due:") {
		t.Errorf("notes: %+v", notes)
	}

	saved := fieldReady
	t.Cleanup(func() { fieldReady = saved })
	fieldReady = func(index, field string) bool { return !strings.HasSuffix(field, ".q") }
	from := cond("from", querySpec.OperatorIn, querySpec.Value{ID: "sam", Type: "user"})
	c = newCompiler(context.Background(), sample, querySpec.Spec{Filter: from}, time.Now(), time.UTC)
	if p := c.compile(sources[1], from); !p.never {
		t.Errorf("posts without their copy: %+v, want left out", p)
	}
	if p := c.compile(sources[0], from); p.never || p.always {
		t.Errorf("chats filter on their own field: %+v", p)
	}
	if notes := c.unfilteredNotes(); len(notes) != 1 || notes[0].Code != "partial" {
		t.Errorf("notes: %+v", notes)
	}
}

// The request: every kind counted, the kind shown as a post_filter, a page
// from its offset, and the order asked for.
func TestTheSearchAsksForCountsAndAPage(t *testing.T) {
	allReady(t)
	var body map[string]any
	var asked []string
	saved := searchIndex
	t.Cleanup(func() { searchIndex = saved })
	searchIndex = func(_ context.Context, indices []string, raw []byte) (opensearchModels.SearchAnswer, error) {
		asked = indices
		_ = json.Unmarshal(raw, &body)
		return opensearchModels.SearchAnswer{ByIndex: map[string]int{"posts": 4, "comments": 2, "docs": 3}, Total: 9}, nil
	}
	spec := querySpec.Spec{Scope: querySpec.Scope{Kind: querySpec.ScopeSearch}, Filter: cond("from", querySpec.OperatorIn, querySpec.Value{ID: "sam", Type: "user"})}
	r, err := Search(context.Background(), sample, spec, Page{Size: 10, After: encodeCursor(30, cursorBinding(sample, spec, Page{Kinds: []string{KindDoc}, Sort: SortNewest})), Kinds: []string{KindDoc}, Sort: SortNewest})
	if err != nil {
		t.Fatal(err)
	}
	if r.Totals[KindMessage] != 6 || r.Totals[KindDoc] != 3 {
		t.Errorf("totals %v, want messages 6 and docs 3", r.Totals)
	}
	if body["from"] != 30.0 || body["size"] != 10.0 || body["track_total_hits"] != true {
		t.Errorf("paging: from %v size %v", body["from"], body["size"])
	}
	if pf, _ := json.Marshal(body["post_filter"]); string(pf) != `{"terms":{"_index":["docs"]}}` {
		t.Errorf("post_filter %s", pf)
	}
	if sorts, _ := json.Marshal(body["sort"]); !strings.HasPrefix(string(sorts), `[{"created_date":{"order":"desc"`) {
		t.Errorf("newest first: %s", sorts)
	}
	for _, index := range asked {
		if index == "channels" || index == "users" {
			t.Errorf("asked %s, which from: rules out", index)
		}
	}
	// Nothing asked, nothing searched.
	if r, err := Search(context.Background(), sample, querySpec.Spec{Scope: querySpec.Scope{Kind: querySpec.ScopeSearch}}, Page{}); err != nil || r.Total != 0 || len(r.Page) != 0 {
		t.Errorf("an empty search: %+v %v", r, err)
	}
}

// D-23, guard 1: every count is counted inside the asker's reach. The counts
// are an aggregation over the query itself (never a global one), the kind on
// show narrows only the hits (a post_filter), and every source in the query
// carries its access clause; so a private channel's matches can't reach the
// count of someone who was never in it.
func TestCountsAreCountedInsideTheAskersReach(t *testing.T) {
	allReady(t)
	var body map[string]any
	saved := searchIndex
	t.Cleanup(func() { searchIndex = saved })
	searchIndex = func(_ context.Context, _ []string, raw []byte) (opensearchModels.SearchAnswer, error) {
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		return opensearchModels.SearchAnswer{}, nil
	}
	outsider := Reach{UserUUID: "O", Email: "o@example.test", Channels: []string{"C1"}}
	launch := cond("text", querySpec.OperatorContains, querySpec.Value{Text: "launch"})
	if _, err := Search(context.Background(), outsider, querySpec.Spec{Scope: querySpec.Scope{Kind: querySpec.ScopeSearch}, Filter: launch},
		Page{Kinds: []string{KindMessage}}); err != nil {
		t.Fatal(err)
	}
	aggs, _ := body["aggs"].(map[string]any)
	kinds, _ := aggs["kinds"].(map[string]any)
	if len(aggs) != 1 || len(kinds) != 1 || kinds["terms"] == nil {
		t.Fatalf("the counts are one terms aggregation on _index, nothing that escapes the query: %v", aggs)
	}
	if field := kinds["terms"].(map[string]any)["field"]; field != "_index" {
		t.Errorf("counted by %v", field)
	}
	if pf, _ := json.Marshal(body["post_filter"]); string(pf) != `{"terms":{"_index":["chats","posts","comments"]}}` {
		t.Errorf("the kind on show: %s", pf)
	}
	should := body["query"].(map[string]any)["bool"].(map[string]any)["should"].([]any)
	if len(should) != len(sources) {
		t.Fatalf("%d sources searched, want all %d", len(should), len(sources))
	}
	for _, clause := range should {
		filter := clause.(map[string]any)["bool"].(map[string]any)["filter"].([]any)
		index, _ := filter[0].(map[string]any)["term"].(map[string]any)["_index"].(string)
		var s *source
		for i := range sources {
			if sources[i].index == index {
				s = &sources[i]
			}
		}
		if s == nil {
			t.Fatalf("a clause for no source: %v", filter[0])
		}
		if !reflect.DeepEqual(filter[1], canon(t, notDeleted)) {
			t.Errorf("%s doesn't keep deleted entries out: %v", index, filter[1])
		}
		if s.reach == nil {
			if len(filter) != 2 {
				t.Errorf("%s: %v", index, filter)
			}
			continue
		}
		if len(filter) != 3 || !reflect.DeepEqual(filter[2], canon(t, s.reach(outsider))) {
			t.Errorf("%s isn't held to the asker's reach: %v", index, filter)
		}
	}
}

// D-23: on the last page the total is what was shown. A hit the graph drops
// there comes off its kind's count and the total, and no cursor is offered.
func TestTheLastPageCountsWhatItShows(t *testing.T) {
	withChecks(t, map[string]liveness.Checker{
		"task": func(context.Context, []string) (map[string]bool, error) { return map[string]bool{liveTask: true}, nil },
	})
	saved := searchIndex
	t.Cleanup(func() { searchIndex = saved })
	searchIndex = func(context.Context, []string, []byte) (opensearchModels.SearchAnswer, error) {
		return opensearchModels.SearchAnswer{
			Hits:    []*openSearchStruct.GlobalSearchOpenSearchResp{task(goneTask), task(liveTask)},
			ByIndex: map[string]int{openSearchStruct.TASK_INDEX: 22, openSearchStruct.DOC_INDEX: 4},
			Total:   22,
		}, nil
	}
	spec := querySpec.Spec{Scope: querySpec.Scope{Kind: querySpec.ScopeSearch}, Filter: cond("text", querySpec.OperatorContains, querySpec.Value{Text: "launch"})}
	p, err := Search(context.Background(), Reach{UserUUID: "me"}, spec, Page{Size: 20, After: encodeCursor(20, cursorBinding(Reach{UserUUID: "me"}, spec, Page{Kinds: []string{KindTask}})), Kinds: []string{KindTask}})
	if err != nil {
		t.Fatal(err)
	}
	if p.HasMore || p.Next != "" || len(p.Page) != 1 {
		t.Fatalf("has more %v, next %q, page %d; want the last page of 1", p.HasMore, p.Next, len(p.Page))
	}
	if p.Total != 21 || p.Totals[KindTask] != 21 || p.Totals[KindDoc] != 4 {
		t.Errorf("total %d, totals %v; want 21 tasks shown in all and the docs' 4 untouched", p.Total, p.Totals)
	}
}

// What a search's text names exactly (a task's key) shows first on the first
// page, once: the index leaves its entry out of every page, every page counts
// it, and a page that can't list its kind, or a search that is more than the
// one word, doesn't ask.
func TestWhatTheTextNamesExactlyShowsFirstAndCountsOnce(t *testing.T) {
	withChecks(t, map[string]liveness.Checker{
		"task": func(_ context.Context, ids []string) (map[string]bool, error) {
			out := map[string]bool{}
			for _, id := range ids {
				out[id] = true
			}
			return out, nil
		},
	})
	other := "7a1c9a52-3e1b-4c55-9d2f-0c6f3c1a0001"
	savedSteps, savedDecorators, savedIndex := exactSteps, decorators, searchIndex
	t.Cleanup(func() { exactSteps, decorators, searchIndex = savedSteps, savedDecorators, savedIndex })
	asked := 0
	exactSteps = []exactStep{func(_ context.Context, _ Reach, text string) *openSearchStruct.GlobalSearchOpenSearchResp {
		asked++
		if text == "Q4-12" {
			return task(liveTask)
		}
		return nil
	}}
	decorators = []decorator{func(_ context.Context, page []*openSearchStruct.GlobalSearchOpenSearchResp) {
		for _, h := range page {
			if h.Task != nil {
				h.Task.TaskLabel = &h.Task.Uuid
			}
		}
	}}
	var body string
	hits := 1
	searchIndex = func(_ context.Context, _ []string, raw []byte) (opensearchModels.SearchAnswer, error) {
		body = string(raw)
		page := make([]*openSearchStruct.GlobalSearchOpenSearchResp, hits)
		for i := range page {
			page[i] = task(other)
		}
		return opensearchModels.SearchAnswer{Hits: page, ByIndex: map[string]int{openSearchStruct.TASK_INDEX: 21}, Total: 21}, nil
	}
	key := querySpec.Spec{Scope: querySpec.Scope{Kind: querySpec.ScopeSearch}, Filter: cond("text", querySpec.OperatorContains, querySpec.Value{Text: "Q4-12"})}

	p, err := Search(context.Background(), sample, key, Page{Size: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Page) != 2 || p.Page[0].Task.Uuid != liveTask || p.Page[1].Task.Uuid != other {
		t.Fatalf("the page: %+v; want the key's task first, then the index's", p.Page)
	}
	if p.Totals[KindTask] != 22 || p.Total != 22 {
		t.Errorf("counted %d (%v); want the index's 21 and the key's task", p.Total, p.Totals)
	}
	if !strings.Contains(body, `"must_not":[{"ids":{"values":["`+liveTask+`"]}}]`) {
		t.Errorf("the index wasn't told to leave the key's task out: %s", body)
	}
	if p.Page[0].Task.TaskLabel == nil || p.Page[1].Task.TaskLabel == nil {
		t.Errorf("the page wasn't decorated: %+v", p.Page)
	}

	// The last page counts the key's task among those shown before it.
	hits = 1
	p, err = Search(context.Background(), sample, key, Page{Size: 20, After: encodeCursor(20, cursorBinding(sample, key, Page{}))})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Page) != 1 || p.Page[0].Task.Uuid != other || p.Total != 22 || p.HasMore {
		t.Errorf("the last page: %d hits, total %d, more %v; want 1 and 22 in all", len(p.Page), p.Total, p.HasMore)
	}

	// A page of docs, or more than the one word, doesn't ask.
	asked = 0
	if p, _ := Search(context.Background(), sample, key, Page{Kinds: []string{KindDoc}}); len(p.Page) > 0 && p.Page[0].Task != nil && p.Page[0].Task.Uuid == liveTask {
		t.Errorf("a page of docs led with the task")
	}
	both := &querySpec.Filter{Op: querySpec.OpAnd, Filters: []querySpec.Filter{*key.Filter, *cond("type", querySpec.OperatorIs, querySpec.Value{Text: KindTask})}}
	if p, _ := Search(context.Background(), sample, querySpec.Spec{Scope: key.Scope, Filter: both}, Page{}); p.Page[0].Task.Uuid == liveTask {
		t.Errorf("a search of more than the key led with its task")
	}
	if asked != 1 {
		t.Errorf("the step was asked %d times; want once, for the page of docs, which then leaves it out", asked)
	}
}
