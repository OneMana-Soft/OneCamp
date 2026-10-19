package businness

// How much someone may do with a doc: the one rule every doc check uses, from
// opening it to joining it live (invariant I9). The rule itself is
// domain/Doc's (docAccessRule.go); business/Access asks it through here, and
// access.go's checks are its yes-or-no answers.
//
// The sources, strongest first:
//   - the owner, and the people given full access: full;
//   - the people it is shared with, at the level they were given;
//   - the teams it is shared with: their members and admins, at the team's
//     level;
//   - its space (a project's or a team's): that space's members and admins,
//     at its general level and never below view. One hop: a doc's own space,
//     never a space above it;
//   - its general access, when it isn't private: everyone in the workspace,
//     at view, comment or edit;
//   - "everyone who can read it may comment" raises a reader to comment.
//
// The modifiers: a hard lock holds everyone below full at comment (D-12), and
// a deleted doc is open to nobody outside the trash, which decides by the
// level the person had before it was deleted (Was).

import (
	"context"
	"strings"

	"github.com/akashc777/OneCamp/business/Access/level"
	domain "github.com/akashc777/OneCamp/domain/Doc"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// Viewer is who a decision is for: their graph uid, their uuid, or both
// (domain/Doc docAccessRule.go, where the rule is).
type Viewer = domain.Viewer

// DocDecision is someone's level on a doc, and why.
type DocDecision = domain.DocDecision

// DecideDoc is the person's level on a doc read for them: the one rule.
func DecideDoc(doc *dgraphStruct.DgraphDoc, v Viewer) DocDecision {
	return domain.DecideDoc(doc, v)
}

// GeneralLevel is what the doc's general audience gets.
func GeneralLevel(doc *dgraphStruct.DgraphDoc) level.Level {
	return domain.GeneralLevel(doc)
}

// DecideDocs is the person's decision on each of the docs (by uuid), read in
// one query, keyed by lower-case uuid; a doc that isn't there is left out.
//
// A hosted doc is decided by its host on a context that opens hosted docs
// (WithHostedOpen, docHost.go), and as none on any other.
func DecideDocs(ctx context.Context, v Viewer, docUUIDs []string) (map[string]DocDecision, error) {
	rows, err := domain.GetDocAccessRows(ctx, docUUIDs, v.UID, v.UUID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]DocDecision, len(rows))
	var hosts map[string]Host
	for _, d := range rows {
		if d != nil && d.Uuid != "" {
			id := strings.ToLower(d.Uuid)
			out[id] = DecideDoc(d, v)
			if domain.IsHosted(d) {
				if hosts == nil {
					hosts = map[string]Host{}
				}
				hosts[id] = Host{Kind: d.HostKind, UUID: d.HostUUID}
			}
		}
	}
	out, err = withEndedGrants(ctx, v, out)
	if err != nil {
		return nil, err
	}
	return withHosts(ctx, v, out, hosts)
}

// ReadsDoc is whether someone (by uuid) may read a doc now, by the rule
// (DecideDocs: a grant whose end has passed is none): the one answer the
// broker gets before a subscription to the doc's topic (business/MqttAccess),
// and again when a change takes the doc from people (AccessChanged). A doc
// that isn't there is read by nobody.
func ReadsDoc(ctx context.Context, userUUID, docUUID string) (bool, error) {
	d, err := DecideDocs(ctx, Viewer{UUID: userUUID}, []string{docUUID})
	if err != nil {
		return false, err
	}
	return d[strings.ToLower(docUUID)].Level >= level.View, nil
}

// ShowAccessNow writes the person's decision into the doc's per-person counts
// as the web app reads them (doc_read_access, doc_comment_access,
// doc_edit_access, doc_full_access: more than none is yes), so that the app's
// checks agree with the rule: a team's editor or a space's member may edit,
// an editor a hard lock holds may not, and a grant whose end has passed is
// gone (withEndedGrants), as the resolver and the notices decide, in the
// minute before the expiry sweep runs. Call it on a doc read for the person,
// before it is sent to them.
// It answers the decision; an error says nothing, and the doc isn't shown.
func ShowAccessNow(ctx context.Context, doc *dgraphStruct.DgraphDoc, v Viewer) (DocDecision, error) {
	if doc == nil {
		return DocDecision{}, ErrNotFound
	}
	id := strings.ToLower(doc.Uuid)
	got, err := withEndedGrants(ctx, v, map[string]DocDecision{id: DecideDoc(doc, v)})
	if err != nil {
		return DocDecision{}, err
	}
	showDecision(doc, got[id])
	return got[id], nil
}

// showDecision writes a decision into the doc's per-person counts.
func showDecision(doc *dgraphStruct.DgraphDoc, d DocDecision) {
	if doc == nil {
		return
	}
	yes := func(b bool) uint8 {
		if b {
			return 1
		}
		return 0
	}
	doc.HasReadAccess = yes(d.Level >= level.View)
	doc.HasCommentAccess = yes(d.Level >= level.Comment)
	doc.HasEditAccess = yes(d.Level >= level.Edit)
	doc.HasFullAccess = yes(d.Level >= level.Full)
}
