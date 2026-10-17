package business

// The docs home's search (docs programme B5, structure.md P0.6): the words in
// a doc's body, not only its title, with the passages they were found in. It
// replaces the title-only searches the docs page used (/doc/searchPrivate and
// /doc/searchPublic, a regexp over titles).
//
// The search index finds candidates for the person (domain/DocHome); the
// Access Resolver decides which of them they may open, and a hit it refuses
// is never named (I9, I12): not in the results, not in a result's path, not
// in the count.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	domain "github.com/akashc777/OneCamp/domain/DocHome"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	docSearchModel "github.com/akashc777/OneCamp/models/openSearch/DocSearch"
)

// ErrSearchTooShort is a search for less than two letters.
var ErrSearchTooShort = errors.New("Type at least 2 letters to search.")

// maxSearchRunes bounds what is searched for.
const maxSearchRunes = 200

// SearchItem is one doc a search found, as a docs list row shows it, with the
// passages it was found in.
type SearchItem struct {
	UUID      string              `json:"doc_uuid"`
	Title     string              `json:"doc_title"`
	Icon      string              `json:"doc_icon,omitempty"`
	Snippet   string              `json:"doc_snippet,omitempty"`
	Private   *bool               `json:"doc_private,omitempty"`
	CreatedAt *time.Time          `json:"doc_created_at,omitempty"`
	UpdatedAt *time.Time          `json:"doc_updated_at,omitempty"`
	Owner     *docBusiness.Person `json:"owner,omitempty"`
	// Path is up to three pages above it, the top first.
	Path []docBusiness.Crumb `json:"path"`
	// Highlights are passages of its body: text, the words found in <mark>.
	Highlights     []string `json:"highlights"`
	TitleHighlight string   `json:"title_highlight,omitempty"`
}

