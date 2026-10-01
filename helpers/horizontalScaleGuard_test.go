package helpers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What has to stay outside the process for a second one to be possible.
//
// WHY THIS IS A TEST AND NOT A DOCUMENT. OneCamp runs as a single go-service
// today and the shipped compose declares no replicas, so nothing exercises the
// multi-node path and nothing would notice it closing. The expensive part of
// clustering a chat product is already done here: realtime fan-out is a broker,
// sessions and rate limits are Redis, scheduled work is claimed atomically in
// Postgres. That was designed in, and it is one careless change away from being
// designed out, at which point the option is gone and nobody finds out until a
// customer needs it.
//
// This does NOT claim OneCamp is tested at multiple nodes. It claims the four
// things that would make it impossible have not happened. See docs/Scaling.md.
type externalisedProperty struct {
	name string
	file string
	// needs are the markers that show the shared store is still in the path.
	needs []string
	// why explains what breaks on a second node without it.
	why string
	// aiOnly marks a property that exists only in the AI edition. The AI-free
	// build genuinely has no agent scheduler and no assistant history, so a
	// missing file there is the edition boundary working rather than a
	// regression. Everything NOT marked must be present in both, which is what
	// stops "it is edition-specific" becoming a way to delete a guard.
	aiOnly bool
}

var externalised = []externalisedProperty{
	{
		name:  "realtime fan-out goes through the broker",
		file:  "initializers/mqttInit/connectMqtt.go",
		needs: []string{"AddBroker"},
		why: "A node that fans out from memory only reaches the clients connected to it, " +
			"so half a workspace stops seeing messages the other half sends.",
	},
	{
		name:  "webhook rate limiting is shared, with in-process only as a fallback",
		file:  "business/Webhook/webhookBusiness.go",
		needs: []string{"redisStore.AllowSlidingWindow", "redisStore.IsAvailable"},
		why: "Per-process counters mean the published limit is multiplied by the number " +
			"of nodes, which is a limit that silently stops being one.",
	},
	{
		name:   "a scheduled agent run is claimed, not merely scheduled",
		aiOnly: true,
		file:   "models/postgres/AIAgent/aiAgentModel.go",
		needs:  []string{"ClaimDueScheduledRun"},
		why:    "Without an atomic claim every node fires the same routine, so the work happens N times.",
	},
	{
		name:   "AI conversation history lives in a shared store",
		aiOnly: true,
		file:   "services/AI/aiSessions.go",
		needs:  []string{"redisStore.GetJSON", "redisStore.SetJSON"},
		why: "A conversation held in one process is lost the moment a request lands on " +
			"another, which reads to the person as the assistant forgetting mid-thread.",
	},
}

func TestTheSharedStoresStayShared(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("cannot resolve the repository root: %v", err)
	}

	for _, p := range externalised {
		t.Run(p.name, func(t *testing.T) {
			body, rerr := os.ReadFile(filepath.Join(root, p.file))
			if rerr != nil {
				if p.aiOnly && os.IsNotExist(rerr) {
					t.Skipf("%s is absent, which is correct for the AI-free edition", p.file)
				}
				t.Fatalf("%s is gone; if it moved, move this guard with it: %v", p.file, rerr)
			}
			text := string(body)
			for _, marker := range p.needs {
				if !strings.Contains(text, marker) {
					t.Errorf("%s no longer references %s.\n%s\nIf this was deliberate, "+
						"update docs/Scaling.md too, because it currently tells an operator otherwise.",
						p.file, marker, p.why)
				}
			}
		})
	}
}

// The claim on the sales page and the claim in the docs have to be the same
// claim. Capacity is the one number a buyer plans around, and this repository
// has already shipped three cases of a page saying something the product had
// stopped doing.
func TestScalingDocumentExistsAndIsHonest(t *testing.T) {
	root, _ := filepath.Abs("..")
	body, err := os.ReadFile(filepath.Join(root, "docs", "Scaling.md"))
	if err != nil {
		t.Fatalf("docs/Scaling.md is missing: %v", err)
	}
	text := string(body)

	// The two things an operator must not have to infer.
	for _, required := range []string{"single", "not been load tested"} {
		if !strings.Contains(strings.ToLower(text), required) {
			t.Errorf("docs/Scaling.md does not say %q, which is the part an operator plans around", required)
		}
	}
}
