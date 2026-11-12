package business

// What this workspace is on, in one answer for every screen that shows it:
// the Plan page, the seat meter, the limit notes, the locked company
// controls and the plan card. Before it, three screens read GET /admin/seats
// and each typed its own idea of the answer; /admin/seats is now this.
//
// The server knows the plan only from its stamps (helpers.SeatLimit,
// helpers.ReleaseVersion) and whether OneCamp Cloud runs it. It holds no
// licence key or email, so it never claims one.

import (
	"context"
	"strings"

	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
)

// The plan kinds.
const (
	KindFree     = "free"     // the free release: a seat limit, no company controls
	KindLicensed = "licensed" // a release built without the seat stamp
	KindCloud    = "cloud"    // OneCamp Cloud runs it
	KindSource   = "source"   // built from source (AGPL): no stamps, no limit
)

// Plan is GET /admin/plan's answer. used, limit, upgrade_url, free_plan and
// locked are /admin/seats' keys, kept as they were for older web builds.
type Plan struct {
	Plan       string                `json:"plan"`
	Used       int                   `json:"used"`
	Limit      int                   `json:"limit"` // 0 = unlimited
	FreePlan   bool                  `json:"free_plan"`
	Locked     []helpers.PlanFeature `json:"locked"`
	Controls   []Control             `json:"controls"`
	Release    Release               `json:"release"`
	Managed    bool                  `json:"managed"`
	Hosting    string                `json:"hosting"`
	UpgradeURL string                `json:"upgrade_url"`
	Links      Links                 `json:"links"`
}

// Control is one company control and whether this plan includes it.
type Control struct {
	Key      helpers.PlanFeature `json:"key"`
	Included bool                `json:"included"`
}

// Release is the build that is running, "" for both from source.
type Release struct {
	Running string `json:"running"`
	Edition string `json:"edition"`
}

// Links are the ways past the free plan, from the server.
type Links struct {
	Licence string `json:"licence"`
	Cloud   string `json:"cloud"`
	Account string `json:"account"`
	Pricing string `json:"pricing"`
}

// Seams.
var (
	seatUsage   = userDomain.SeatUsage
	isManaged   = settingsBusiness.Managed
	whereHosted = settingsBusiness.Hosting
)

// Current is this workspace's plan. A failed member count is an error, never
// a made-up number.
func Current(ctx context.Context) (Plan, error) {
	used, limit, err := seatUsage(ctx)
	if err != nil {
		return Plan{}, err
	}
	managed := isManaged()
	running := strings.TrimSpace(helpers.ReleaseVersion)
	controls := make([]Control, 0, len(helpers.CompanyControls))
	for _, f := range helpers.CompanyControls {
		controls = append(controls, Control{Key: f, Included: helpers.PlanAllows(f)})
	}
	return Plan{
		Plan:       kindOf(managed, helpers.MemberSeatLimit(), running),
		Used:       used,
		Limit:      limit,
		FreePlan:   helpers.OnFreePlan(),
		Locked:     helpers.LockedFeatures(),
		Controls:   controls,
		Release:    Release{Running: running, Edition: helpers.ReleaseLine(running)},
		Managed:    managed,
		Hosting:    whereHosted(),
		UpgradeURL: helpers.PlanUpgradeURL,
		Links: Links{
			Licence: helpers.PlanLicenceURL,
			Cloud:   helpers.PlanCloudURL,
			Account: helpers.PlanAccountURL,
			Pricing: helpers.PlanPricingURL,
		},
	}, nil
}

// kindOf names the plan from what the server knows. Cloud first: a Cloud
// workspace is never "free" whatever its binary says. Pure.
func kindOf(managed bool, seatLimit int, release string) string {
	switch {
	case managed:
		return KindCloud
	case seatLimit > 0:
		return KindFree
	case strings.TrimSpace(release) != "":
		return KindLicensed
	}
	return KindSource
}