// SearchPage is one page of a search's results.
type SearchPage struct {
	Query string       `json:"query"`
	Docs  []SearchItem `json:"docs"`
	// Count is how many of the docs searched the person can open: the pages'
	// own count, exact. Capped says more docs matched than were searched.
	Count      int    `json:"count"`
	Capped     bool   `json:"capped"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// Searcher is who searches: the person, and the projects and teams they're in.
type Searcher struct {
	Subject      accessBusiness.Subject
	ProjectUUIDs []string
	TeamUUIDs    []string
}

// Seams for tests: the index, and the graph's rows.
var (
	runDocSearch = docSearchModel.SearchDocs
	readDocRows  = docBusiness.DocRows
)

// cleanSearchText trims what was typed and bounds it. Pure.
func cleanSearchText(text string) (string, error) {
	text = strings.TrimSpace(text)
	if utf8.RuneCountInString(text) < 2 {
		return "", ErrSearchTooShort
	}
	if utf8.RuneCountInString(text) > maxSearchRunes {
		text = string([]rune(text)[:maxSearchRunes])
	}
	return text, nil
}

// SafePassage keeps a passage's <mark> and </mark> and makes every other
// angle bracket text, so a passage can't carry a tag of anyone's. The index
// escapes passages already (its html encoder); this holds if it didn't. Pure.
func SafePassage(s string) string {
	var b strings.Builder
	for len(s) > 0 {
		switch {
		case strings.HasPrefix(s, "<mark>"):
			b.WriteString("<mark>")
			s = s[len("<mark>"):]
		case strings.HasPrefix(s, "</mark>"):
			b.WriteString("</mark>")
			s = s[len("</mark>"):]
		case s[0] == '<':
			b.WriteString("&lt;")
			s = s[1:]
		case s[0] == '>':
			b.WriteString("&gt;")
			s = s[1:]
		default:
			b.WriteByte(s[0])
			s = s[1:]
		}
	}
	return strings.TrimSpace(b.String())
}

// docRes is a doc, as the Access Resolver names it.
func docRes(id string) accessBusiness.Resource {
	return accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: strings.ToLower(id)}
}

// The access rule is asked about a search's docs filterChunk at a time,
// filterWorkers at once: today's rule reads the graph once a doc (A5 batches
// it), and a search checks up to SearchCandidates of them, which one after
// another kept a person waiting.
const (
	filterChunk   = 25
	filterWorkers = 8
)

// readable is which of the docs the person may open now.
func readable(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]bool, error) {
	out := make(map[string]bool, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu    sync.Mutex
		first error
		wg    sync.WaitGroup
		slots = make(chan struct{}, filterWorkers)
	)
	for start := 0; start < len(ids); start += filterChunk {
		chunk := ids[start:min(start+filterChunk, len(ids))]
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer func() {
				<-slots
				wg.Done()
			}()
			res := make([]accessBusiness.Resource, len(chunk))
			for i, id := range chunk {
				res[i] = docRes(id)
			}
			kept, err := accessBusiness.Rules.Filter(ctx, who, res, accessBusiness.LevelView)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if first == nil {
					first = err
					cancel()
				}
				return
			}
			for _, r := range kept {
				out[strings.ToLower(r.UUID)] = true
			}
		}()
	}
	wg.Wait()
	if first != nil {
		return nil, first
	}
	return out, nil
}

// SearchDocs finds the docs the person may open whose words match text (owner,
// when given, keeps one person's), best first, and answers the page from
// offset, of at most limit.
func SearchDocs(ctx context.Context, who Searcher, text, owner string, offset, limit int) (*SearchPage, error) {
	text, err := cleanSearchText(text)
	if err != nil {
		return nil, err
	}
	body, err := domain.DocSearchRequest(domain.DocSearchQuery{
		Text: text, UserUUID: who.Subject.UserUUID, ProjectUUIDs: who.ProjectUUIDs, TeamUUIDs: who.TeamUUIDs, OwnerUUID: owner,
	})
	if err != nil {
		return nil, err
	}
	hits, total, err := runDocSearch(ctx, body)
	if err != nil {
		return nil, err
	}

	// The hits the person may open, best first, each once.
	seen := map[string]bool{}
	var ids []string
	byID := map[string]docSearchModel.DocHit{}
	for _, h := range hits {
		id := strings.ToLower(h.UUID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
		byID[id] = h
	}
	open, err := readable(ctx, who.Subject, ids)
	if err != nil {
		return nil, err
	}
	var shown []string
	for _, id := range ids {
		if open[id] {
			shown = append(shown, id)
		}
	}

	page := &SearchPage{Query: text, Docs: []SearchItem{}, Count: len(shown), Capped: total > len(hits)}
	if offset < 0 {
		offset = 0
	}
	if offset >= len(shown) {
		return page, nil
	}
	end := min(offset+limit, len(shown))
	pageIDs := shown[offset:end]
	if end < len(shown) {
		page.NextCursor = helpers.OffsetCursor(end)
	}

	rows, err := readDocRows(ctx, pageIDs, who.Subject.UserUID)
	if err != nil {
		return nil, err
	}
	rowOf := make(map[string]*dgraphStruct.DgraphDoc, len(rows))
	var above []string
	aboveSeen := map[string]bool{}
	for _, d := range rows {
		if d == nil {
			continue
		}
		rowOf[strings.ToLower(d.Uuid)] = d
		for _, p := range docBusiness.PathAbove(d) {
			if id := strings.ToLower(p.Uuid); id != "" && !aboveSeen[id] {
				aboveSeen[id] = true
				above = append(above, id)
			}
		}
	}
	openAbove, err := readable(ctx, who.Subject, above)
	if err != nil {
		return nil, err
	}

	for _, id := range pageIDs {
		d := rowOf[id]
		if d == nil {
			// Gone from the graph since it was indexed: not shown, and logged
			// so the index is put right by its repair.
			helpers.LogInfoWithContext(ctx, "business/DocHome: search found %s, which the graph no longer has", id)
			page.Count--
			continue
		}
		h := byID[id]
		it := SearchItem{
			UUID: d.Uuid, Title: d.Title, Icon: d.Icon, Snippet: d.Snippet, Private: d.IsPrivate,
			CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt, Highlights: []string{},
		}
		it.Owner = docBusiness.OwnerOf(d)
		it.Path = docBusiness.CrumbsOf(d, func(id string) bool { return openAbove[strings.ToLower(id)] })
		for _, passage := range h.BodyPassages {
			if s := SafePassage(passage); s != "" {
				it.Highlights = append(it.Highlights, s)
			}
		}
		if h.TitlePassage != "" {
			it.TitleHighlight = SafePassage(h.TitlePassage)
		}
		page.Docs = append(page.Docs, it)
	}
	return page, nil
}
