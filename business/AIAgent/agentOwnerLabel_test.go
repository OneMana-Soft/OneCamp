package business

import (
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

// Every agent says who authorised it.
//
// The product's whole claim about agents is that one cannot do what the person
// behind it could not. The list showed the tools it may call, its autonomy and
// its budget, and never that person — so an admin could read what an agent was
// allowed to do without learning whose permissions allowed it.
//
// labelOwners is the fill. These cover the shapes it has to survive, none of
// which need a database: an empty list, a name that resolves, one that does
// not, and the same person owning several agents.

func TestLabellingAnEmptyListDoesNothing(t *testing.T) {
	// Called on every list, including the common one with no agents at all.
	labelOwnersFrom(nil, nil)
	labelOwnersFrom([]*model.AiAgent{}, map[uuid.UUID]string{})
}

func TestEachAgentIsLabelledWithItsOwner(t *testing.T) {
	priya, sam := uuid.New(), uuid.New()
	agents := []*model.AiAgent{
		{Id: uuid.New(), Name: "Release Captain", CreatedBy: priya},
		{Id: uuid.New(), Name: "Support Triage", CreatedBy: sam},
	}
	labelOwnersFrom(agents, map[uuid.UUID]string{priya: "Priya N.", sam: "sam@example.test"})

	if agents[0].CreatedByName != "Priya N." {
		t.Errorf("first agent is labelled %q, wanted its owner", agents[0].CreatedByName)
	}
	if agents[1].CreatedByName != "sam@example.test" {
		t.Errorf("an owner with no display name should fall back to the address, got %q",
			agents[1].CreatedByName)
	}
}

// One query for the whole list: the same owner appearing twice must not mean
// two lookups, and must label both rows.
func TestOneOwnerOfSeveralAgentsLabelsThemAll(t *testing.T) {
	priya := uuid.New()
	agents := []*model.AiAgent{
		{CreatedBy: priya}, {CreatedBy: priya}, {CreatedBy: priya},
	}
	labelOwnersFrom(agents, map[uuid.UUID]string{priya: "Priya N."})
	for i, a := range agents {
		if a.CreatedByName != "Priya N." {
			t.Errorf("agent %d was not labelled", i)
		}
	}
}

// A deleted or unknown owner leaves the label empty rather than inventing one,
// so the interface can say "nobody" in its own words instead of rendering a
// uuid at somebody.
func TestAnUnresolvableOwnerIsLeftEmpty(t *testing.T) {
	agents := []*model.AiAgent{{CreatedBy: uuid.New()}}
	labelOwnersFrom(agents, map[uuid.UUID]string{})
	if agents[0].CreatedByName != "" {
		t.Errorf("wanted an empty label for an owner we could not resolve, got %q",
			agents[0].CreatedByName)
	}
}

// The id stays on the row whatever the label says: anything that needs to be
// exact uses it, and a label is only ever for reading.
func TestLabellingNeverDisturbsTheId(t *testing.T) {
	priya := uuid.New()
	agents := []*model.AiAgent{{CreatedBy: priya}}
	labelOwnersFrom(agents, map[uuid.UUID]string{priya: "Priya N."})
	if agents[0].CreatedBy != priya {
		t.Error("the owner id changed while labelling it")
	}
}
