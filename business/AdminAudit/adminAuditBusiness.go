// Package business (AdminAudit) records admin configuration changes for
// compliance. It extracts the actor, IP, and user-agent from the request and
// writes a redacted, append-only entry. All writes are best-effort: an audit
// failure is logged but never blocks the originating admin action.
package business

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"fmt"

	"github.com/akashc777/OneCamp/helpers"
	auditModel "github.com/akashc777/OneCamp/models/postgres/AdminAudit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// Re-export categories so callers use one import.
const (
	CategorySettings    = auditModel.CategorySettings
	CategoryIntegration = auditModel.CategoryIntegration
	CategoryAuth        = auditModel.CategoryAuth
	CategoryApp         = auditModel.CategoryApp
	CategorySecurity    = auditModel.CategorySecurity
)

// Record writes an audit entry derived from the request context. `metadata`
// may be nil; if provided it must already be redacted (no raw secrets).
// Record writes an entry for the person making this request.
//
// The kind is not a parameter because this function reads a browser session:
// there is a human on the other end by construction, and letting a caller claim
// otherwise would make the field a suggestion rather than a fact.
func Record(r *http.Request, action, category, summary string, metadata map[string]interface{}) {
	ctx := r.Context()

	entry := &auditModel.AuditEntry{
		Action:    action,
		Category:  category,
		Summary:   summary,
		ActorKind: ActorHuman,
		IPAddress: clientIP(r),
		UserAgent: truncate(r.UserAgent(), 512),
	}

	if id, email, ok := ActorFromContext(ctx); ok {
		entry.ActorID = &id
		entry.ActorEmail = email
	}

	if metadata != nil {
		if b, err := json.Marshal(metadata); err == nil {
			s := string(b)
			entry.Metadata = &s
		}
	}

	// Fire-and-forget so the audit write never adds latency to the admin
	// action. Uses a detached context bound by the model's own DB timeout.
	go func() {
		defer func() { _ = recover() }()
		_ = auditModel.Insert(context.Background(), entry)
	}()
}

// ListWhere is List with an initiator filter.
//
// initiator is the client's one word: a kind ("schedule", "handoff", ...),
// "unattended" for every kind nobody was watching, or empty for no filter. The
// translation to a set lives here so the query and the predicate on Initiator
// cannot disagree about what unattended means.
func ListWhere(ctx context.Context, category, initiator string, limit, offset int) ([]*auditModel.AuditEntry, error) {
	f := auditModel.ListFilter{Category: category}
	switch {
	case initiator == "":
	case initiator == "unattended":
		f.Initiators = UnattendedInitiators()
	case Initiator(initiator).Valid():
		f.Initiators = []string{initiator}
	default:
		return nil, fmt.Errorf("audit: unknown initiator filter %q", initiator)
	}
	return auditModel.ListFiltered(ctx, f, limit, offset)
}

// ActorFromContext resolves the authenticated user behind a request.
//
// Extracted because a second caller needed it and the two must agree: an audit
// entry names an actor, and so does an evidence pack, and a pack that disagreed
// with the log about who asked for it would undermine the one thing it exists
// to establish. Returns ok=false for an unauthenticated or system context, which
// callers record as an empty actor rather than inventing one.
func ActorFromContext(ctx context.Context) (uuid.UUID, string, bool) {
	ui, ok := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	if !ok {
		return uuid.Nil, "", false
	}
	return ui.UserPostgresInfo.Id, ui.UserPostgresInfo.EmailID, true
}

// Verify recomputes the audit hash chain and reports the first divergence (if
// any), so an admin/auditor can confirm the log is unaltered.
func Verify(ctx context.Context) (*auditModel.VerifyResult, error) {
	return auditModel.Verify(ctx)
}

// VerifyRecent recomputes the chain over the last limit entries, for callers on a
// request path where a full walk would grow without bound. The result discloses
// that it checked a window; pass that through rather than reporting it as a full
// verification.
func VerifyRecent(ctx context.Context, limit int) (*auditModel.VerifyResult, error) {
	return auditModel.VerifyRecent(ctx, limit)
}

// ExportEntries returns the audit entries (oldest-first, chain order) for an
// admin export, optionally filtered by category and date range.
func ExportEntries(ctx context.Context, category string, from, to *time.Time) ([]*auditModel.AuditEntry, error) {
	return auditModel.ListForExport(ctx, category, from, to)
}

