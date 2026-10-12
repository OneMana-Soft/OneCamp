package business

// Search hits on a doc are kept only if the person can read the doc now.
//
// The docs index carries each doc's privacy and sharing, and its comments' and
// files' entries carry their doc's, so the index query can grant a hit. But
// those copies are written after a change and could be wrong for long (a doc
// made public stayed private in them, a person taken off a doc stayed on it,
// so they kept finding its title and text). The doc decides: a hit on a doc,
// or on a comment or a file in one, is kept only if the doc's own read rule
// (business/Doc CanRead, which also refuses a deleted doc) says the person may
// read it, asked of the graph once a page. A page that can't be checked keeps
// none of them: refusing costs a search result, keeping a wrong one costs a
// private doc. AI search holds its own doc hits to the same rule
// (business/AI keepReadableDocs).

import (
	"context"
	"strings"

	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	"github.com/akashc777/OneCamp/helpers"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
)

// docOfHit is the doc a hit is about: a doc, or the doc a comment or a file is
// in; "" for anything else.
func docOfHit(h *openSearchStruct.GlobalSearchOpenSearchResp) string {
	switch {
	case h == nil:
		return ""
	case h.Doc != nil:
		return h.Doc.Uuid
	case h.Comment != nil:
		return h.Comment.CommentDocUuid
	case h.Attachment != nil:
		return h.Attachment.AttachmentDocUuid
	}
	return ""
}

// readableDocs answers which of the docs the person may read. A seam for tests.
var readableDocs = docBusiness.ReadableDocsFor

// keepReadableDocHits returns hits without those on a doc the person (by uuid)
// can't read now, nor a comment or file in one. Hits on anything else are
// kept as they are.
func keepReadableDocHits(ctx context.Context, userUUID string, hits []*openSearchStruct.GlobalSearchOpenSearchResp) []*openSearchStruct.GlobalSearchOpenSearchResp {
	var docs []string
	seen := map[string]bool{}
	for _, h := range hits {
		if d := strings.ToLower(docOfHit(h)); d != "" && !seen[d] {
			seen[d] = true
			docs = append(docs, d)
		}
	}
	if len(docs) == 0 {
		return hits
	}
	readable, err := readableDocs(ctx, docs, userUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "search: could not check %d docs against the graph, leaving their results out: %v", len(docs), err)
		readable = nil
	}
	kept := make([]*openSearchStruct.GlobalSearchOpenSearchResp, 0, len(hits))
	dropped := 0
	for _, h := range hits {
		if d := strings.ToLower(docOfHit(h)); d != "" && !readable[d] {
			dropped++
			continue
		}
		kept = append(kept, h)
	}
	if dropped > 0 {
		helpers.LogInfoWithContext(ctx, "search: left out %d results on docs the person can't read", dropped)
	}
	return kept
}
