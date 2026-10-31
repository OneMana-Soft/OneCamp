package business

// The search engine on the one grammar (docs C2-1; design/QUERY_STACK_CONTRACT.md):
// a built spec searched across every index the asker may search, in one
// request: the page, how many match in each kind and in all (an aggregation
// over the whole query, which the kind being shown narrows only as a
// post_filter), and the cursor of the next page.
//
// The counts are the index's honest count (decision D-23): its access fields
// gate every one, so nothing the asker never had is counted. Each page is
// still checked against its source (readableDocs.go, readableBoards.go,
// readableEvents.go, readableFiles.go, liveHits.go), and what that check
// drops comes off the counts too: on the last page the total is what was
// shown.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	querySpec "github.com/akashc777/OneCamp/business/QuerySpec"
	searchQuery "github.com/akashc777/OneCamp/business/SearchQuery"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	opensearchModels "github.com/akashc777/OneCamp/models/openSearch/GlobalSearch"
	"github.com/akashc777/OneCamp/models/taskread"
)

// Paging.
const (
	// DefaultPageSize is a page's size when none is asked for.
	DefaultPageSize = 20
	// MaxPageSize is the largest page.
	MaxPageSize = 50
	// resultWindow is how far into the results a search pages: the index's
	// own window (from + size).
	resultWindow = 10000
)

// The orders a search may come back in.
const (
	SortBest   = "best"
	SortNewest = "newest"
	SortOldest = "oldest"
)

// Page asks for one page of a search.
type Page struct {
	// Size is how many hits (DefaultPageSize when 0).
	Size int
	// After is the cursor a page answered with (Next), or "" for the first.
	After string
	// Kinds are the kinds this page lists (a kind's chip): the counts still
	// count every kind.
	Kinds []string
	// Sort is SortBest (the default), SortNewest or SortOldest; the spec's
	// ORDER BY comes before it.
	Sort string
	// Now and Zone are when and where dates are counted: the asker's zone,
	// UTC when nil.
	Now  time.Time
	Zone *time.Location
}

// Result is one page of a search.
type Result struct {
	Page []*openSearchStruct.GlobalSearchOpenSearchResp `json:"page"`
	// Totals are how many match in each kind, whatever kinds the page lists;
	// Total is how many the listing holds: every kind, or the kinds asked
	// for. Both are the index's count inside the asker's reach, less what the
	// graph's check dropped from this page, and on the last page Total is
	// what the pages showed (D-23).
	Totals  map[string]int `json:"totals"`
	Total   int            `json:"total"`
	HasMore bool           `json:"has_more"`
	// Next is the cursor of the page after this one, while HasMore.
	Next string `json:"next,omitempty"`
	// Notes say what of the query search couldn't apply (a field it can't
	// filter on yet).
	Notes []searchQuery.Issue `json:"notes,omitempty"`
}

// ErrBadSearchCursor is a cursor this server didn't write.
var ErrBadSearchCursor = errors.New("that isn't a search cursor")

