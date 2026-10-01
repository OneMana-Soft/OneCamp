package business

import (
	"testing"

	model "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	"github.com/google/uuid"
)

func TestValidateSkill(t *testing.T) {
	if _, _, err := validateSkill(&SkillInput{Name: "", Instructions: "x"}); err == nil {
		t.Fatal("empty name must fail")
	}
	if _, _, err := validateSkill(&SkillInput{Name: "x", Instructions: "  "}); err == nil {
		t.Fatal("empty instructions must fail")
	}
	name, body, err := validateSkill(&SkillInput{Name: "  Status update  ", Instructions: "  Be concise.  "})
	if err != nil || name != "Status update" || body != "Be concise." {
		t.Fatalf("expected trimmed values, got (%q,%q,%v)", name, body, err)
	}
}

func TestSortByConfiguredOrder(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	// Configured order: a(0), b(1), c(2). Provide them shuffled.
	skills := []*model.AgentSkill{{Id: c}, {Id: a}, {Id: b}}
	order := map[uuid.UUID]int{a: 0, b: 1, c: 2}
	sortByConfiguredOrder(skills, order)
	if skills[0].Id != a || skills[1].Id != b || skills[2].Id != c {
		t.Fatalf("expected a,b,c order, got %v,%v,%v", skills[0].Id, skills[1].Id, skills[2].Id)
	}
}
