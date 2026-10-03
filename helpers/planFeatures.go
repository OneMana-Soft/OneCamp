package helpers

import (
	"fmt"
	"net/http"
)

// Company controls: what the free plan leaves out.
//
// The free release (SeatLimit stamped into the binary, see seatLimit.go) is for
// small teams, and it keeps everything a team uses: chat, docs, tasks, calls,
// AI agents and their governance, two-factor sign-in, the audit log itself and
// its chain verification. What it leaves out is what a company's IT and
// compliance functions ask for: single sign-on through their identity provider,
// LDAP, SCIM provisioning, and exporting the audit log as evidence.
//
// WHY THESE. Until 3 Oct 2026 the free plan had every feature, so a team under
// 25 never had a reason to pay, and the paid licence only mattered to companies
// large enough to be wary of a new vendor. These four are the controls that only
// a company needs, which makes the price match the headline the site sells:
// governance is what you buy. Builds from source (no stamp) keep everything, as
// the AGPL promises; this is the difference between the free official release
// and the licensed one, nothing else.
//
// Safety is never gated: approvals, the agent kill switch, refusals and the
// record of them work on every plan.

// PlanFeature names one company control.
type PlanFeature string

const (
	FeatureSSO         PlanFeature = "sso"          // SAML 2.0 and OpenID Connect sign-in
	FeatureLDAP        PlanFeature = "ldap"         // LDAP / Active Directory sign-in
	FeatureSCIM        PlanFeature = "scim"         // SCIM 2.0 provisioning and its tokens
	FeatureAuditExport PlanFeature = "audit_export" // audit-log export and the evidence pack
)

// CompanyControls lists every gated feature, in the order the product names them.
var CompanyControls = []PlanFeature{FeatureSSO, FeatureLDAP, FeatureSCIM, FeatureAuditExport}

// PlanUpgradeURL is where a refusal sends an admin.
const PlanUpgradeURL = "https://onemana.dev/buy"

// OnFreePlan reports whether this binary is the free release.
func OnFreePlan() bool { return MemberSeatLimit() > 0 }

// PlanAllows reports whether this workspace's plan includes f.
func PlanAllows(f PlanFeature) bool { return !OnFreePlan() }

// LockedFeatures is what this plan leaves out: every company control on the
// free plan, none otherwise. Never nil, so it serialises as [].
func LockedFeatures() []PlanFeature {
	if OnFreePlan() {
		return append([]PlanFeature(nil), CompanyControls...)
	}
	return []PlanFeature{}
}

func (f PlanFeature) label() string {
	switch f {
	case FeatureSSO:
		return "Single sign-on (SAML and OpenID Connect)"
	case FeatureLDAP:
		return "LDAP sign-in"
	case FeatureSCIM:
		return "SCIM provisioning"
	case FeatureAuditExport:
		return "Exporting the audit log"
	}
	return string(f)
}

// PlanRequiredMessage says what is missing and how to get it, in words an admin
// can act on.
func PlanRequiredMessage(f PlanFeature) string {
	return fmt.Sprintf("%s needs a OneCamp licence. This workspace is on the free plan, which includes "+
		"everything a team uses but not company controls: single sign-on, LDAP, SCIM and audit export. "+
		"An admin can add a licence at onemana.dev/buy; the workspace keeps everything it has.", f.label())
}

// WritePlanRequired answers a request for a feature the plan leaves out: 403,
// with a code the web app recognises and the place to upgrade.
func WritePlanRequired(w http.ResponseWriter, f PlanFeature) {
	WriteJSON(w, http.StatusForbidden, Envolope{
		"msg":         PlanRequiredMessage(f),
		"status":      "failed",
		"code":        "plan_required",
		"feature":     f,
		"upgrade_url": PlanUpgradeURL,
	})
}
