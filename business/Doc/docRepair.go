package businness

// Repairing doc bodies the collaboration service damaged.
//
// WHY THIS EXISTS. The collaboration service writes a doc's HTML body from its
// Yjs document with its own copy of the editor's schema, and that copy lacked
// blocks the web editor writes (`/table`, `+doc` and `#channel` chips, polls,
// charts, diffs, recordings). Before v2.70.0 a save of such a doc replaced the
// whole body with "<p>Error generating HTML</p>"; from v2.70.0 the save failed
// instead, and the doc stopped saving. Both are fixed in the service
// (schema-manifest.json, preserved.js, storeDoc.js); this puts back what the
// first one destroyed, on any install that ran it.
//
// `go-one-camp repairdocs` lists what it would do; `repairdocs --apply` does it.
// It is idempotent: a repaired doc no longer looks damaged, so a second run
// changes nothing. For each doc:
//
//   - body is the placeholder: rebuilt from the doc's saved Yjs state when it
//     has one (written by the collaboration service, so every block is kept),
//     else from the newest version in its history that is not the placeholder
//     (the history keeps the body from before a sharp shrink);
//   - body is very short while its saved state, stored with that very body, holds
//     much more: rebuilt from the state.
//
// A doc open in the collaboration service right now is left alone, because the
// copy in memory is saved over whatever is written underneath it; the run says
// so, and a later run picks it up.

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	adapter "github.com/akashc777/OneCamp/adapter/Doc"
	domain "github.com/akashc777/OneCamp/domain/Doc"
	"github.com/akashc777/OneCamp/helpers"
	snapshotModels "github.com/akashc777/OneCamp/models/postgres/DocSnapshot"
	"github.com/akashc777/OneCamp/services/collab"
)

const (
	repairPageSize = 200
	// A body with at most this much text, saved with a state that holds much
	// more, is a candidate (repairWantsState).
	repairShortRunes    = 40
	collabRenderTimeout = 20 * time.Second
)

// RepairReport is what a run found and did.
type RepairReport struct {
	Scanned       int
	Placeholder   int
	ShortBody     int
	FromState     int
	FromSnapshot  int
	SkippedOpen   int
	Unrecoverable []string
	Failed        []string
}

func (r RepairReport) String() string {
	return fmt.Sprintf("scanned %d docs: %d with the error placeholder, %d short beside a fuller saved state; restored %d from saved state, %d from history; %d open now (run again later); %d unrecoverable; %d failed",
		r.Scanned, r.Placeholder, r.ShortBody, r.FromState, r.FromSnapshot, r.SkippedOpen, len(r.Unrecoverable), len(r.Failed))
}

// collabRender is the collaboration service writing a saved Yjs state as HTML
// (the Live Doc Writer's render op, services/collab Render). With no state it
// only says whether the doc is open there.
type collabRender func(ctx context.Context, docUUID, stateB64 string) (html string, open bool, err error)

func runeLen(html string) int {
	return utf8.RuneCountInString(strings.TrimSpace(helpers.HTMLToPlainText(html)))
}

// repairWantsState reports whether a body that is not the placeholder should be
// rebuilt from a state saved with it: the body is very short, and the state,
// written as HTML, holds clearly more.
func repairWantsState(body, rendered string) bool {
	if unsafeCollabBody(rendered) || rendered == body {
		return false
	}
	b, r := runeLen(body), runeLen(rendered)
	return b <= repairShortRunes && r > 2*b+20
}

// RepairDocs scans every doc and, with apply, restores the damaged ones.
func RepairDocs(ctx context.Context, apply bool, out io.Writer) (RepairReport, error) {
	return repairDocs(ctx, apply, out, postCollabRender, latestGoodSnapshotBody, writeRepairedBody)
}

// writeRepairedBody stores a restored body: with the state it was written from
// (one mutation, so the doc opens from that state), or alone when it came from
// the history (the state is then stale, and the doc is rebuilt from the body).
// Either way the snippet and the search index follow.
func writeRepairedBody(ctx context.Context, docUUID, body, state string) error {
	// Deleted docs too: restored, a doc should be whole.
	return updateDoc(ctx, &adapter.InputUpdateDoc{DocId: docUUID, Body: &body}, state, true)
}

type repairWriter func(ctx context.Context, docUUID, body, state string) error

