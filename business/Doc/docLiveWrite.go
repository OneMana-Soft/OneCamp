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
// template, the demo seed, an agent's new doc), loudly: the people its body
// mentions are told, as a person's own words tell them. A doc that already
// holds anything answers collab.ErrHasContent. When the collaboration service
// cannot be reached, a doc that is new and empty is written directly: nobody
// can hold a copy of a doc that has never been opened, so no copy can double
// (I2).
func SeedBodyLive(ctx context.Context, docUUID, html string, by collab.By) (collab.Result, error) {
	return seedBodyWith(ctx, docUUID, html, collab.Options{By: by})
}

// KeepCopiedMentions keeps whom html mentions as the doc's, before it is
// seeded: a body whose words were written elsewhere, a copy's (DuplicateDocs)
// or an import's (Content IO). Whoever it mentions was told where it was
// written, so the content is attributed, not the save: the seed's save finds
// nobody new to tell even when it comes back loud (its quiet mark lost to a
// restart, or past its hour), while a mention typed in the doc since is told.
// They stay kept if the seed then fails: it may have gone in with its answer
// lost, and a doc left blank keeps whatever its next save holds.
func KeepCopiedMentions(ctx context.Context, docUUID, html string) {
	people, _ := bodyReferences(html)
	if _, err := syncMentions(ctx, docUUID, people); err != nil {
		helpers.LogErrorWithContext(ctx, "business/KeepCopiedMentions doc %s: who its body mentions wasn't kept before its seed, so only the seed being quiet keeps them untold: %+v", docUUID, err)
	}
}

// seedBodyWith is SeedBodyLive with the write's options: the first body of a
// doc nobody has opened (a template's, a duplicate's). The seed refuses a doc
// that already holds anything, so two tabs never both insert a template, and
// with the collaboration service unreachable it writes only a doc that is
// still blank. A template's seed is loud: making a doc from one is a person's
// act, and the mentions in it are theirs to send. A quiet seed (a copy's body:
// collab.Options.Quiet) keeps whom its body mentions and tells nobody. Written directly, it runs no save hook, so its mentions and links are
// kept here as a quiet save keeps them: the first edit after it tells only
// whom that edit adds. Only a page takes a body: the collaboration service
// refuses to open a folder, a file or a link, which reads as the service being
// down, and the fallback answers ErrNotAPage for one instead of writing into it.
func seedBodyWith(ctx context.Context, docUUID, html string, o collab.Options) (collab.Result, error) {
	r, err := collab.Live.Seed(ctx, docUUID, html, o)
	if err == nil || !errors.Is(err, collab.ErrUnavailable) || errors.Is(err, collab.ErrNoSuchDoc) {
		return r, err
	}
	stored, rerr := readStoredBody(ctx, docUUID)
	if errors.Is(rerr, ErrNotAPage) {
		return r, rerr
	}
	if rerr != nil {
		return r, err
	}
	if !blankBody(stored) {
		return r, collab.ErrHasContent
	}
	helpers.LogErrorWithContext(ctx, "business/SeedBodyLive doc %s: the collaboration service is unavailable (%v); writing the new doc's first body directly", docUUID, err)
	if werr := writeFirstBody(ctx, docUUID, html); werr != nil {
		return r, werr
	}
	now := time.Now()
	if o.Quiet {
		// The Change the service's save would have carried. Kept whatever
		// becomes of the request: the body is in already.
		c := Change{DocUUID: docUUID, Body: html, Hash: collabBodyHash(html), SavedAt: now, Quiet: true}
		switch {
		case o.By.AgentUUID != "":
			c.Agents = []AgentContributor{{AgentUUID: o.By.AgentUUID, AskerUUID: o.By.UserUUID}}
		case o.By.UserUUID != "":
			c.Contributors = []string{o.By.UserUUID}
		}
		mentionsSaved(quietly(context.WithoutCancel(ctx)), c)
	}
	// Stored directly; nobody held a copy to tell.
	return collab.Result{Persisted: true, PersistedAt: now}, nil
}

// writeFirstBody writes a new doc's first body underneath the collaboration
// service, for a seed it could not take (a seam for tests).
var writeFirstBody = func(ctx context.Context, docUUID, html string) error {
	return UpdateDoc(ctx, &adapter.InputUpdateDoc{DocId: docUUID, Body: &html})
}

// blankBody is whether a stored body holds nothing anyone wrote.
func blankBody(body string) bool {
	return helpers.HTMLToPlainText(body) == "" && !strings.Contains(body, "<img") && !strings.Contains(body, "data-type=")
}
