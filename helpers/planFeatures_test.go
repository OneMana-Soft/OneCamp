package helpers

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// withSeatLimit stamps the binary as the free release (or not) for one test.
func withSeatLimit(t *testing.T, v string) {
	t.Helper()
	old := SeatLimit
	SeatLimit = v
	t.Cleanup(func() { SeatLimit = old })
}

func TestPlanFollowsTheStamp(t *testing.T) {
	withSeatLimit(t, "")
	if OnFreePlan() || !PlanAllows(FeatureSSO) || len(LockedFeatures()) != 0 {
		t.Fatal("an unstamped build (licensed, Cloud or from source) must have every company control")
	}
	withSeatLimit(t, "25")
	if !OnFreePlan() {
		t.Fatal("a stamped build is the free plan")
	}
	for _, f := range CompanyControls {
		if PlanAllows(f) {
			t.Errorf("the free plan must leave out %s", f)
		}
	}
	if got := LockedFeatures(); len(got) != len(CompanyControls) {
		t.Errorf("locked = %v", got)
	}
}

func TestLockedFeaturesSerialisesAsAList(t *testing.T) {
	withSeatLimit(t, "")
	b, _ := json.Marshal(LockedFeatures())
	if string(b) != "[]" {
		t.Errorf("licensed plan locked = %s, want [] (the web app reads a list)", b)
	}
}

func TestPlanRequiredResponse(t *testing.T) {
	rec := httptest.NewRecorder()
	WritePlanRequired(rec, FeatureSCIM)
	if rec.Code != 403 {
		t.Fatalf("status %d", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "plan_required" || body["feature"] != "scim" || body["upgrade_url"] != PlanUpgradeURL {
		t.Errorf("body %v", body)
	}
	msg, _ := body["msg"].(string)
	for _, want := range []string{"SCIM provisioning", "free plan", "onemana.dev/buy", "keeps everything"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
}