func repairDocs(ctx context.Context, apply bool, out io.Writer, render collabRender, history func(context.Context, string) (string, error), write repairWriter) (RepairReport, error) {
	var rep RepairReport
	say := func(format string, a ...any) { _, _ = fmt.Fprintf(out, format+"\n", a...) }
	verb := "would restore"
	if apply {
		verb = "restored"
	}

	for offset := 0; ; offset += repairPageSize {
		page, err := domain.GetDocBodyPage(ctx, offset, repairPageSize)
		if err != nil {
			return rep, err
		}
		for _, d := range page {
			if d == nil || d.Uuid == "" {
				continue
			}
			rep.Scanned++
			placeholder := strings.TrimSpace(d.Body) == collabErrorBody
			stateMatches := d.YjsBodyHash != "" && d.YjsBodyHash == collabBodyHash(d.Body)
			short := !placeholder && stateMatches && runeLen(d.Body) <= repairShortRunes
			if !placeholder && !short {
				continue
			}

			full, err := domain.GetCollabDocByUUID(ctx, d.Uuid)
			if err != nil || full == nil {
				rep.Failed = append(rep.Failed, d.Uuid)
				say("%s: could not read it: %v", d.Uuid, err)
				continue
			}
			// Read again: the page may be a moment old.
			placeholder = strings.TrimSpace(full.Body) == collabErrorBody
			if !placeholder && !(full.YjsBodyHash != "" && full.YjsBodyHash == collabBodyHash(full.Body)) {
				continue
			}
			if placeholder {
				rep.Placeholder++
			}

			if full.YjsState != "" {
				html, open, rerr := render(ctx, d.Uuid, full.YjsState)
				switch {
				case rerr != nil:
					say("%s: the collaboration service could not write its saved state: %v", d.Uuid, rerr)
					if !placeholder {
						rep.Failed = append(rep.Failed, d.Uuid)
						continue
					}
					// A placeholder still has its history, below.
				case open:
					rep.SkippedOpen++
					say("%s: open in an editor now, left alone", d.Uuid)
					continue
				case placeholder && !unsafeCollabBody(html), !placeholder && repairWantsState(full.Body, html):
					if !placeholder {
						rep.ShortBody++
					}
					if apply {
						if err := write(ctx, d.Uuid, html, full.YjsState); err != nil {
							rep.Failed = append(rep.Failed, d.Uuid)
							say("%s: writing the restored body failed: %v", d.Uuid, err)
							continue
						}
					}
					rep.FromState++
					say("%s: %s from its saved state (%d characters of text)", d.Uuid, verb, runeLen(html))
					continue
				}
			}
			if !placeholder {
				continue
			}

			body, herr := history(ctx, d.Uuid)
			if herr != nil || unsafeCollabBody(body) {
				rep.Unrecoverable = append(rep.Unrecoverable, d.Uuid)
				say("%s: the placeholder, with no saved state and no earlier version to restore", d.Uuid)
				continue
			}
			if _, open, rerr := render(ctx, d.Uuid, ""); rerr != nil || open {
				if rerr != nil {
					rep.Failed = append(rep.Failed, d.Uuid)
					say("%s: could not ask the collaboration service whether it is open: %v", d.Uuid, rerr)
				} else {
					rep.SkippedOpen++
					say("%s: open in an editor now, left alone", d.Uuid)
				}
				continue
			}
			if apply {
				if err := write(ctx, d.Uuid, body, ""); err != nil {
					rep.Failed = append(rep.Failed, d.Uuid)
					say("%s: writing the restored body failed: %v", d.Uuid, err)
					continue
				}
			}
			rep.FromSnapshot++
			say("%s: %s from its version history (%d characters of text)", d.Uuid, verb, runeLen(body))
		}
		if len(page) < repairPageSize {
			break
		}
	}
	say("repairdocs: %s", rep)
	return rep, nil
}

// latestGoodSnapshotBody is the newest version in a doc's history that is a
// real body, not the placeholder.
func latestGoodSnapshotBody(ctx context.Context, docUUID string) (string, error) {
	snaps, err := snapshotModels.ListSnapshots(ctx, docUUID, time.Time{}, 30)
	if err != nil {
		return "", err
	}
	for _, s := range snaps {
		gz, err := getDocSnapshotObject(ctx, s.ObjectKey)
		if err != nil {
			continue
		}
		raw, err := docGunzip(gz)
		if err != nil {
			continue
		}
		if body := string(raw); !unsafeCollabBody(body) {
			return body, nil
		}
	}
	return "", fmt.Errorf("no earlier version")
}

// postCollabRender asks the collaboration service to write a saved state as
// HTML, with the same schema it saves with (its Live Doc Writer's render op).
func postCollabRender(ctx context.Context, docUUID, stateB64 string) (string, bool, error) {
	cctx, cancel := context.WithTimeout(ctx, collabRenderTimeout)
	defer cancel()
	html, open, err := collab.Render(cctx, docUUID, stateB64)
	if err != nil {
		return "", false, fmt.Errorf("render: %w", err)
	}
	return html, open, nil
}
