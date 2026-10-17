package businness

// Saving a doc from the collaboration service (docs programme A4; invariants I1,
// I8, I14 and I15). The service's in-memory Yjs copy is the doc while it is open;
// this stores what it sends, and nothing else writes the body of an existing doc
// (I1: server-side writes go through the Live Doc Writer, docLiveWrite.go).
//
// ONE UPSERT. The body, its preview, the Yjs state, the body's hash and the
// lineage are written together or not at all (domain/Doc WriteCollabSave). They
// were two commits, and a failed second one left a stale state that made the
// next open rebuild the doc as a new copy.
//
// NO FAN-OUT WITHOUT A CHANGE (I15). The upsert answers the hash it replaced. A
// body that hashes the same (an edit undone, a save of the state only) stops
// there: no search update, no re-embedding, no hooks, no event. Every save used to
// do all of it, every 5 to 30 seconds while anyone typed.
//
// A CHANGE IS TOLD (I14). A changed body re-indexes the doc's search entry, runs
// the save hooks with the Change (the AI index's re-embed, with the doc's title,
// is one, in the AI edition), and emits doc.updated, credited to whoever made it.

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	resourceViewBusiness "github.com/akashc777/OneCamp/business/ResourceView"
	domain "github.com/akashc777/OneCamp/domain/Doc"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// CollabSave is one save as the collaboration service sends it.
type CollabSave struct {
	DocUUID string `json:"documentId"`
	// HTML is the body, written by the service with the editor's schema.
	HTML string `json:"htmlContent"`
	// YjsState is the service's Yjs state for that body, base64.
	YjsState string `json:"yjsState"`
	// Lineage is the copy of the doc the state is (the service's epoch).
	Lineage string `json:"lineage"`
	// KeepBody stores the state only, leaving the body as it is: the service
	// could not write the HTML (ExpectBodyHash empty: against the stored body),
	// or the body is unchanged or just rebuilt (against ExpectBodyHash, only
	// while that is still the stored body).
	KeepBody       bool   `json:"keepBody"`
	ExpectBodyHash string `json:"expectBodyHash"`
	// Contributors are the people who edited since the last save, and Agents
	// the agents, with who asked each.
	Contributors []string           `json:"contributors"`
	Agents       []AgentContributor `json:"agents"`
}

// CollabSaved is what a save did.
type CollabSaved struct {
	// Stored is false for a state-only save whose body is not the stored one
	// any more: nothing was written, and the service sends the whole body.
	Stored bool `json:"stored"`
	// Changed is whether the body changed.
	Changed bool `json:"changed"`
}

// Seams, so the rules are tested without the graph, search or the AI index.
var (
	writeCollabSave = domain.WriteCollabSave
	readStoredBody  = func(ctx context.Context, docUUID string) (string, error) {
		doc, err := domain.GetSystemDocByUUID(ctx, docUUID)
		if err != nil {
			return "", err
		}
		if doc == nil {
			return "", ErrNotFound
		}
		return doc.Body, nil
	}
	// fanOutSavedBody is a changed save's side effects outside the hooks.
	fanOutSavedBody = reindexSavedBody
)

// SaveFromCollab stores one save from the collaboration service.
func SaveFromCollab(ctx context.Context, in CollabSave) (CollabSaved, error) {
	if in.KeepBody {
		return saveCollabState(ctx, in)
	}
	if unsafeCollabBody(in.HTML) {
		helpers.LogErrorWithContext(ctx, "business/SaveFromCollab doc %s: refused a blank or placeholder body (%d bytes)", in.DocUUID, len(in.HTML))
		return CollabSaved{}, ErrUnsafeCollabBody
	}
	hash := collabBodyHash(in.HTML)
	previous := plausiblePrevious(ctx, in.DocUUID, len(in.HTML))
	w, err := writeCollabSave(ctx, domain.CollabWrite{
		DocUUID:    in.DocUUID,
		Body:       in.HTML,
		Snippet:    helpers.TruncateRunes(helpers.HTMLToPlainText(in.HTML), docSnippetRunes),
		State:      in.YjsState,
		Hash:       hash,
		Lineage:    in.Lineage,
		EditorUUID: lastEditor(in),
		Conversion: nobodyEdited(in),
	})
	if err != nil {
		return CollabSaved{}, collabSaveError(err)
	}
	resourceViewBusiness.SetCachedStateSize(ctx, resourceViewBusiness.ResourceDoc, in.DocUUID, len(in.HTML))
	if w.PrevHash == hash {
		return CollabSaved{Stored: true}, nil
	}

	c := Change{
		DocUUID: in.DocUUID, Body: in.HTML, Hash: hash, PrevHash: w.PrevHash, Lineage: in.Lineage,
		Contributors: in.Contributors, Agents: in.Agents, SavedAt: time.Now(),
	}
	if previous != "" && massDeletion(len(previous), len(in.HTML)) {
		c.Previous = previous
	}
	fanOutSavedBody(ctx, c)
	SaveHooks.run(ctx, c)
	resourceEventBusiness.Bus.Emit(ctx, savedEvent(c))
	return CollabSaved{Stored: true, Changed: true}, nil
}

