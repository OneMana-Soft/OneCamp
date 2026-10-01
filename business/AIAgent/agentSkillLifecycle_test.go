package business

import (
	"encoding/json"
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

// skillIDsOf decides which skills an agent uses. Two callers depend on it and
// must agree: the prompt builder, which decides what the agent is TOLD, and the
// regression notice, which decides which skills to BLAME. A disagreement
// between those two is an agent blamed for a skill it never read.
func TestSkillIDsOf(t *testing.T) {
	a, b := uuid.New(), uuid.New()

	t.Run("parses a normal list", func(t *testing.T) {
		raw, _ := json.Marshal([]string{a.String(), b.String()})
		got := skillIDsOf(&model.AiAgent{SkillIds: string(raw)})
		if len(got) != 2 || got[0] != a || got[1] != b {
			t.Errorf("got %v, want [%s %s] in order", got, a, b)
		}
	})

	t.Run("order is preserved", func(t *testing.T) {
		// Skills are composed into the prompt in the configured order, so this
		// is not incidental: reordering them changes what the agent reads first.
		raw, _ := json.Marshal([]string{b.String(), a.String()})
		got := skillIDsOf(&model.AiAgent{SkillIds: string(raw)})
		if len(got) != 2 || got[0] != b {
			t.Errorf("got %v, want %s first", got, b)
		}
	})

	t.Run("one bad id does not strip the rest", func(t *testing.T) {
		// Losing every skill because one entry is malformed would change an
		// agent's behaviour completely and silently.
		raw, _ := json.Marshal([]string{"not-a-uuid", a.String()})
		got := skillIDsOf(&model.AiAgent{SkillIds: string(raw)})
		if len(got) != 1 || got[0] != a {
			t.Errorf("got %v, want just %s", got, a)
		}
	})

	t.Run("empty shapes yield nothing", func(t *testing.T) {
		for _, raw := range []string{"", "[]", "   ", "not json", "{}"} {
			if got := skillIDsOf(&model.AiAgent{SkillIds: raw}); len(got) != 0 {
				t.Errorf("skillIDsOf(%q) = %v, want none", raw, got)
			}
		}
		if got := skillIDsOf(nil); got != nil {
			t.Errorf("skillIDsOf(nil) = %v, want nil", got)
		}
	})
}
