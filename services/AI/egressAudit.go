package ai

// Content-free audit for local-only egress blocks (AI data residency,
// Requirement 4.1). When local-only mode is on and an operation would reach a
// non-local endpoint, the dial guard already refuses the connection; here we
// also record WHO was blocked, WHAT operation, and the endpoint KIND, with the
// actor taken from the request context. No prompt/content is ever written
// (Property 5).

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	auditModel "github.com/akashc777/OneCamp/models/postgres/AdminAudit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// auditEgressBlocked appends a best-effort security audit row for a local-only
// block. A failed audit write never affects the (already-blocked) operation.
func auditEgressBlocked(ctx context.Context, op string, kind ProviderType) {
	e := &auditModel.AuditEntry{
		Action:   "ai.egress.blocked",
		Category: auditModel.CategorySecurity,
		Summary:  "Local-only AI mode blocked an AI " + op + " to a non-local endpoint (" + string(kind) + ")",
	}
	if ui, ok := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo); ok {
		id := ui.UserPostgresInfo.Id
		e.ActorID = &id
		e.ActorEmail = ui.UserPostgresInfo.EmailID
	}
	_ = auditModel.Insert(ctx, e)
}
