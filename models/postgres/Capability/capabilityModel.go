// Package models (Capability) is the data-access layer for the generic
// capability-permission policies introduced in migration 78. A policy decides
// whether a delegatable capability (create workflows, invite members, …) is
// restricted to admins or open to all members.
package models

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// Policy values (aligned with the migration CHECK).
const (
	PolicyAdminsOnly = "admins_only"
	PolicyAllMembers = "all_members"
)

// Capability keys. Stable strings; referenced by the permission helper and the
// admin Settings UI. Add a new delegatable capability here + a seed row, AND
// register it in AllCapabilities once it is actually enforced end-to-end.
const (
	CapWorkflowManage   = "workflow.manage"
	CapInvitationCreate = "invitation.create"
	// CapAgentManage gates the Agent Builder (create/manage tool-using AI
	// agents). Delegatable like workflows: admins always, members when opened.
	CapAgentManage = "agent.manage"
	// CapAppManage / CapWebhookManage are reserved for future delegation. They
	// are intentionally NOT in AllCapabilities: apps (handler URLs / OAuth /
	// SSRF surface) and outgoing webhooks (event exfiltration to arbitrary
	// URLs) are security-sensitive and stay admin-only until a guarded
	// delegation model exists. Defining them as constants prevents typos if
	// they are wired later.
	CapAppManage     = "app.manage"
	CapWebhookManage = "webhook.manage"
)

// AllCapabilities is the catalog the Settings UI renders AND the only set the
// permission engine will accept/return. A capability appears here ONLY when its
// enforcement is wired end-to-end, so the admin never sees a toggle that does
// nothing. Order is display order.
var AllCapabilities = []string{
	CapWorkflowManage,
	CapInvitationCreate,
	CapAgentManage,
}

// CapabilityPolicy is one policy row.
type CapabilityPolicy struct {
	Capability string     `json:"capability"`
	Policy     string     `json:"policy"`
	UpdatedBy  *uuid.UUID `json:"updated_by,omitempty"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// ValidPolicy reports whether p is a recognized policy value.
func ValidPolicy(p string) bool {
	return p == PolicyAdminsOnly || p == PolicyAllMembers
}

// ValidCapability reports whether c is a known delegatable capability.
func ValidCapability(c string) bool {
	for _, k := range AllCapabilities {
		if k == c {
			return true
		}
	}
	return false
}

// GetPolicy returns the policy for a capability. A missing row resolves to
// admins_only (fail-closed) so an unknown/unseeded capability is never
// accidentally open.
func GetPolicy(ctx context.Context, capability string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	var policy string
	err := postgresInit.DBConn.SqlDB.QueryRowContext(cctx,
		`SELECT policy FROM capability_policies WHERE capability = $1`, capability).Scan(&policy)
	if errors.Is(err, sql.ErrNoRows) {
		return PolicyAdminsOnly, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Capability GetPolicy failed: %+v", err)
		return PolicyAdminsOnly, err
	}
	if !ValidPolicy(policy) {
		return PolicyAdminsOnly, nil
	}
	return policy, nil
}

// ListPolicies returns all policy rows for the admin Settings UI. Capabilities
// without a row are synthesized at the admins_only default so the UI always
// shows the full catalog.
func ListPolicies(ctx context.Context) ([]CapabilityPolicy, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx,
		`SELECT capability, policy, updated_by, updated_at FROM capability_policies`)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Capability ListPolicies failed: %+v", err)
		return nil, err
	}
	defer rows.Close()

	stored := map[string]CapabilityPolicy{}
	for rows.Next() {
		var cp CapabilityPolicy
		var updatedBy uuid.NullUUID
		if serr := rows.Scan(&cp.Capability, &cp.Policy, &updatedBy, &cp.UpdatedAt); serr != nil {
			return nil, serr
		}
		if updatedBy.Valid {
			cp.UpdatedBy = &updatedBy.UUID
		}
		stored[cp.Capability] = cp
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, rerr
	}

	out := make([]CapabilityPolicy, 0, len(AllCapabilities))
	for _, cap := range AllCapabilities {
		if cp, ok := stored[cap]; ok {
			out = append(out, cp)
		} else {
			out = append(out, CapabilityPolicy{Capability: cap, Policy: PolicyAdminsOnly})
		}
	}
	return out, nil
}

// SetPolicy upserts a capability's policy and records the admin who changed it.
func SetPolicy(ctx context.Context, capability, policy string, updatedBy uuid.UUID) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`INSERT INTO capability_policies (capability, policy, updated_by, updated_at)
		 VALUES ($1, $2, $3, NOW())
		 ON CONFLICT (capability) DO UPDATE
		 SET policy = EXCLUDED.policy, updated_by = EXCLUDED.updated_by, updated_at = NOW()`,
		capability, policy, updatedBy)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Capability SetPolicy failed: %+v", err)
		return err
	}
	return nil
}
