package businness

// The AI search index's copy of every doc's privacy and sharing, put right.
//
// Until 9 Oct a save from the editor re-indexed a doc for AI search as public,
// editing a comment on a doc re-indexed it as public, and a change of who a doc
// is shared with never reached the index. So a private doc's text, and its
// comments, could be found by every member through AI search. Search now
// re-checks each doc it returns against the doc, and writes keep a doc's
// privacy; this writes the current privacy and sharing of every doc, private
// and public, onto its entries, so the index itself is right again, not only
// filtered. An entry that already says what the doc says is left alone, so what
// it reports is how many entries were wrong.
//
// The first version (ai_embeddings_doc_access_v1) wrote private docs only and
// counted docs, not corrections; v2 runs once more everywhere, over every doc.
//
// It runs in the background at startup until it has succeeded once, which the
// system_configs key below records, so later starts skip it. Until then it
// retries with a growing wait (OpenSearch or Dgraph may still be starting, or an
// embedding rebuild may be under way). Running it twice is harmless: it writes
// what the docs say. A startup job rather than an admin command, so that every
// install is fixed by upgrading, without anyone having to know to run it.

import (
	"context"
	"errors"
	"fmt"

	repairBusiness "github.com/akashc777/OneCamp/business/Repair"
	domain "github.com/akashc777/OneCamp/domain/Doc"
	ai "github.com/akashc777/OneCamp/services/AI"
)

const (
	embeddingAccessBackfillKey  = "ai_embeddings_doc_access_v2"
	embeddingAccessBackfillPage = 200
	// embeddingAccessBackfillTries bounds the retries in one process: about
	// three and a half hours of waits, then the next start tries again.
	embeddingAccessBackfillTries = 12
)

// EmbeddingAccessCheck is what one pass did: the docs whose privacy and
// sharing it wrote onto their AI search entries, and the entries that said
// something else and were corrected.
type EmbeddingAccessCheck struct{ Docs, Corrected int }

func (c EmbeddingAccessCheck) String() string {
	return fmt.Sprintf("checked the privacy and sharing of %d docs on their AI search entries and corrected %d entries", c.Docs, c.Corrected)
}

// EmbeddingAccessBackfill writes every doc's privacy and sharing onto its AI
// search entries (backfillEmbeddingAccess) once per install.
var EmbeddingAccessBackfill = repairBusiness.Once[EmbeddingAccessCheck]{
	Key: embeddingAccessBackfillKey, Name: "doc.embedding-access-backfill", Tries: embeddingAccessBackfillTries, Repair: backfillEmbeddingAccess,
}

// backfillEmbeddingAccess writes every doc's privacy and sharing onto its AI
// search entries and its comments'.
func backfillEmbeddingAccess(ctx context.Context) (c EmbeddingAccessCheck, err error) {
	if ai.IsReindexRunning() {
		return c, errors.New("the AI search index is being rebuilt")
	}
	for offset := 0; ; offset += embeddingAccessBackfillPage {
		page, err := domain.GetDocAccessPage(ctx, offset, embeddingAccessBackfillPage)
		if err != nil {
			return c, err
		}
		access := make([]ai.DocAccess, 0, len(page))
		for _, d := range page {
			if d != nil && d.Uuid != "" {
				access = append(access, ai.DocAccessOf(d.Uuid, d))
			}
		}
		n, err := ai.SetDocAccess(ctx, access)
		c.Corrected += n
		if err != nil {
			return c, err
		}
		c.Docs += len(access)
		if len(page) < embeddingAccessBackfillPage {
			break
		}
	}
	// A rebuild that began meanwhile copied the index before these writes
	// reached it, so they are done again once it has finished.
	if ai.IsReindexRunning() {
		return c, errors.New("the AI search index is being rebuilt")
	}
	return c, nil
}
