package ai

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
)

// The limit bounds are stated in four places and must be one number each.
//
// WHY. The database CHECK is the real enforcement. Go restates it so an admin sees a
// sentence instead of a driver error. The FE restates it so the form rejects a value
// without a round trip. Three copies of a range is three chances to disagree, and the
// disagreements are all bad in a specific way: Go looser than SQL turns a readable message
// into a driver error; Go tighter refuses a value the schema allows; FE tighter blocks a
// legal value with no way to override it from the UI.
func TestLimitBoundsAgreeAcrossSchemaGoAndFrontend(t *testing.T) {
	migration := readFileOrSkip(t, "../../migrations/140_add_authorized_model_limits.up.sql")

	// The CHECK constraints, read out of the migration rather than restated here — a copy
	// in this test would be a fourth place to drift.
	winMin, winMax := betweenBounds(t, migration, "ai_authorized_models_context_window_sane")
	outMin, outMax := betweenBounds(t, migration, "ai_authorized_models_max_output_sane")

	if winMin != aiModels.MinModelContextWindow || winMax != aiModels.MaxModelContextWindow {
		t.Errorf("context window: schema says %d-%d, Go says %d-%d",
			winMin, winMax, aiModels.MinModelContextWindow, aiModels.MaxModelContextWindow)
	}
	if outMin != aiModels.MinModelMaxOutput || outMax != aiModels.MaxModelMaxOutput {
		t.Errorf("max output: schema says %d-%d, Go says %d-%d",
			outMin, outMax, aiModels.MinModelMaxOutput, aiModels.MaxModelMaxOutput)
	}

	// The floor must not be below the application's own minimum usable window, or an admin
	// could store a value the budgeter silently clamps — a setting that appears to take and
	// does not.
	if winMin < minContextWindow {
		t.Errorf("schema allows a %d-token window but the budgeter floors at %d, so a stored "+
			"value between them would be silently ignored", winMin, minContextWindow)
	}

	// The frontend's copy, for the same reason.
	fe := readFileOrSkip(t, "../../../onecamp-fe/services/aiModelService.ts")
	for _, c := range []struct {
		field    string
		min, max int
	}{
		{"contextWindow", winMin, winMax},
		{"maxOutput", outMin, outMax},
	} {
		feMin, feMax := feBounds(t, fe, c.field)
		if feMin != c.min || feMax != c.max {
			t.Errorf("frontend %s bounds are %d-%d but the schema says %d-%d",
				c.field, feMin, feMax, c.min, c.max)
		}
	}
}

// A stated window must reach the budget AND the client. Both halves, because they fail
// differently: a wrong budget wastes or overflows the window, while a wrong client runs
// the model at the wrong size where nothing downstream can see it.
func TestStatedWindowReachesBothTheBudgetAndTheClient(t *testing.T) {
	const stated = 131072
	lim := LimitsForModel("ollama", "big", stated, 0)

	if lim.ContextWindow != stated {
		t.Fatalf("window did not survive: %d", lim.ContextWindow)
	}
	// The budget must actually scale with it, not merely record it.
	small := LimitsForModel("ollama", "small", 8192, 0)
	if lim.ContextBudget() <= small.ContextBudget() {
		t.Errorf("a stated 128k window must yield a larger context budget than 8k: %d vs %d",
			lim.ContextBudget(), small.ContextBudget())
	}
	if lim.CompactionTriggerTokens(1024) <= small.CompactionTriggerTokens(1024) {
		t.Error("a stated larger window must compact later")
	}

	// And the client, via the resolver.
	rm, err := newModelResolver().get("k|big", Endpoint{Kind: ProviderOllama, BaseURL: "http://localhost:11434", Model: "big"},
		&AIConfig{ContextWindowTokens: 8192}, func() ModelLimits { return lim })
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	op, ok := rm.llm.(*OllamaProvider)
	if !ok || op.numCtx != stated {
		t.Errorf("client must be built at the stated window %d, got %v", stated, rm.llm)
	}
}

// Editing a model's limits must reload the AI service. The resolver caches clients for the
// life of a service instance and Ollama bakes num_ctx in at construction, so without the
// reload an admin raises a window, sees the new value, and every request keeps running at
// the old one until the next restart.
func TestSettingLimitsReloadsTheService(t *testing.T) {
	src := readFileOrSkip(t, "../../business/AI/authorizedModels.go")
	body := funcBody(t, src, "func SetAuthorizedModelLimits(")
	if !strings.Contains(body, "ReloadAIService") {
		t.Error("business.SetAuthorizedModelLimits must call ai.ReloadAIService — a cached client " +
			"keeps the old num_ctx, so the setting would appear to take effect and not")
	}
}

// --- helpers -----------------------------------------------------------------

func readFileOrSkip(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		// The frontend is a sibling checkout and may be absent in some environments.
		// Skipping is honest; silently passing would not be.
		t.Skipf("cannot read %s (%v)", path, err)
	}
	return string(b)
}

// betweenBounds pulls "BETWEEN <min> AND <max>" out of a named CHECK constraint.
func betweenBounds(t *testing.T, sql, constraint string) (int, int) {
	t.Helper()
	i := strings.Index(sql, constraint)
	if i < 0 {
		t.Fatalf("constraint %q not found in the migration", constraint)
	}
	m := regexp.MustCompile(`BETWEEN\s+(\d+)\s+AND\s+(\d+)`).FindStringSubmatch(sql[i:])
	if m == nil {
		t.Fatalf("no BETWEEN bounds found for %q", constraint)
	}
	lo, _ := strconv.Atoi(m[1])
	hi, _ := strconv.Atoi(m[2])
	return lo, hi
}

// feBounds pulls "field: { min: N, max: M }" out of MODEL_LIMIT_BOUNDS, tolerating the
// numeric separators TypeScript allows (20_000_000).
func feBounds(t *testing.T, ts, field string) (int, int) {
	t.Helper()
	i := strings.Index(ts, "MODEL_LIMIT_BOUNDS")
	if i < 0 {
		t.Fatal("MODEL_LIMIT_BOUNDS not found in the frontend service")
	}
	m := regexp.MustCompile(field + `:\s*\{\s*min:\s*([\d_]+),\s*max:\s*([\d_]+)`).FindStringSubmatch(ts[i:])
	if m == nil {
		t.Fatalf("no bounds found for frontend field %q", field)
	}
	lo, _ := strconv.Atoi(strings.ReplaceAll(m[1], "_", ""))
	hi, _ := strconv.Atoi(strings.ReplaceAll(m[2], "_", ""))
	return lo, hi
}
