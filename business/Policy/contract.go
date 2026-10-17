package business

// The Policy Registry: typed workspace policies, one middleware that enforces
// them, and every change recorded (docs programme, PROGRAM.md §4 #15).
//
// A policy's value lives in system_configs under the policy's key; a policy
// nobody set answers its default, and each default is what the server did before
// policies existed: members may export and publish, guest links carry no cap of
// their own, and no embed list is configured. Admins read and set them
// (Admin.List, Admin.Set; the /admin/policies routes), and every change is a row
// in the audit log, or it doesn't happen.
//
// A route behind a policy:
//
//	r.With(policyBusiness.Policies.Require(policyBusiness.DocsExportMembers)).Post("/export", ...)
//
// Values are read through a short cache, so a change reaches every instance
// within cacheFor; a read that fails answers the default, today's behaviour.

import (
	"context"
	"net/http"

	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// BoolPolicy is an on/off policy and its default.
type BoolPolicy struct {
	Key     string
	Default bool
}

// IntPolicy is a numeric policy and its default.
type IntPolicy struct {
	Key     string
	Default int
}

// ListPolicy is a list policy and its default.
type ListPolicy struct {
	Key     string
	Default []string
}

var (
	// DocsExportMembers lets members export docs; admins always may.
	DocsExportMembers = BoolPolicy{Key: "docs.export.members", Default: true}
	// DocsPublishMembers lets members publish docs to the web; admins always may.
	DocsPublishMembers = BoolPolicy{Key: "docs.publish.members", Default: true}
	// GuestLinkMaxDays caps how long a guest link lasts; 0 sets no cap.
	GuestLinkMaxDays = IntPolicy{Key: "guest.link.max_days", Default: 0}
	// EmbedAllowlist lists the hosts docs may embed; nil means no list is
	// configured.
	EmbedAllowlist = ListPolicy{Key: "embed.allowlist"}
)

// Store answers policies.
type Store interface {
	Bool(ctx context.Context, p BoolPolicy) bool
	Int(ctx context.Context, p IntPolicy) int
	List(ctx context.Context, p ListPolicy) []string
	// Require refuses a request when p is off for the signed-in member. Admins
	// always pass.
	Require(p BoolPolicy) func(http.Handler) http.Handler
}

// Policies is where every policy is read: the registry.
var Policies Store = Admin

// isAdmin is whether the signed-in member is a workspace admin, as
// VerifyAuth put them on the request.
func isAdmin(r *http.Request) bool {
	u, ok := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	return ok && u.UserPostgresInfo.IsAdmin
}
