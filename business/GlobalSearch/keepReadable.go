package business

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
)

// readCheck is one kind's read check on a page: keep is the page less the
// hits of its kind the asker can't read now (a check that can't be made
// leaves its whole kind out and every other hit in), and repair, when named,
// has the entries of the hits it left out written again (of names what a hit
// is about).
type readCheck struct {
	name   string
	keep   func(ctx context.Context, hits []*openSearchStruct.GlobalSearchOpenSearchResp) []*openSearchStruct.GlobalSearchOpenSearchResp
	repair string
	of     func(*openSearchStruct.GlobalSearchOpenSearchResp) string
}

// readChecks are the checks every page goes through, for reach: the
// transcript lines, docs, boards, events and project files the asker may
// find now.
func readChecks(reach Reach) []readCheck {
	projects := make([]*dgraphStruct.DgraphProject, 0, len(reach.Projects))
	for _, p := range reach.Projects {
		projects = append(projects, &dgraphStruct.DgraphProject{Uuid: p})
	}
	type hits = []*openSearchStruct.GlobalSearchOpenSearchResp
	return []readCheck{
		{name: "transcripts", keep: func(ctx context.Context, h hits) hits { return keepReadableTranscriptHits(ctx, reach, h) }},
		{name: "docs", keep: func(ctx context.Context, h hits) hits { return keepReadableDocHits(ctx, reach.UserUUID, h) }, repair: repairDoc, of: docOfHit},
		{name: "boards", keep: func(ctx context.Context, h hits) hits { return keepReadableBoardHits(ctx, reach.UserUUID, h) }, repair: repairBoard, of: boardOfHit},
		{name: "events", keep: func(ctx context.Context, h hits) hits { return keepReadableEventHits(ctx, reach.Who, h) }, repair: repairEvent, of: eventOfHit},
		{name: "project files", keep: func(ctx context.Context, h hits) hits {
			return keepReadableProjectFileHits(ctx, reach.UserUUID, projects, h)
		}},
	}
}

// keepReadable runs every check on the same page at once, one goroutine each
// (so at most len(checks), five), and keeps a hit only if every check kept
// it, in the page's order. Each check fails closed for its own kind, as it
// did when they ran one after another, and none stops another: the docs
// check failing leaves the boards, events and the rest as they were. A check
// that panics keeps nothing, and a request cancelled while they ran keeps
// nothing. The entries of the hits a check left out are repaired after.
func keepReadable(ctx context.Context, hits []*openSearchStruct.GlobalSearchOpenSearchResp, checks []readCheck) []*openSearchStruct.GlobalSearchOpenSearchResp {
	if len(hits) == 0 {
		return hits
	}
	kept := make([][]*openSearchStruct.GlobalSearchOpenSearchResp, len(checks))
	each := make([]int, len(checks))
	for i := range each {
		each[i] = i
	}
	// A check that panics is recovered by EachBounded and leaves kept[i] nil.
	helpers.EachBounded("search.read-checks", each, len(checks), func(i int) {
		kept[i] = checks[i].keep(ctx, hits)
	})
	if ctx.Err() != nil {
		return hits[:0]
	}
	votes := make(map[*openSearchStruct.GlobalSearchOpenSearchResp]int, len(hits))
	var drops []droppedKind
	for i, c := range checks {
		for _, h := range kept[i] {
			votes[h]++
		}
		if c.repair != "" {
			drops = append(drops, droppedKind{c.repair, droppedIDs(hits, kept[i], c.of)})
		}
	}
	repairDropped(drops...)
	out := make([]*openSearchStruct.GlobalSearchOpenSearchResp, 0, len(hits))
	for _, h := range hits {
		if votes[h] == len(checks) {
			out = append(out, h)
		}
	}
	return out
}
