package business

// The AI record of what agents do to docs (AI edition; docs programme A6).
//
// Every doc event an agent made (Actor.AgentUUID set: a proposal, an edit, a
// comment, a page it made or moved) is one row in the audit log's hash chain,
// as agent.<kind> under "agent", which the AI activity feed lists with the rest
// of the agent's work: what it did, for whom, and when. The person who asked it
// is the row's actor, as on every agent row (I11). A doc event the docs audit
// records already (a delete, a share: business/Follow AuditedKinds) is that
// audit's row, written as agent.<kind> too, so each event is one row. Rows carry
// ids, never a doc's title or words.

import (
	"context"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	followBusiness "github.com/akashc777/OneCamp/business/Follow"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// agentDocVerbs is how each kind of agent doc event reads in the feed.
var agentDocVerbs = map[resourceEventBusiness.Kind]string{
	resourceEventBusiness.DocCreated:           "Made a page",
	resourceEventBusiness.DocUpdated:           "Edited a page",
	resourceEventBusiness.DocRenamed:           "Renamed a page",
	resourceEventBusiness.DocMoved:             "Moved a page",
	resourceEventBusiness.DocMentioned:         "Mentioned people in a page",
	resourceEventBusiness.DocCommentCreated:    "Commented on a page",
	resourceEventBusiness.DocProposalCreated:   "Proposed a change to a page",
	resourceEventBusiness.DocProposalAccepted:  "Accepted a proposed change to a page",
	resourceEventBusiness.DocProposalDeclined:  "Declined a proposed change to a page",
	resourceEventBusiness.DocProposalWithdrawn: "Withdrew a proposed change to a page",
}

// Seams for tests: where the row goes, and the asker's email.
var (
	recordAgentRow = auditBusiness.RecordForPrincipal
	askerEmail     = func(ctx context.Context, id uuid.UUID) string {
		if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
			return ""
		}
		if u, err := userDomain.GetUserByUUID(ctx, id); err == nil && u != nil {
			return u.EmailID
		}
		return ""
	}
)

func init() {
	resourceEventBusiness.Sinks.Register(resourceEventBusiness.Sink{
		Name:   "ai.record",
		Wants:  agentDocEvent,
		Run:    recordAgentDocEvent,
		InBulk: true,
	})
}

// agentDocEvent is an agent's doc event that no other row records.
func agentDocEvent(e resourceEventBusiness.Event) bool {
	if e.Actor.AgentUUID == "" || e.ResourceKind != "doc" {
		return false
	}
	_, audited := followBusiness.AuditedKinds[e.Kind]
	return !audited
}

func recordAgentDocEvent(ctx context.Context, e resourceEventBusiness.Event) {
	var asker *uuid.UUID
	email := ""
	if id, err := uuid.Parse(e.Actor.UserUUID); err == nil {
		asker = &id
		email = askerEmail(ctx, id)
	}
	summary := agentDocVerbs[e.Kind]
	if summary == "" {
		summary = "Changed a page"
	}
	meta := map[string]interface{}{
		"resource_kind": e.ResourceKind,
		"resource_uuid": e.ResourceUUID,
		"agent_uuid":    e.Actor.AgentUUID,
	}
	for k, v := range e.Data {
		if k != "title" {
			meta[k] = v
		}
	}
	if err := recordAgentRow(ctx, asker, email, auditBusiness.ActorAgent, "agent."+string(e.Kind), auditBusiness.CategoryAgent, summary, meta); err != nil {
		helpers.LogErrorWithContext(ctx, "business/AI record: an agent's %s on doc %s wasn't recorded: %+v", e.Kind, e.ResourceUUID, err)
	}
}