// encodeCursor is the place offset in the search binding names
// (cursorBinding).
func encodeCursor(offset int, binding string) string {
	raw, _ := json.Marshal(struct {
		O int    `json:"o"`
		Q string `json:"q"`
	}{offset, binding})
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodeCursor is the offset a cursor encodeCursor wrote for the search
// binding names; "" is the start. Any other cursor, or one of another
// search, is ErrBadSearchCursor.
func decodeCursor(cursor, binding string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(raw) > 64 {
		return 0, ErrBadSearchCursor
	}
	var c struct {
		O *int   `json:"o"`
		Q string `json:"q"`
	}
	if err := json.Unmarshal(raw, &c); err != nil || c.O == nil || *c.O < 0 || *c.O >= resultWindow || c.Q != binding {
		return 0, ErrBadSearchCursor
	}
	return *c.O, nil
}

// cursorBinding names the search a cursor belongs to: its asker, query,
// kinds on show and order, hashed. A cursor carries it, and one sent with any
// other search is refused, so a cursor only ever moves along the pages of the
// search that wrote it. (A cursor is no grant: every page runs the asker's
// whole query, reach and live checks again.)
func cursorBinding(reach Reach, spec querySpec.Spec, page Page) string {
	raw, _ := json.Marshal(struct {
		Asker string
		Spec  querySpec.Spec
		Kinds []string
		Sort  string
	}{reach.UserUUID, spec, page.Kinds, page.Sort})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

// The seams the engine reads the index and the asker's projects through.
var searchIndex = opensearchModels.SearchPage

// An exact step finds the one thing a search's text names exactly (a task's
// key, "Q4-12"), to show first. It is asked only when the whole search is
// that one word or phrase, finds only what the asker may open now, and is
// nil when the text names nothing. An area adds its step from an init in
// this package.
type exactStep func(ctx context.Context, reach Reach, text string) *openSearchStruct.GlobalSearchOpenSearchResp

// A decorator lays over a page's hits what their entries don't hold (a
// task's key), in one read; a read that fails leaves the page as it is.
type decorator func(ctx context.Context, page []*openSearchStruct.GlobalSearchOpenSearchResp)

var (
	exactSteps []exactStep
	decorators []decorator
)

// exactHit is what the search's text names exactly, if the page may list
// its kind.
func exactHit(ctx context.Context, reach Reach, spec querySpec.Spec, page Page) *openSearchStruct.GlobalSearchOpenSearchResp {
	f := spec.Filter
	if f == nil || f.Op != "" || f.Field != "text" || f.Operator != querySpec.OperatorContains || len(f.Values) != 1 {
		return nil
	}
	for _, step := range exactSteps {
		h := step(ctx, reach, f.Values[0].Text)
		if h == nil {
			continue
		}
		kind := kindOfHit(h)
		if kind == "" || (len(spec.Scope.Kinds) > 0 && !contains(spec.Scope.Kinds, kind)) || (len(page.Kinds) > 0 && !contains(page.Kinds, kind)) {
			return nil
		}
		return h
	}
	return nil
}

// Search is one page of the search spec describes, as reach's asker.
func Search(ctx context.Context, reach Reach, spec querySpec.Spec, page Page) (Result, error) {
	binding := cursorBinding(reach, spec, page)
	offset, err := decodeCursor(page.After, binding)
	if err != nil {
		return Result{}, err
	}
	size := page.Size
	if size <= 0 {
		size = DefaultPageSize
	}
	if size > MaxPageSize {
		size = MaxPageSize
	}
	if offset+size > resultWindow {
		size = resultWindow - offset
	}
	if page.Now.IsZero() {
		page.Now = time.Now()
	}
	result := Result{Totals: map[string]int{}}
	if spec.Filter == nil && len(spec.Scope.Kinds) == 0 {
		return result, nil // nothing asked
	}

	c := newCompiler(ctx, reach, spec, page.Now, page.Zone)
	// What the text names exactly shows first on the first page, once: the
	// index leaves its entry out of every page, and every page counts it.
	exact := exactHit(ctx, reach, spec, page)
	var exactIndex, exactID string
	if exact != nil {
		var t string
		t, exactID = hitKey(exact)
		exactIndex = hitIndex[t]
	}
	// The sources this search looks at, compiled. The calendars the asker
	// sees are read once, before any query is built, and only when events
	// are among them.
	type looked struct {
		s source
		p part
	}
	var looks []looked
	events := false
	for _, s := range sources {
		if len(spec.Scope.Kinds) > 0 && !contains(spec.Scope.Kinds, s.kind) {
			continue
		}
		p := c.compile(s, spec.Filter)
		if p.never {
			continue
		}
		looks = append(looks, looked{s, p})
		events = events || s.kind == KindEvent
	}
	if events && reach.Calendars == nil {
		reach.Calendars = eventCalendars(ctx, reach.Who)
	}
	var should []map[string]any
	var indices []string
	for _, l := range looks {
		q := sourceQuery(l.s, reach, l.p)
		if l.s.index == exactIndex && exactID != "" {
			leaveOut(q, exactID)
		}
		should = append(should, q)
		indices = append(indices, l.s.index)
	}
	result.Notes = c.unfilteredNotes()
	if len(should) == 0 {
		if exact != nil {
			result.Totals[kindOfHit(exact)], result.Total = 1, 1
			if offset == 0 {
				result.Page = []*openSearchStruct.GlobalSearchOpenSearchResp{exact}
			}
		}
		return result, nil
	}

	body := map[string]any{
		"from":             offset,
		"size":             size,
		"track_total_hits": true,
		"_source":          map[string]any{"excludes": unreturned},
		"highlight":        highlight,
		"query":            map[string]any{"bool": map[string]any{"should": should, "minimum_should_match": 1}},
		"aggs":             map[string]any{"kinds": map[string]any{"terms": map[string]any{"field": "_index", "size": len(sources)}}},
		"sort":             sortOf(spec.Sorts, page.Sort, &result.Notes),
	}
	if len(page.Kinds) > 0 {
		body["post_filter"] = map[string]any{"terms": map[string]any{"_index": kindIndices(page.Kinds)}}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Result{}, err
	}
	// Restricted tasks the asker may not see, with their comments and files,
	// are left out inside the query, so no page and no count has them.
	tr, err := taskread.From(ctx)
	if err != nil {
		return Result{}, err
	}
	query, err := tr.SearchWithout(string(raw), "task_id", "comment_task_id", "attachment_task_id")
	if err != nil {
		return Result{}, err
	}
	answer, err := searchIndex(ctx, indices, []byte(query))
	if err != nil {
		return Result{}, err
	}
	for index, n := range answer.ByIndex {
		if kind := indexKind(index); kind != "" {
			result.Totals[kind] += n
		}
	}
	total := answer.Total
	result.HasMore = offset+len(answer.Hits) < total && offset+size < resultWindow

	// The graph has the last word on every hit; what it drops comes off the
	// counts, so a page never says more than it shows.
	kept := keepPage(ctx, reach, answer.Hits)
	dropped := map[string]int{}
	keptSet := map[*openSearchStruct.GlobalSearchOpenSearchResp]bool{}
	for _, h := range kept {
		keptSet[h] = true
	}
	for _, h := range answer.Hits {
		if !keptSet[h] {
			dropped[kindOfHit(h)]++
		}
	}
	for kind, n := range dropped {
		result.Totals[kind] = max(0, result.Totals[kind]-n)
		total -= n
	}
	// before is how many the pages before this one showed.
	before := offset
	if exact != nil {
		result.Totals[kindOfHit(exact)]++
		total++
		if offset == 0 {
			kept = withExact(exact, kept)
		} else {
			before++
		}
	}
	if !result.HasMore {
		total = before + len(kept)
	}
	for _, d := range decorators {
		d(ctx, kept)
	}
	result.Page = kept
	result.Total = max(total, before+len(kept))
	if result.HasMore {
		result.Next = encodeCursor(offset+size, binding)
	}
	return result, nil
}

// withExact is a page with the exact hit first, and once.
func withExact(first *openSearchStruct.GlobalSearchOpenSearchResp, page []*openSearchStruct.GlobalSearchOpenSearchResp) []*openSearchStruct.GlobalSearchOpenSearchResp {
	t, id := hitKey(first)
	out := make([]*openSearchStruct.GlobalSearchOpenSearchResp, 0, len(page)+1)
	out = append(out, first)
	for _, h := range page {
		if ht, hid := hitKey(h); id != "" && ht == t && hid == id {
			continue
		}
		out = append(out, h)
	}
	return out
}

// keepPage is a page without the hits the graph says the asker can't open
// now, or whose content is gone: every check today's search makes.
func keepPage(ctx context.Context, reach Reach, hits []*openSearchStruct.GlobalSearchOpenSearchResp) []*openSearchStruct.GlobalSearchOpenSearchResp {
	hits = keepLiveHits(ctx, hits)
	hits = demoPeopleHits(reach.Email, hits)
	before := hits
	hits = keepReadableDocHits(ctx, reach.UserUUID, hits)
	repairDropped(repairDoc, before, hits, docOfHit)
	before = hits
	hits = keepReadableBoardHits(ctx, reach.UserUUID, hits)
	repairDropped(repairBoard, before, hits, boardOfHit)
	before = hits
	hits = keepReadableEventHits(ctx, reach.Who, hits)
	repairDropped(repairEvent, before, hits, eventOfHit)
	projects := make([]*dgraphStruct.DgraphProject, 0, len(reach.Projects))
	for _, p := range reach.Projects {
		projects = append(projects, &dgraphStruct.DgraphProject{Uuid: p})
	}
	return keepReadableProjectFileHits(ctx, reach.UserUUID, projects, hits)
}

// kindOfHit is the kind a hit is.
func kindOfHit(h *openSearchStruct.GlobalSearchOpenSearchResp) string {
	switch h.Type {
	case openSearchStruct.CHAT_TYPE, openSearchStruct.POST_TYPE, openSearchStruct.COMMENT_TYPE:
		return KindMessage
	case openSearchStruct.ATTACHMENT_TYPE:
		return KindFile
	case openSearchStruct.DOC_TYPE:
		return KindDoc
	case openSearchStruct.BOARD_TYPE:
		return KindBoard
	case openSearchStruct.EVENT_TYPE:
		return KindEvent
	case openSearchStruct.TASK_TYPE:
		return KindTask
	case openSearchStruct.PROJECT_TYPE:
		return KindProject
	case openSearchStruct.CHANNEL_TYPE:
		return KindChannel
	case openSearchStruct.TEAM_TYPE:
		return KindTeam
	case openSearchStruct.USER_TYPE:
		return KindPerson
	}
	return ""
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// notDeleted keeps out entries whose content was deleted.
var notDeleted = map[string]any{"bool": map[string]any{"must_not": []map[string]any{{"exists": map[string]any{"field": "deleted_date"}}}}}

// leaveOut has q, a source's query, leave out the entry id as well: added to
// what its bool leaves out already, never in place of it.
func leaveOut(q map[string]any, id string) {
	b := q["bool"].(map[string]any)
	out, _ := b["must_not"].([]map[string]any)
	b["must_not"] = append(out, map[string]any{"ids": map[string]any{"values": []string{id}}})
}

// sourceQuery is one source's part of the search: its own entries, that the
// asker may reach and that aren't deleted, matching the compiled spec.
func sourceQuery(s source, reach Reach, p part) map[string]any {
	filter := []map[string]any{term("_index", s.index), notDeleted}
	if s.reach != nil {
		filter = append(filter, s.reach(reach))
	}
	b := map[string]any{"filter": filter}
	if !p.always {
		b["must"] = []map[string]any{p.q}
	}
	if s.boost > 0 {
		b["boost"] = s.boost
	}
	return map[string]any{"bool": b}
}

// unreturned are the fields an entry is matched on and a hit never carries: a
// doc's body, and an event's notes, the people it is theirs to see and its
// guests' names (who is invited is the event's to show, as its guests may see).
var unreturned = []string{"doc_body", "event_notes", "event_people", "event_guest_names"}

// highlight marks what matched, as the search page shows it.
var highlight = map[string]any{
	"pre_tags":  []string{"<mark>"},
	"post_tags": []string{"</mark>"},
	"fields": map[string]any{
		"chat_body": map[string]any{}, "post_body": map[string]any{}, "comment_body": map[string]any{},
		"attachment_file_name": map[string]any{}, "doc_title": map[string]any{}, "doc_body": map[string]any{},
		"task_name": map[string]any{}, "task_desc": map[string]any{}, "project_name": map[string]any{},
		"team_name": map[string]any{}, "ch_name": map[string]any{}, "user_name": map[string]any{},
		"event_title": map[string]any{}, "event_notes": map[string]any{}, "event_location": map[string]any{},
	},
}

// sortOf is a search's order: the spec's ORDER BY on the fields the index
// can sort by, then the order asked for.
func sortOf(sorts []querySpec.Sort, order string, notes *[]searchQuery.Issue) []map[string]any {
	var out []map[string]any
	for _, s := range sorts {
		field := ""
		switch s.Field {
		case "created":
			field = "created_date"
		case "updated":
			field = updatedQueried
		}
		if field == "" {
			*notes = append(*notes, searchQuery.Issue{Code: "unsorted", Level: searchQuery.LevelNote,
				Message: "Search can't sort by " + s.Field + " yet, so it's in its usual order."})
			continue
		}
		dir := "asc"
		if s.Desc {
			dir = "desc"
		}
		out = append(out, map[string]any{field: map[string]any{"order": dir, "unmapped_type": "date", "missing": "_last"}})
	}
	created := func(dir string) map[string]any {
		return map[string]any{"created_date": map[string]any{"order": dir, "unmapped_type": "date"}}
	}
	switch order {
	case SortNewest:
		out = append(out, created("desc"), map[string]any{"_score": map[string]any{"order": "desc"}})
	case SortOldest:
		out = append(out, created("asc"), map[string]any{"_score": map[string]any{"order": "desc"}})
	default:
		out = append(out, map[string]any{"_score": map[string]any{"order": "desc"}}, created("desc"))
	}
	return out
}
