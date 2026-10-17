package business

import (
	"context"

	docDomain "github.com/akashc777/OneCamp/domain/Doc"
)

// Which inactive docs the retention archive may take, now that docs nest.
//
// Archiving hides a doc (business/Archive archives = deletes and marks). Taken
// alone, a page under a page somebody still uses would vanish from its parent
// for everyone, and a page with a subpage still in use would leave that
// subpage hanging with its parent gone. So the archive takes whole subtrees:
// a doc goes only when nothing above it in the tree stays (its parent goes
// too, or it has none, or its parent is already gone), and nothing under it
// stays (every page under it is inactive too).

// docLinks is the tree around the docs the archive could take: each one's
// live parent (by id, "" for none) and its live children.
type docLinks struct {
	Parent map[string]string
	Kids   map[string][]string
}

// archivableDocs answers which of the inactive docs may be archived together:
// the subset closed downward (all of a doc's live children are in it) and
// upward (a doc's live parent is in it, or it has none). due is every inactive
// doc the job found; links has a Parent and Kids entry for each of them, kids
// and parents named whether or not they're inactive. Pure; the answer keeps
// due's order.
func archivableDocs(due []string, links docLinks) []string {
	in := make(map[string]bool, len(due))
	for _, id := range due {
		in[id] = true
	}
	// Take docs out until nothing changes: a doc whose parent stays, or one of
	// whose children stays, stays; and taking it out can make its parent and
	// its children stay in turn.
	for changed := true; changed; {
		changed = false
		for _, id := range due {
			if !in[id] {
				continue
			}
			if p := links.Parent[id]; p != "" && !in[p] {
				in[id], changed = false, true
				continue
			}
			for _, k := range links.Kids[id] {
				if !in[k] {
					in[id], changed = false, true
					break
				}
			}
		}
	}
	out := make([]string, 0, len(due))
	for _, id := range due {
		if in[id] {
			out = append(out, id)
		}
	}
	return out
}

// archivableDocsOf keeps, of the inactive docs, the ones the archive may take
// together (archivableDocs), reading the tree around them.
func archivableDocsOf(ctx context.Context, due []string) ([]string, error) {
	rows, err := docDomain.GetDocTreeLinks(ctx, due)
	if err != nil {
		return nil, err
	}
	links := docLinks{Parent: map[string]string{}, Kids: map[string][]string{}}
	for _, r := range rows {
		if r.Parent != nil {
			links.Parent[r.Uuid] = r.Parent.Uuid
		}
		for _, k := range r.Children {
			links.Kids[r.Uuid] = append(links.Kids[r.Uuid], k.Uuid)
		}
	}
	return archivableDocs(due, links), nil
}
