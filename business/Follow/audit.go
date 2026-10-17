package business

// The audit sink: what an auditor asks about a doc, recorded in the admin audit
// log under "docs", one row per change, in its hash chain. Viewing the log is
// on every plan; exporting it stays behind FeatureAuditExport.
//
// Who acted comes from the event (I11): a person, an agent (the row names the
// person who asked it, says an agent acted, and its action is agent.<kind>), or
// the server itself (a sweep).
// A private doc's title never goes into the log: the log is read by admins, who
// can't open a private doc, so the row names it "a private page" and carries its
// id. The row is written while the sink runs, after the change: a change is
// never held up, or refused, for its record.

import (
	"context"
	"fmt"
	"strings"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// AuditedKinds are the doc events the log records, each with how its summary
// reads. The AI edition's record of agents' work (business/AI) records the
// rest of an agent's doc events, so each is one row.
var AuditedKinds = map[resourceEventBusiness.Kind]string{
	resourceEventBusiness.DocShared:         "Shared %s",
	resourceEventBusiness.DocAccessChanged:  "Changed who can open %s",
	resourceEventBusiness.DocLocked:         "Changed the lock on %s",
	resourceEventBusiness.DocRestored:       "Restored %s from the trash",
	resourceEventBusiness.DocVersionRestore: "Restored an earlier version of %s",
	resourceEventBusiness.DocDeleted:        "Moved %s to the trash",
	resourceEventBusiness.DocPurged:         "Deleted %s for good",
	resourceEventBusiness.DocArchived:       "Archived %s",
	resourceEventBusiness.DocExported:       "Exported %s",
	resourceEventBusiness.DocPublished:      "Published %s",
	resourceEventBusiness.DocTransferred:    "Transferred the ownership of %s",
	resourceEventBusiness.DocRecovered:      "Recovered %s",
	resourceEventBusiness.ThreadCreated:     "Started a comment thread on %s",
	resourceEventBusiness.ThreadReplied:     "Replied in a comment thread on %s",
	resourceEventBusiness.ThreadResolved:    "Resolved a comment thread on %s",
	resourceEventBusiness.ThreadReopened:    "Reopened a comment thread on %s",
	resourceEventBusiness.ThreadReanchored:  "Reattached a comment thread on %s",

	// Asking for access (business/Access requests.go), and the answer.
	resourceEventBusiness.DocAccessRequested: "Asked for access to %s",
	resourceEventBusiness.DocAccessAnswered:  "Answered a request for access to %s",
}

// Seams for tests: where the row goes, and the actor's email.
var (
	auditRecord = auditBusiness.RecordForPrincipal
	actorEmail  = func(ctx context.Context, id uuid.UUID) string {
		if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
			return ""
		}
		u, err := userDomain.GetUserByUUID(ctx, id)
		if err != nil || u == nil {
			return ""
		}
		return u.EmailID
	}
)

func isAudited(e resourceEventBusiness.Event) bool {
	_, ok := AuditedKinds[e.Kind]
	return ok && e.ResourceKind == "doc"
}

// record writes the event's row.
func record(ctx context.Context, e resourceEventBusiness.Event) {
	var doc *dgraphStruct.DgraphDoc
	if e.Kind != resourceEventBusiness.DocPurged {
		// A purged doc is gone: its row names it by id alone.
		d, err := docPeople(ctx, e.ResourceUUID)
		if err == nil {
			doc = d
		}
	}
	summary, meta := auditRow(e, doc)
	kind := auditBusiness.ActorHuman
	switch {
	case e.Actor.AgentUUID != "":
		kind = auditBusiness.ActorAgent
	case e.Actor.System || e.Actor.UserUUID == "":
		kind = auditBusiness.ActorSystem
	}
	var actorID *uuid.UUID
	email := ""
	if id, err := uuid.Parse(e.Actor.UserUUID); err == nil {
		actorID = &id
		email = actorEmail(ctx, id)
	}
	// An agent's change is recorded as agent.<kind>, so the AI activity feed,
	// which lists agent.* actions, shows it beside the agent's other work.
	action := string(e.Kind)
	if e.Actor.AgentUUID != "" {
		action = "agent." + action
	}
	if err := auditRecord(ctx, actorID, email, kind, action, auditBusiness.CategoryDocs, summary, meta); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Follow audit: %s on doc %s wasn't recorded: %+v", e.Kind, e.ResourceUUID, err)
	}
}

// auditRow is an event's summary and metadata. doc is the doc as read now, nil
// if it couldn't be. Pure.
func auditRow(e resourceEventBusiness.Event, doc *dgraphStruct.DgraphDoc) (string, map[string]interface{}) {
	private := doc == nil || (doc.IsPrivate != nil && *doc.IsPrivate)
	name := "a page"
	switch {
	case doc != nil && private:
		name = "a private page"
	case doc != nil:
		name = "‘" + titleOf(doc) + "’"
	}
	summary := fmt.Sprintf(AuditedKinds[e.Kind], name)
	if e.Kind == resourceEventBusiness.DocExported {
		// An export says what it made and where (business/ContentIO).
		if format := e.Data["format"]; format != "" {
			summary += " as " + format
		}
		if e.Data["via"] == "browser" {
			summary += ", in the browser"
		}
	}
	switch e.Kind {
	case resourceEventBusiness.DocAccessRequested:
		// The level asked for; the request's id is in the metadata.
		if level := e.Data["level"]; level != "" {
			summary += " (" + level + ")"
		}
	case resourceEventBusiness.DocAccessAnswered:
		// "(approved, edit)": the level given, or for a decline, asked for.
		if status := e.Data["status"]; status != "" {
			summary += " (" + status
			if level := e.Data["level"]; level != "" {
				summary += ", " + level
			}
			summary += ")"
		}
	}
	if e.Kind == resourceEventBusiness.DocShared && len(e.Subjects) > 0 {
		people := "1 person"
		if len(e.Subjects) > 1 {
			people = fmt.Sprintf("%d people", len(e.Subjects))
		}
		summary += " with " + people
		if level := e.Data["level"]; level != "" {
			summary += " (" + level + ")"
		}
	}
	meta := map[string]interface{}{
		"resource_kind": e.ResourceKind,
		"resource_uuid": e.ResourceUUID,
		"at":            e.At.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
	}
	if e.Actor.AgentUUID != "" {
		meta["agent_uuid"] = e.Actor.AgentUUID
	}
	if len(e.Subjects) > 0 {
		meta["people"] = strings.Join(e.Subjects, ",")
	}
	for k, v := range e.Data {
		// A title stays out of the log for a private page, as in the summary.
		if k == "title" && private {
			continue
		}
		meta[k] = v
	}
	return summary, meta
}
