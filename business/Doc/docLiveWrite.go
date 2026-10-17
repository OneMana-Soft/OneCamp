package businness

// Writing a doc's body from the server: through the Live Doc Writer, never under
// the live copy (invariants I1–I3). Writing doc_body directly was undone by the
// next save of anyone who had the doc open, and once the doc was unloaded it
// opened as a new copy that a sleeping tab doubled.

import (
	"context"
	"errors"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Doc"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/services/collab"
)

// ReplaceBodyLive makes html the doc's body, as one transaction on the live
// copy, keeping a version of what it replaced first (reason "before_write"
// unless given). Blocks that read the same keep their identity. A write the
// collaboration service cannot make answers collab.ErrUnavailable: nothing is
// written underneath instead.
func ReplaceBodyLive(ctx context.Context, docUUID, html string, by collab.By, reason string) (collab.Result, error) {
	if reason == "" {
		reason = "before_write"
	}
	return collab.Live.ReplaceAll(ctx, docUUID, html, collab.Options{By: by, SnapshotFirst: true, SnapshotReason: reason})
}

// SeedBodyLive writes the first body of a doc nobody has written in yet (a
// template, a duplicate, the demo seed). A doc that already holds anything
// answers collab.ErrHasContent. When the collaboration service cannot be
// reached, a doc that is new and empty is written directly: nobody can hold a
// copy of a doc that has never been opened, so no copy can double (I2).
func SeedBodyLive(ctx context.Context, docUUID, html string, by collab.By) (collab.Result, error) {
	r, err := collab.Live.Seed(ctx, docUUID, html, collab.Options{By: by})
	if err == nil || !errors.Is(err, collab.ErrUnavailable) || errors.Is(err, collab.ErrNoSuchDoc) {
		return r, err
	}
	stored, rerr := readStoredBody(ctx, docUUID)
	if rerr != nil {
		return r, err
	}
	if !blankBody(stored) {
		return r, collab.ErrHasContent
	}
	helpers.LogErrorWithContext(ctx, "business/SeedBodyLive doc %s: the collaboration service is unavailable (%v); writing the new doc's first body directly", docUUID, err)
	body := html
	if uerr := UpdateDoc(ctx, &adapter.InputUpdateDoc{DocId: docUUID, Body: &body}); uerr != nil {
		return r, uerr
	}
	// Stored directly; nobody held a copy to tell.
	return collab.Result{Persisted: true, PersistedAt: time.Now()}, nil
}

// blankBody is whether a stored body holds nothing anyone wrote.
func blankBody(body string) bool {
	return helpers.HTMLToPlainText(body) == "" && !strings.Contains(body, "<img") && !strings.Contains(body, "data-type=")
}
