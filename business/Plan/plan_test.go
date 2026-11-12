package business

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
)

// The plan is a function of three facts the server has: whether OneCamp Cloud
// runs it, the free release's seat stamp, and the release stamp. Cloud first:
// a Cloud workspace is never "free" whatever its binary says.
func TestPlanKindFreeLicensedCloudSource(t *testing.T) {
	for _, c := range []struct {
		name      string
		managed   bool
		seatLimit int
		release   string
		want      string
	}{
		{"Cloud runs it", true, 0, "v2.79.0", "cloud"},
		{"Cloud wins over a free stamp", true, 25, "v2.79.0", "cloud"},
		{"the free release", false, 25, "v2.79.0", "free"},
		{"a free stamp with no release stamp is still free", false, 25, "", "free"},
		{"a release with no seat stamp is licensed", false, 0, "v2.79.0", "licensed"},
		{"no stamps at all is built from source", false, 0, "", "source"},
		{"a blank release stamp is no release", false, 0, "   ", "source"},
	} {
		if got := kindOf(c.managed, c.seatLimit, c.release); got != c.want {
			t.Errorf("%s: kind %q, want %q", c.name, got, c.want)
		}
	}
}

func withPlanSeams(t *testing.T, used, limit int, managed bool) {
	t.Helper()
	oldUsage, oldManaged, oldHosting, oldRelease := seatUsage, isManaged, whereHosted, helpers.ReleaseVersion
	oldSeat := helpers.SeatLimit
	t.Cleanup(func() {
		seatUsage, isManaged, whereHosted, helpers.ReleaseVersion = oldUsage, oldManaged, oldHosting, oldRelease
		helpers.SeatLimit = oldSeat
	})
	seatUsage = func(context.Context) (int, int, error) { return used, limit, nil }
	isManaged = func() bool { return managed }
	whereHosted = func() string { return "self" }
	helpers.ReleaseVersion = "v2.79.0"
	if limit > 0 {
		helpers.SeatLimit = "25"
	} else {
		helpers.SeatLimit = ""
	}
}

// GET /admin/seats now answers with the plan. Every key an older web build
// reads from it is still there, with the same meaning.
func TestSeatsAliasKeepsOldKeys(t *testing.T) {
	withPlanSeams(t, 14, 25, false)
	p, err := Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(p)
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"used", "limit", "upgrade_url", "free_plan", "locked"} {
		if _, ok := got[key]; !ok {
			t.Errorf("the seats answer lost %q: %s", key, raw)
		}
	}
	if got["used"] != float64(14) || got["limit"] != float64(25) || got["free_plan"] != true ||
		got["upgrade_url"] != helpers.PlanUpgradeURL {
		t.Errorf("the old keys changed meaning: %s", raw)
	}
	if locked, _ := got["locked"].([]any); len(locked) != len(helpers.CompanyControls) {
		t.Errorf("the free plan locks every company control: %s", raw)
	}
}

func TestPlanNamesWhatItIsAndWhatsIncluded(t *testing.T) {
	withPlanSeams(t, 14, 25, false)
	p, err := Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.Plan != "free" || p.Hosting != "self" || p.Managed {
		t.Fatalf("a free self-hosted workspace: %+v", p)
	}
	if p.Release.Running != "v2.79.0" || p.Release.Edition != "v2" {
		t.Errorf("the running release rides along, so nobody has to press Check: %+v", p.Release)
	}
	if len(p.Controls) != len(helpers.CompanyControls) {
		t.Fatalf("one row per company control: %+v", p.Controls)
	}
	for i, c := range p.Controls {
		if c.Key != helpers.CompanyControls[i] || c.Included {
			t.Errorf("free: %s must be listed in order and not included: %+v", helpers.CompanyControls[i], c)
		}
	}
	if p.Links.Licence == "" || p.Links.Cloud == "" || p.Links.Account == "" || p.Links.Pricing == "" {
		t.Errorf("every way out comes from the server, so the web names no host: %+v", p.Links)
	}

	withPlanSeams(t, 140, 0, false)
	p, _ = Current(context.Background())
	if p.Plan != "licensed" || p.FreePlan || len(p.Locked) != 0 {
		t.Fatalf("a licensed workspace locks nothing: %+v", p)
	}
	for _, c := range p.Controls {
		if !c.Included {
			t.Errorf("licensed: %s is included: %+v", c.Key, c)
		}
	}
}

// A failed count is the handler's error to answer, never a made-up number.
func TestPlanSaysSoWhenItCannotCount(t *testing.T) {
	withPlanSeams(t, 0, 25, false)
	seatUsage = func(context.Context) (int, int, error) { return 0, 25, errors.New("down") }
	if _, err := Current(context.Background()); err == nil {
		t.Fatal("a failed count must be an error")
	}
}
