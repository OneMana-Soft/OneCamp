package businness

// The AI search index's copy of every private doc's privacy, written right once.
//
// Until this release a save from the editor re-indexed a doc for AI search as
// public, editing a comment on a doc re-indexed it as public, and a change of
// who a doc is shared with never reached the index. So a private doc's text,
// and its comments, could be found by every member through AI search. Search
// now re-checks each doc it returns against the doc, and writes keep a doc's
// privacy; this writes the current privacy and sharing of every private doc
// onto its entries once, so the index itself is right again, not only filtered.
//
// It runs in the background at startup until it has succeeded once, which the
// system_configs key below records, so later starts skip it. Until then it
// retries with a growing wait (OpenSearch or Dgraph may still be starting, or an
// embedding rebuild may be under way). Running it twice is harmless: it writes
// what the docs say. A startup job rather than an admin command, so that every
// install is fixed by upgrading, without anyone having to know to run it.

import (
	"context"
	"database/sql"
	"errors"
	"time"

	domain "github.com/akashc777/OneCamp/domain/Doc"
	"github.com/akashc777/OneCamp/helpers"
	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
	ai "github.com/akashc777/OneCamp/services/AI"
)

const (
	embeddingAccessBackfillKey  = "ai_embeddings_doc_access_v1"
	embeddingAccessBackfillPage = 200
	// embeddingAccessBackfillTries bounds the retries in one process: about
	// three and a half hours of waits, then the next start tries again.
	embeddingAccessBackfillTries = 12
)

// StartEmbeddingAccessBackfill runs RunEmbeddingAccessBackfill in the
// background until it succeeds, ctx ends, or the tries run out.
func StartEmbeddingAccessBackfill(ctx context.Context) {
	helpers.GoSafeNamed("doc.embedding-access-backfill", func() {
		wait := 30 * time.Second
		for try := 0; try < embeddingAccessBackfillTries; try++ {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			written, err := RunEmbeddingAccessBackfill(ctx)
			if err == nil {
				if written > 0 {
					helpers.MessageLogs.InfoLog.Printf("AI search: wrote the privacy and sharing of %d private docs onto their entries", written)
				}
				return
			}
			helpers.MessageLogs.ErrorLog.Printf("AI search: writing private docs' privacy onto their entries failed (will retry): %v", err)
			if wait < 30*time.Minute {
				wait *= 2
			}
		}
	})
}

// RunEmbeddingAccessBackfill writes every private doc's privacy and sharing
// onto its AI search entries and its comments', unless that has been done, and
// then records that it has. It answers how many docs it wrote.
func RunEmbeddingAccessBackfill(ctx context.Context) (int, error) {
	if done, err := embeddingAccessBackfillDone(); err != nil || done {
		return 0, err
	}
	if ai.IsReindexRunning() {
		return 0, errors.New("the AI search index is being rebuilt")
	}
	written := 0
	for offset := 0; ; offset += embeddingAccessBackfillPage {
		docs, err := domain.GetPrivateDocAccessPage(ctx, offset, embeddingAccessBackfillPage)
		if err != nil {
			return written, err
		}
		access := make([]ai.DocAccess, 0, len(docs))
		for _, d := range docs {
			if d != nil && d.Uuid != "" {
				access = append(access, docAccessOf(d.Uuid, d))
			}
		}
		if err := ai.SetDocAccess(ctx, access); err != nil {
			return written, err
		}
		written += len(access)
		if len(docs) < embeddingAccessBackfillPage {
			break
		}
	}
	// A rebuild that began meanwhile copied the index before these writes
	// reached it, so they are done again once it has finished.
	if ai.IsReindexRunning() {
		return written, errors.New("the AI search index is being rebuilt")
	}
	return written, configModel.UpsertConfig(embeddingAccessBackfillKey, "done")
}

func embeddingAccessBackfillDone() (bool, error) {
	cfg, err := configModel.GetConfigByKey(embeddingAccessBackfillKey)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return cfg != nil && cfg.Value == "done", nil
}
