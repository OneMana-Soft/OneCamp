package business

import (
	"context"

	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	docDomain "github.com/akashc777/OneCamp/domain/Doc"
	"github.com/akashc777/OneCamp/helpers"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

// A doc's, a board's or an event's index entry carries who may read it,
// written on every change of its sharing. When the live check of a page
// (readableDocs.go, readableBoards.go, readableEvents.go) drops a hit the
// entry let through, that entry disagrees with its source: a write that
// failed, or an event gone since. The hit is already off the page and
// off that page's counts; the entry is written again from its source, so the
// next count is right too. Only the dropped entries, each at most once in
// registry.SearchEntryRepair's window whoever searches, and off the request
// path, so searching again and again can't make the index rewrite anything.

const (
	repairDoc   = "doc"
	repairBoard = "board"
	repairEvent = "event"
)

var (
	// repairEntry writes one doc's (with its comments' and files') or one
	// board's (with its comments') entries again from the graph, or one
	// event's from the calendar's store. A seam for tests.
	repairEntry = func(kind, id string) {
		switch kind {
		case repairDoc:
			docDomain.SyncDocSearch(id, true)
		case repairBoard:
			boardBusiness.SyncBoardSearch(context.Background(), id)
		case repairEvent:
			calendarBusiness.SearchEntriesChanged(context.Background(), []string{id})
		}
	}
	// goRepairs runs the repairs of one page in the background. A seam for
	// tests.
	goRepairs = func(repair func()) { helpers.GoSafeNamed("search.entry-repair", repair) }
)

// repairDropped has the entries of the hits in before that kept leaves out
// written again; of names the doc, board or event a hit is about.
func repairDropped(kind string, before, kept []*openSearchStruct.GlobalSearchOpenSearchResp, of func(*openSearchStruct.GlobalSearchOpenSearchResp) string) {
	if len(before) == len(kept) {
		return
	}
	stays := make(map[*openSearchStruct.GlobalSearchOpenSearchResp]bool, len(kept))
	for _, h := range kept {
		stays[h] = true
	}
	seen := map[string]bool{}
	var ids []string
	for _, h := range before {
		if id := of(h); id != "" && !stays[h] && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}
	goRepairs(func() {
		for _, id := range ids {
			if redisStore.AllowFixedWindowOrLocal(context.Background(), registry.SearchEntryRepair, []string{kind, id}, 1).Allowed {
				repairEntry(kind, id)
			}
		}
	})
}
