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

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	accessModels "github.com/akashc777/OneCamp/models/postgres/Access"
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

	// Asking for access (business/Access requests.go), and the answer: what
	// the request asked to do with the doc (askVerb), and whether it was
	// approved.
	resourceEventBusiness.DocAccessRequested: "Asked to %s %s",
	resourceEventBusiness.DocAccessAnswered:  "%s a request to %s %s",
}

func isAudited(e resourceEventBusiness.Event) bool {
	_, ok := AuditedKinds[e.Kind]
	return ok && e.ResourceKind == "doc"
}

// docsAudit is the docs' events in the admin audit log, under "docs". It runs
// for a bulk import's events too.
var docsAudit = auditBusiness.EventSink{Name: "audit", Category: auditBusiness.CategoryDocs, Wants: isAudited, Row: docRow, InBulk: true}

// docRow is the event's row, with the doc as read now.
func docRow(ctx context.Context, e resourceEventBusiness.Event) (string, map[string]interface{}) {
	var doc *dgraphStruct.DgraphDoc
	if e.Kind != resourceEventBusiness.DocPurged {
		// A purged doc is gone: its row names it by id alone.
		d, err := docPeople(ctx, e.ResourceUUID)
		if err == nil {
			doc = d
		}
	}
	return auditRow(e, doc)
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
	var summary string
	switch e.Kind {
	case resourceEventBusiness.DocAccessRequested:
		// "Asked to edit ‘Plan’"; the request's id is in the metadata.
		summary = fmt.Sprintf(AuditedKinds[e.Kind], askVerb(e.Data["level"]), name)
	case resourceEventBusiness.DocAccessAnswered:
		// "Approved a request to edit ‘Plan’", and the level given when an
		// approval gave another than the one asked for.
		answer := "Declined"
		if e.Data["status"] == accessModels.StatusApproved {
			answer = "Approved"
		}
		summary = fmt.Sprintf(AuditedKinds[e.Kind], answer, askVerb(e.Data["asked"]), name)
		if given := e.Data["level"]; answer == "Approved" && given != e.Data["asked"] {
			summary += ", with " + given + " access"
		}
	default:
		summary = fmt.Sprintf(AuditedKinds[e.Kind], name)
	}
	if e.Kind == resourceEventBusiness.DocExported {
		// An export says what it made and where (business/ContentIO).
		if format := e.Data["format"]; format != "" {
			summary += " as " + format
		}
		if e.Data["via"] == "browser" {
			summary += ", in the browser"
		}
	}
	if e.Kind == resourceEventBusiness.DocShared && len(e.Subjects) > 0 {
		summary += " with " + auditBusiness.PeopleWho(len(e.Subjects), e.Data["level"])
	}
	return summary, auditBusiness.EventMeta(e, private)
}