// nobodyEdited is whether a save holds nobody's edit: no person and no agent
// edited since the last save, so what changed is the collaboration service's
// own conversion of the doc as it loaded (block ids, old content moved over;
// its edits are the user "unknown", never counted). The service records every
// person's and every agent's edit before the save that holds it.
func nobodyEdited(in CollabSave) bool {
	return len(in.Contributors) == 0 && len(in.Agents) == 0
}

// lastEditor is who a save that changes the body credits as the doc's last
// editor (doc_updated_by): the person whose edits it holds, or, for an agent's
// edit, the person who asked it (I11). With several, the first of them in
// order, so the doc always names someone who edited it in that save.
func lastEditor(in CollabSave) string {
	people := append([]string(nil), in.Contributors...)
	sort.Strings(people)
	for _, p := range people {
		if _, err := uuid.Parse(p); err == nil {
			return p
		}
	}
	for _, a := range in.Agents {
		if _, err := uuid.Parse(a.AskerUUID); err == nil {
			return a.AskerUUID
		}
	}
	return ""
}

// saveCollabState stores a state without its body.
func saveCollabState(ctx context.Context, in CollabSave) (CollabSaved, error) {
	if in.YjsState == "" {
		return CollabSaved{}, errors.New("keepBody needs the document's Yjs state")
	}
	hash := in.ExpectBodyHash
	if hash == "" {
		// The service could not write this doc's HTML at all: the state holds
		// every edit, so the next open has them, beside the stored body.
		body, err := readStoredBody(ctx, in.DocUUID)
		if err != nil {
			return CollabSaved{}, collabSaveError(err)
		}
		hash = collabBodyHash(body)
		helpers.LogErrorWithContext(ctx, "business/SaveFromCollab doc %s: the collaboration service could not write its HTML; kept the stored body, saved the Yjs state only", in.DocUUID)
	}
	w, err := writeCollabSave(ctx, domain.CollabWrite{DocUUID: in.DocUUID, StateOnly: true, State: in.YjsState, Hash: hash, Lineage: in.Lineage})
	if err != nil {
		return CollabSaved{}, collabSaveError(err)
	}
	return CollabSaved{Stored: w.Stored}, nil
}

func collabSaveError(err error) error {
	switch {
	case errors.Is(err, ErrDocDeleted):
		return ErrDocDeleted
	case errors.Is(err, ErrNotFound):
		return ErrNotFound
	}
	return err
}

// plausiblePrevious is the stored body, read only when this save could be a
// mass deletion (the cached size says it shrinks sharply), so history can keep
// what was deleted. A normal edit skips the read; a cache miss reads.
func plausiblePrevious(ctx context.Context, docUUID string, newBytes int) string {
	if cached, ok := resourceViewBusiness.CachedStateSize(ctx, resourceViewBusiness.ResourceDoc, docUUID); ok {
		if !massDeletion(cached, newBytes) {
			return ""
		}
	}
	body, err := readStoredBody(ctx, docUUID)
	if err != nil {
		return ""
	}
	return body
}

// massDeletion is whether a body of oldBytes shrinking to newBytes is a large
// deletion worth keeping the old body for.
func massDeletion(oldBytes, newBytes int) bool {
	return oldBytes >= docSnapshotMassDeleteMinByte && float64(newBytes) < float64(oldBytes)*docSnapshotMassDeleteRatio
}

// savedEvent is doc.updated for a changed save, credited to the one person or
// agent who made it; a save holding several people's edits names them in its
// data. Never the body (I14).
func savedEvent(c Change) resourceEventBusiness.Event {
	e := resourceEventBusiness.Event{
		Kind: resourceEventBusiness.DocUpdated, ResourceKind: "doc", ResourceUUID: c.DocUUID, At: c.SavedAt,
		Data: map[string]string{"lineage": c.Lineage},
	}
	people := append([]string(nil), c.Contributors...)
	sort.Strings(people)
	switch {
	case len(c.Agents) == 1 && len(people) == 0:
		e.Actor = resourceEventBusiness.Actor{UserUUID: c.Agents[0].AskerUUID, AgentUUID: c.Agents[0].AgentUUID}
	case len(people) == 1 && len(c.Agents) == 0:
		e.Actor = resourceEventBusiness.Actor{UserUUID: people[0]}
	default:
		e.Actor = resourceEventBusiness.Actor{System: len(people) == 0 && len(c.Agents) == 0}
	}
	if len(people) > 1 {
		if len(people) > 10 {
			people = people[:10]
		}
		e.Data["editors"] = strings.Join(people, ",")
	}
	return e
}

// reindexSavedBody brings the doc's search entry up to the saved body: written
// whole from the doc (domain/Doc searchEntry.go), in the background. The AI
// index follows through its own save hook, in the AI edition (business/AI
// docSaveEmbed.go), with the doc's title and who may see it.
func reindexSavedBody(_ context.Context, c Change) {
	syncDocSearch(c.DocUUID, false)
}