// EntriesToCSV renders audit entries as CSV, including the chain hash per row so
// the export is independently verifiable.
func EntriesToCSV(entries []*auditModel.AuditEntry) ([]byte, error) {
	var buf bytes.Buffer
	cw := csv.NewWriter(&buf)
	_ = cw.Write([]string{"seq", "id", "created_at", "actor_email", "action", "category", "summary", "metadata", "ip_address", "entry_hash", "prev_hash"})
	for _, e := range entries {
		meta := ""
		if e.Metadata != nil {
			meta = *e.Metadata
		}
		_ = cw.Write([]string{
			strconv.FormatInt(e.Seq, 10),
			e.Id.String(),
			e.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
			e.ActorEmail,
			e.Action,
			e.Category,
			e.Summary,
			meta,
			e.IPAddress,
			e.EntryHash,
			e.PrevHash,
		})
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// clientIP mirrors middleware/loginRateLimit.clientIP: trust proxy headers only
// when TRUST_PROXY_HEADERS=true (behind a known LB), else use RemoteAddr.
func clientIP(r *http.Request) string {
	if strings.EqualFold(os.Getenv("TRUST_PROXY_HEADERS"), "true") {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if comma := strings.IndexByte(xff, ','); comma > 0 {
				return strings.TrimSpace(xff[:comma])
			}
			return strings.TrimSpace(xff)
		}
		if xrip := r.Header.Get("X-Real-IP"); xrip != "" {
			return strings.TrimSpace(xrip)
		}
	}
	addr := r.RemoteAddr
	if i := strings.LastIndexByte(addr, ':'); i > 0 {
		return addr[:i]
	}
	return addr
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// SecretChangeSummary builds a redacted summary for a secret field change:
// "set", "cleared", or "unchanged". Never includes the value.
func SecretChangeSummary(field string, newValue *string) string {
	if newValue == nil {
		return field + " unchanged"
	}
	if strings.TrimSpace(*newValue) == "" {
		return field + " cleared"
	}
	return field + " set"
}

// CategoryAgent covers actions taken by an agent rather than by a person at a
// keyboard — including actions arriving over MCP from outside the workspace.
//
// Re-exported from the model alongside the other five rather than declared here.
// It was originally defined in this file, and being the one category living apart
// from the rest is exactly how the admin UI's filter list came to omit it.
const CategoryAgent = auditModel.CategoryAgent

// Who acted, as opposed to who answers for it.
//
// The actor id names the accountable human, which for an agent is whoever's
// credential it used. That is the right answer to "who is responsible" and it
// reads, on the row, as though the person did the thing themselves. Enterprise
// buyers now ask for a log that tells the two apart, and this is that field.
const (
	// ActorHuman is a person at a keyboard.
	ActorHuman = "human"
	// ActorAgent is an AI agent acting under a person's authority.
	ActorAgent = "agent"
	// ActorSystem is the workspace acting on its own: a sweep, a scheduler, a
	// migration. Distinct from an agent because nobody authorised it for a task.
	ActorSystem = "system"
)

// AllCategories is the canonical category list, for callers that need to present
// every filter rather than record one entry.
func AllCategories() []string { return auditModel.AllCategories() }

// RecordForPrincipal writes an audit entry SYNCHRONOUSLY for an actor identified
// explicitly, rather than read from a request's session.
//
// TWO DIFFERENCES FROM Record, both deliberate.
//
// It takes the actor as a parameter because there is no session to read. An agent
// acting over MCP has no request context carrying a logged-in user; its authority
// comes from a credential, and the human accountable for it is that credential's
// creator. The caller has already resolved that person, and passing it in is what
// keeps this function honest about whose action it is recording.
//
// It is SYNCHRONOUS and returns an error, where Record is fire-and-forget. That
// asymmetry is the point. Record covers admin UI actions, where losing one row to a
// restart is a shame but the action is already visible in the product. An agent
// action recorded nowhere is an action nobody can review — and under logging
// obligations for high-risk AI systems, an unrecorded action is worse than a
// refused one. So the caller can adopt "no record, no action": if this fails, fail
// the call.
//
// metadata must already be redacted; nothing here strips secrets.
func RecordForPrincipal(
	ctx context.Context,
	actorID *uuid.UUID,
	actorEmail, actorKind, action, category, summary string,
	metadata map[string]interface{},
) error {
	// Required rather than inferred. This path has no session to read, so
	// whether a person or an agent acted is knowledge only the caller has, and
	// defaulting it would put a confident value in a compliance record on the
	// strength of a guess.
	if actorKind != ActorHuman && actorKind != ActorAgent && actorKind != ActorSystem {
		return fmt.Errorf("audit: actor kind must be %q, %q or %q, got %q",
			ActorHuman, ActorAgent, ActorSystem, actorKind)
	}
	entry := &auditModel.AuditEntry{
		Action:     action,
		Category:   category,
		Summary:    truncate(summary, 1024),
		ActorID:    actorID,
		ActorEmail: actorEmail,
		ActorKind:  actorKind,
	}
	// What started this, from the context, so every row an agent run writes
	// carries the answer without each call site being told. See initiator.go.
	metadata = stampInitiator(ctx, actorKind, metadata)
	if metadata != nil {
		if b, err := json.Marshal(metadata); err == nil {
			s := string(b)
			entry.Metadata = &s
		}
	}
	return auditModel.Insert(ctx, entry)
}
