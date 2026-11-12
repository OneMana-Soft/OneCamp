package business

import (
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
)

// Saving an agent from a client that sends no autonomy keeps the one it has:
// it used to flip the agent to acting on its own (ADMIN-16).
func TestUpdateAgentWithoutAutonomyKeepsIt(t *testing.T) {
	for _, c := range []struct{ had, sent, want string }{
		{model.AutonomyApproval, "", model.AutonomyApproval},
		{model.AutonomyPlan, "  ", model.AutonomyPlan},
		{model.AutonomyApproval, "bogus", model.AutonomyApproval},
		{model.AutonomyApproval, model.AutonomyAuto, model.AutonomyAuto},
		{model.AutonomyAuto, model.AutonomyPlan, model.AutonomyPlan},
	} {
		if got := keptAutonomy(c.had, c.sent); got != c.want {
			t.Errorf("had %q, sent %q: %q, want %q", c.had, c.sent, got, c.want)
		}
	}
}
